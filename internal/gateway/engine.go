package gateway

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	g "tiktok-chat-bot/internal/greeter"
	"time"
)

type Sticker struct {
	Name    string `json:"name,omitempty"`
	ID      string `json:"id"`
	Preview string `json:"preview"`
}
type API interface {
	Check(context.Context) error
	Events(context.Context, time.Time) ([]g.Event, error)
	Profile(context.Context, string) (g.Profile, error)
	Prepare(context.Context, string) (g.Conversation, error)
	ReadyStickerID(context.Context, string) error
	Send(context.Context, g.Conversation, string, string) (string, error)
	Verify(context.Context, Operation) (bool, error)
	Stickers(context.Context) ([]Sticker, error)
	Preview(context.Context, string) ([]byte, string, error)
}
type Engine struct {
	Store            *Store
	API              API
	gate             sync.Mutex // pause/resume and durable dispatch boundary
	work             sync.Mutex // one send or reconciliation at a time
	statusMu         sync.RWMutex
	connected        bool
	pollError        string
	VerifyDelay      time.Duration
	incomingMu       sync.Mutex
	messagePollError string
}

func (e *Engine) Connection() (bool, string) {
	e.statusMu.RLock()
	defer e.statusMu.RUnlock()
	return e.connected, e.pollError
}
func (e *Engine) setConnection(ok bool, reason string) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.connected = ok
	e.pollError = reason
}
func (e *Engine) Check(ctx context.Context) error {
	err := e.API.Check(ctx)
	e.setConnection(err == nil, "")
	return err
}
func (e *Engine) Pause() error {
	e.gate.Lock()
	defer e.gate.Unlock()
	return e.Store.Pause("manual_pause")
}
func (e *Engine) Resume(ctx context.Context) error {
	e.work.Lock()
	defer e.work.Unlock()
	e.gate.Lock()
	defer e.gate.Unlock()
	counts, err := e.Store.Counts()
	if err != nil {
		return err
	}
	if counts["uncertain"]+counts["sending"]+counts["verifying"] > 0 {
		return errors.New("uncertain_operations_reconcile_required")
	}
	m, err := e.Store.Meta()
	if err != nil {
		return err
	}
	if m.Started.IsZero() {
		return errors.New("baseline_not_ready")
	}
	if err = e.Check(ctx); err != nil {
		return errors.New("session_check_failed")
	}
	return e.Store.Pause("")
}
func (e *Engine) Poll(ctx context.Context) error {
	if err := e.Check(ctx); err != nil {
		e.setConnection(false, "session_check_failed")
		return nil
	}
	m, err := e.Store.Meta()
	if err != nil {
		return err
	}
	after := m.LastPoll.Add(-2 * time.Minute)
	if m.Started.IsZero() {
		after = time.Time{}
	} else if after.Before(m.Started) {
		after = m.Started
	}
	at := time.Now().UTC()
	events, err := e.API.Events(ctx, after)
	if err != nil {
		e.setConnection(false, "follower_poll_failed")
		return nil
	}
	if err = e.Store.Ingest(events, at); err != nil {
		return err
	}
	e.setConnection(true, "")
	return nil
}
func (e *Engine) finish(o Operation, state, reason, pause string) error {
	o.State = state
	o.Reason = reason
	return e.Store.Save(o, pause)
}
func rejectionCodes(err error) *TikTokRejection {
	var rejection *g.APIRejection
	if errors.As(err, &rejection) {
		return &TikTokRejection{Status: rejection.Status, Check: rejection.Check, BusinessCode: rejection.BusinessCode}
	}
	return nil
}

