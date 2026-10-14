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
	StartedAt           time.Time                     `json:"startedAt"`
	LastPollAt          time.Time                     `json:"lastPollAt"`
	Error               string                        `json:"error,omitempty"`
	FailedConversations []IncomingConversationFailure `json:"failedConversations,omitempty"`
}

type IncomingConversationFailure struct {
	ConversationID string    `json:"conversationId"`
	UserID         string    `json:"userId"`
	Error          string    `json:"error"`
	CheckpointAt   time.Time `json:"checkpointAt"`
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
	if err := insertMessageEvents(tx, events); err != nil {
		return err
	}
	stamp := at.Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO message_sync VALUES(1,?,?) ON CONFLICT(id) DO UPDATE SET last_poll=excluded.last_poll`, stamp, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func insertMessageEvents(tx *sql.Tx, events []Event) error {
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
	return nil
}

// Seed each conversation before polling it: a failed first attempt must retain
// its old watermark even when other conversations advance the global poll time.
func (s *Store) conversationCheckpoint(conv DirectConversation, fallback time.Time) (time.Time, error) {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO message_conversation_sync(id,user_id,last_poll) VALUES(?,?,?)`, conv.ID, conv.UserID, fallback.Format(time.RFC3339Nano))
	if err != nil {
		return time.Time{}, err
	}
	var stamp, user string
	if err = s.db.QueryRow(`SELECT last_poll,user_id FROM message_conversation_sync WHERE id=?`, conv.ID).Scan(&stamp, &user); err != nil {
		return time.Time{}, err
	}
	if user != conv.UserID {
		return time.Time{}, errors.New("conversation_identity_changed")
	}
	return time.Parse(time.RFC3339Nano, stamp)
}
func (s *Store) finishConversation(conv DirectConversation, events []Event, at time.Time, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := at.Format(time.RFC3339Nano)
	if reason == "" {
		if err = insertMessageEvents(tx, events); err != nil {
			return err
		}
		_, err = tx.Exec(`UPDATE message_conversation_sync SET last_poll=?,last_attempt=?,error='' WHERE id=?`, stamp, stamp, conv.ID)
	} else {
		_, err = tx.Exec(`UPDATE message_conversation_sync SET last_attempt=?,error=? WHERE id=?`, stamp, reason, conv.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) incomingFailures(at time.Time) ([]IncomingConversationFailure, error) {
	rows, err := s.db.Query(`SELECT id,user_id,error,last_poll FROM message_conversation_sync WHERE error<>'' AND last_attempt=? ORDER BY id`, at.Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var failures []IncomingConversationFailure
	for rows.Next() {
		var f IncomingConversationFailure
		var stamp string
		if err := rows.Scan(&f.ConversationID, &f.UserID, &f.Error, &stamp); err != nil {
			return nil, err
		}
		f.CheckpointAt, err = time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, err
		}
		failures = append(failures, f)
	}
	return failures, rows.Err()
}

func (e *Engine) incomingError(reason string) {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	e.messagePollError = reason
}
func (e *Engine) IncomingStatus() (IncomingStatus, error) {
	start, last, err := e.Store.incomingTimes()
	if err != nil {
		return IncomingStatus{}, err
	}
	failures, err := e.Store.incomingFailures(last)
	e.statusMu.RLock()
	reason := e.messagePollError
	e.statusMu.RUnlock()
	if reason == "" && len(failures) > 0 {
		reason = "message_history_partial"
	}
	return IncomingStatus{StartedAt: start, LastPollAt: last, Error: reason, FailedConversations: failures}, err
}

// PollMessages keeps transport checkpoints per conversation, never dialog stages.
// Only a complete successful read commits that conversation's events and cursor.
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
	meta, err := e.Store.Meta()
	if err != nil {
		return err
	}
	failed := false
	for _, conv := range conversations {
		if ctx.Err() != nil {
			return nil
		}
		checkpoint, err := e.Store.conversationCheckpoint(conv, last)
		if err != nil {
			return err
		}
		after := checkpoint.Add(-2 * time.Minute)
		if after.Before(start) {
			after = start
		}
		// A slow or looping conversation cannot monopolize the polling worker.
		conversationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		events, reason := pollConversation(conversationCtx, api, meta.AccountID, conv, after, at)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err = e.Store.finishConversation(conv, events, at, reason); err != nil {
			return err
		}
		failed = failed || reason != ""
	}
	if err = e.Store.ingestMessages(nil, at); err != nil {
		return err
	}
	if failed {
		e.incomingError("message_history_partial")
	} else {
		e.incomingError("")
	}
	return nil
}

func pollConversation(ctx context.Context, api MessagingAPI, account string, conv DirectConversation, after, at time.Time) ([]Event, string) {
	cursor := ""
	events := []Event{}
	for pageNum := 0; pageNum < 100; pageNum++ {
		if ctx.Err() != nil {
			return nil, "message_history_timeout"
		}
		page, err := api.Messages(ctx, conv.UserID, cursor)
		if ctx.Err() != nil {
			return nil, "message_history_timeout"
		}
		if err != nil || page.ConversationID != conv.ID || page.UserID != conv.UserID || page.AccountID != account {
			return nil, "message_history_failed"
		}
		crossed := false
		for _, msg := range page.Messages {
			if !msg.SentAt.After(after) {
				crossed = true
			}
			if msg.Direction != "incoming" || !msg.SentAt.After(after) || msg.SentAt.After(at) {
				continue
			}
			id := hash([]byte(account + ":message:" + conv.ID + ":" + msg.ID))
			copy := msg
			events = append(events, Event{SchemaVersion: "1", EventID: id, Type: "message.received", AccountID: account, OccurredAt: msg.SentAt, DetectedAt: at, Data: EventData{UserID: conv.UserID, ConversationID: conv.ID, MessageID: msg.ID, Message: &copy}})
		}
		if crossed || page.Exhausted {
			return events, ""
		}
		next, nerr := strconv.ParseUint(page.NextCursor, 10, 64)
		previous, _ := strconv.ParseUint(cursor, 10, 64)
		if nerr != nil || next == 0 || (cursor != "" && next >= previous) {
			return nil, "message_history_incomplete"
		}
		cursor = page.NextCursor
	}
	return nil, "message_history_incomplete"
}
