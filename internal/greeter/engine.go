package greeter

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"
)

type Profile struct{ Follows, Blocked, Deleted bool }
type Message struct{ ID, Sender, Text, Kind string }

var ErrRecipientUnavailable = errors.New("recipient messaging unavailable")
var ErrDenied = errors.New("messaging denied")
var ErrIncomplete = errors.New("follower history incomplete")

type API interface {
	Events(context.Context, time.Time) ([]Event, error)
	Profile(context.Context, string) (Profile, error)
	Prepare(context.Context, string) (Conversation, error)
	History(context.Context, Conversation) ([]Message, error)
	Send(context.Context, Conversation, string, string) (string, error)
	ReadySticker(context.Context) error
}
type Engine struct {
	Settings *Settings
	State    *State
	API      API
	Save     func(*State) error
	Now      func() time.Time
	Delay    func() (time.Duration, error)
	Log      func(string)
}

func RandomDelay() (time.Duration, error) {
	n, e := rand.Int(rand.Reader, big.NewInt(241))
	if e != nil {
		return 0, e
	}
	return time.Duration(60+n.Int64()) * time.Second, nil
}
func (e *Engine) commit() error { e.State.Heartbeat = e.Now().UTC(); return e.Save(e.State) }
func (e *Engine) pause(reason string) error {
	e.State.Paused = reason
	e.Log(reason)
	return e.commit()
}
func (e *Engine) change(j *Job, phase, reason string) error {
	j.Phase = phase
	j.Reason = reason
	j.Updated = e.Now().UTC()
	e.Log("job_" + phase)
	return e.commit()
}
func (e *Engine) Ingest(events []Event, at time.Time) error {
	// Oldest first: a refollow must never move an existing job's deadline.
	sort.Slice(events, func(i, j int) bool { return events[i].Created < events[j].Created })
	for _, v := range events {
		if e.State.Seen[v.ID] {
			continue
		}
		e.State.Seen[v.ID] = true
		if v.Created <= e.State.Started.Unix() || v.Created > at.Add(5*time.Second).Unix() || at.Sub(time.Unix(v.Created, 0)) > time.Hour {
			continue
		}
		if e.State.Jobs[v.UserID] != nil || v.UserID == e.State.SelfID {
			continue
		}
		d, err := e.Delay()
		if err != nil {
			return err
		}
		scenario := DefaultSettings(45)
		if e.Settings != nil {
			scenario = *e.Settings
			scenario.Blocks = append([]Block(nil), e.Settings.Blocks...)
		}
		step := 0
		if scenario.Blocks != nil {
			d = 0
			step = 1
		}
		e.State.Jobs[v.UserID] = &Job{Step: step, Settings: &scenario, Event: v, Detected: at, Due: at.Add(d), Phase: "queued", Updated: at}
		e.Log("new_follower_queued")
	}
	e.State.LastPoll = at
	return e.commit()
}
func (e *Engine) Poll(ctx context.Context) error {
	at := e.Now().UTC()
	after := e.State.LastPoll.Add(-2 * time.Minute)
	if after.Before(e.State.Started) {
		after = e.State.Started
	}
	events, err := e.API.Events(ctx, after)
	if err != nil {
		return err
	}
	return e.Ingest(events, at)
}
func (e *Engine) Recover() error {
	for _, j := range e.State.Jobs {
		if j.Phase == "text_attempted" || j.Phase == "sticker_attempted" {
			if j.Reason != "tiktok_rejected_send" {
				j.Reason = "interrupted_send_no_automatic_retry"
				e.State.Paused = "uncertain_send_inspect_history"
			} else {
				e.State.Paused = "tiktok_rejected_send"
			}
		}
	}
	return e.commit()
}
func (e *Engine) Work(ctx context.Context) error {
	if e.State.Paused != "" {
		return nil
	}
	jobs := make([]*Job, 0, len(e.State.Jobs))
	for _, j := range e.State.Jobs {
		jobs = append(jobs, j)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Due.Before(jobs[j].Due) })
	for _, j := range jobs {
		if j.Phase != "queued" && j.Phase != "text_verified" {
			continue
		}
		if j.Settings != nil && j.Settings.Blocks != nil {
			if e.Now().Before(j.Due) {
				continue
			}
			return e.workFlow(ctx, j)
		}
		if e.Now().Sub(j.Detected) > time.Hour {
			if err := e.change(j, "skipped", "stale_after_downtime"); err != nil {
				return err
			}
			continue
		}
		// Eligibility is checked before starting the wait, and again just before sending.
		if !j.Prechecked || !e.Now().Before(j.Due) {
			p, err := e.API.Profile(ctx, j.Event.UserID)
			if err != nil {
				return e.pause("profile_check_failed")
			}
			if !p.Follows || p.Blocked || p.Deleted {
				if err := e.change(j, "skipped", "not_following_blocked_or_deleted"); err != nil {
					return err
				}
				continue
			}
			j.Prechecked = true
			if err := e.commit(); err != nil {
				return err
			}
		}
		if e.Now().Before(j.Due) {
			continue
		}
		conv, err := e.API.Prepare(ctx, j.Event.UserID)
		if err != nil {
			if errors.Is(err, ErrRecipientUnavailable) {
				if err = e.change(j, "skipped", "recipient_unavailable"); err != nil {
					return err
				}
				continue
			}
			if errors.Is(err, ErrDenied) {
				if err = e.change(j, "skipped", "conversation_denied"); err != nil {
					return err
				}
				return e.pause("tiktok_rejected_conversation")
			}
			return e.pause("conversation_preflight_failed")
		}
		j.Conversation = conv
		history, err := e.API.History(ctx, conv)
		if err != nil {
			return e.pause("history_preflight_failed")
		}
		if j.Phase == "queued" {
			for _, m := range history {
				if m.Sender == e.State.SelfID && m.Text == j.Scenario().Text {
					return e.change(j, "skipped", "greeting_already_exists")
				}
			}
		}
		if j.Scenario().SendSticker {
			if err = e.API.ReadySticker(ctx); err != nil {
				return e.pause("sticker_source_unavailable")
			}
		}
		if j.Phase == "queued" {
			if err = e.send(ctx, j, "text"); err != nil {
				return err
			}
			if j.Phase != "text_verified" {
				return nil
			}
		}
		if e.State.Paused != "" {
			return nil
		}
		if !j.Scenario().SendSticker {
			return e.change(j, "sent", "")
		}
		if err = e.send(ctx, j, "sticker"); err != nil {
			return err
		}
		// One recipient per tick: serial sends, no catch-up burst.
		return nil
	}
	return nil
}
func (e *Engine) send(ctx context.Context, j *Job, kind string) error {
	if err := e.change(j, kind+"_attempted", ""); err != nil {
		return err
	}
	id, err := e.API.Send(ctx, j.Conversation, kind, j.MessageText())
	if err != nil || id == "" {
		var rejection *APIRejection
		if errors.As(err, &rejection) {
			j.Rejection = &Rejection{Status: rejection.Status, Check: rejection.Check}
		}

		if errors.Is(err, ErrRecipientUnavailable) {
			phase := "skipped"
			if kind == "sticker" && j.TextID != "" {
				phase = "text_only"
			}
			return e.change(j, phase, "recipient_unavailable")
		}
		reason := "uncertain_send_inspect_history"
		if errors.Is(err, ErrDenied) {
			reason = "tiktok_rejected_send"
		}
		j.Reason = reason
		return e.pause(reason)
	}
	if kind == "text" {
		j.TextID = id
	} else {
		j.StickerID = id
	}
	if err = e.commit(); err != nil {
		return err
	}
	// Verify by exact server ID, sender and content, never by a guessed ACK alone.
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return e.pause("verification_interrupted")
			case <-time.After(2 * time.Second):
			}
		}
		messages, err := e.API.History(ctx, j.Conversation)
		if err != nil {
			continue
		}
		for _, m := range messages {
			if m.ID == id && m.Sender == e.State.SelfID && ((kind == "text" && m.Text == j.MessageText()) || (kind == "sticker" && m.Kind == "sticker")) {
				if j.Settings != nil && j.Settings.Blocks != nil {
					j.Results = append(j.Results, StepResult{j.Step, j.Settings.Blocks[j.Step].Type, e.Now().UTC()})
					j.Step++
					j.Due = e.Now().UTC()
					phase := "queued"
					if j.Step == len(j.Settings.Blocks) {
						phase = "sent"
					}
					return e.change(j, phase, "")
				}
				phase := "text_verified"
				if kind == "sticker" {
					phase = "sent"
				}
				return e.change(j, phase, "")
			}
		}
	}
	return e.pause(fmt.Sprintf("%s_not_verified_no_retry", kind))
}