func (e *Engine) Work(ctx context.Context) error {
	e.work.Lock()
	defer e.work.Unlock()
	m, err := e.Store.Meta()
	if err != nil {
		return err
	}
	if m.Paused != "" {
		return nil
	}
	o, err := e.Store.Next()
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = e.Check(ctx); err != nil {
		return e.Store.Pause("session_check_failed")
	}
	p, err := e.API.Profile(ctx, o.Command.RecipientUserID)
	if err != nil {
		return e.Store.Pause("profile_check_failed")
	}
	// Following and previous message text do not restrict a command.
	if p.Blocked || p.Deleted {
		return e.finish(o, "failed", "recipient_unavailable", "")
	}
	o.Conversation, err = e.API.Prepare(ctx, o.Command.RecipientUserID)
	if err != nil {
		o.TikTokRejection = rejectionCodes(err)
		if errors.Is(err, g.ErrRecipientUnavailable) {
			return e.finish(o, "failed", "recipient_unavailable", "")
		}
		if errors.Is(err, g.ErrDenied) {
			return e.finish(o, "failed", "tiktok_rejected_conversation", "tiktok_restriction")
		}
		return e.Store.Pause("conversation_preflight_failed")
	}
	if o.Command.Type == "sticker" {
		if err = e.API.ReadyStickerID(ctx, o.Command.StickerID); err != nil {
			return e.finish(o, "failed", "sticker_unavailable", "")
		}
	}
	e.gate.Lock()
	m, err = e.Store.Meta()
	if err != nil || m.Paused != "" || ctx.Err() != nil {
		e.gate.Unlock()
		return err
	}
	o.State = "sending"
	err = e.Store.Save(o, "")
	e.gate.Unlock()
	if err != nil {
		return err
	}
	id, sendErr := e.API.Send(ctx, o.Conversation, o.Command.Type, o.Command.Text)
	if sendErr != nil || id == "" {
		o.TikTokRejection = rejectionCodes(sendErr)
		// On 2026-10-14 this exact rejection affected a sticker after a
		// delivered text; subsequent text sends also succeeded. Its undocumented
		// meaning does not justify classifying the recipient as unavailable.
		// Isolate only the observed sticker rejection, never retry it, and keep
		// the normal pause policy for text, preflight, and all other codes.
		if o.Command.Type == "sticker" && errors.Is(sendErr, g.ErrDenied) &&
			o.TikTokRejection != nil && o.TikTokRejection.Status == 3 &&
			o.TikTokRejection.Check == 2 && o.TikTokRejection.BusinessCode == 7175 {
			return e.finish(o, "failed", "tiktok_rejected_sticker", "")
		}
		if errors.Is(sendErr, g.ErrRecipientUnavailable) {
			return e.finish(o, "failed", "recipient_unavailable", "")
		}
		if errors.Is(sendErr, g.ErrDenied) {
			return e.finish(o, "failed", "tiktok_rejected_send", "tiktok_restriction")
		}
		return e.finish(o, "uncertain", "send_result_unknown", "uncertain_operations")
	}
	o.MessageID = id
	o.State = "verifying"
	if err = e.Store.Save(o, ""); err != nil {
		return err
	}
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return e.finish(o, "uncertain", "verification_interrupted", "uncertain_operations")
			case <-time.After(e.VerifyDelay):
			}
		}
		ok, err := e.API.Verify(ctx, o)
		if err == nil && ok {
			return e.finish(o, "succeeded", "", "")
		}
	}
	return e.finish(o, "uncertain", "history_not_confirmed", "uncertain_operations")
}
func (e *Engine) Reconcile(ctx context.Context, id string) (Operation, error) {
	e.work.Lock()
	defer e.work.Unlock()
	o, err := e.Store.Get(id)
	if err != nil {
		return o, err
	}
	if o.State != "uncertain" {
		return o, errors.New("only_uncertain_can_reconcile")
	}
	// With no acknowledged server ID there is no safe proof. Never guess by text.
	if o.MessageID == "" {
		o.Reason = "missing_server_id_cannot_prove_delivery"
	} else if err = e.Check(ctx); err != nil {
		o.Reason = "session_check_failed"
	} else if ok, err := e.API.Verify(ctx, o); err == nil && ok {
		o.State = "succeeded"
		o.Reason = ""
	} else {
		o.Reason = "history_not_confirmed"
	}
	if err = e.Store.Save(o, ""); err != nil {
		return o, err
	}
	return e.Store.Get(id)
}
