package gateway

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

type IncomingStatus struct {
	StartedAt  time.Time `json:"startedAt"`
	LastPollAt time.Time `json:"lastPollAt"`
	Error      string    `json:"error,omitempty"`
}

func (s *Store) incomingTimes() (time.Time, time.Time, error) {
	var start, last string
	err := s.db.QueryRow(`SELECT started,last_poll FROM message_sync WHERE id=1`).Scan(&start, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	a, err := time.Parse(time.RFC3339Nano, start)
	if err != nil {
		return a, time.Time{}, err
	}
	b, err := time.Parse(time.RFC3339Nano, last)
	return a, b, err
}
func (s *Store) ingestMessages(events []Event, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, event := range events {
		body, err := json.Marshal(event)
		if err != nil {
			return err
		}
		result, err := tx.Exec(`INSERT OR IGNORE INTO events(id,body) VALUES(?,?)`, event.EventID, body)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n > 0 {
			if _, err = tx.Exec(`INSERT INTO outbox(id) VALUES(?)`, event.EventID); err != nil {
				return err
			}
		}
	}
	stamp := at.Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO message_sync VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET last_poll=excluded.last_poll`, stamp, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) incomingError(reason string) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.messagePollError = reason
}
func (e *Engine) IncomingStatus() (IncomingStatus, error) {
	start, last, err := e.Store.incomingTimes()
	e.statusMu.RLock()
	defer e.statusMu.RUnlock()
	return IncomingStatus{start, last, e.messagePollError}, err
}

// PollMessages persists only a synchronization watermark and event deduplication.
// Initial enablement creates a baseline; it never replays old conversations.
func (e *Engine) PollMessages(ctx context.Context) error {
	e.incomingMu.Lock()
	defer e.incomingMu.Unlock()
	api, ok := e.API.(MessagingAPI)
	if !ok {
		e.incomingError("messages_not_supported")
		return nil
	}
	start, last, err := e.Store.incomingTimes()
	if err != nil {
		return err
	}
	at := time.Now().UTC()
	conversations, err := api.Conversations(ctx)
	if err != nil {
		e.incomingError("inbox_poll_failed")
		return nil
	}
	if start.IsZero() {
		e.incomingError("")
		return e.Store.ingestMessages(nil, at)
	}
	after := last.Add(-2 * time.Minute)
	if after.Before(start) {
		after = start
	}
	meta, err := e.Store.Meta()
	if err != nil {
		return err
	}
	events := []Event{}
	for _, conv := range conversations {
		cursor := ""
		finished := false
		for pageNum := 0; pageNum < 100; pageNum++ {
			page, err := api.Messages(ctx, conv.UserID, cursor)
			if err != nil || page.ConversationID != conv.ID || page.UserID != conv.UserID || page.AccountID != meta.AccountID {
				e.incomingError("message_history_failed")
				return nil
			}
			crossed := false
			for _, msg := range page.Messages {
				if !msg.SentAt.After(after) {
					crossed = true
				}
				if msg.Direction != "incoming" || !msg.SentAt.After(after) || msg.SentAt.After(at) {
					continue
				}
				id := hash([]byte(meta.AccountID + ":message:" + conv.ID + ":" + msg.ID))
				copy := msg
				events = append(events, Event{SchemaVersion: "1", EventID: id, Type: "message.received", AccountID: meta.AccountID, OccurredAt: msg.SentAt, DetectedAt: at, Data: EventData{UserID: conv.UserID, ConversationID: conv.ID, MessageID: msg.ID, Message: &copy}})
			}
			if crossed || page.Exhausted {
				finished = true
				break
			}
			next, nerr := strconv.ParseUint(page.NextCursor, 10, 64)
			previous, _ := strconv.ParseUint(cursor, 10, 64)
			if nerr != nil || next == 0 || (cursor != "" && next >= previous) {
				break
			}
			cursor = page.NextCursor
		}
		if !finished {
			e.incomingError("message_history_incomplete")
			return nil
		}
	}
	if err = e.Store.ingestMessages(events, at); err != nil {
		return err
	}
	e.incomingError("")
	return nil
}
