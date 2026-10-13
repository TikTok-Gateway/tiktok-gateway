// Package gateway implements durable commands and an at-least-once event outbox.
package gateway

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
	g "tiktok-chat-bot/internal/greeter"
)

var ErrConflict = errors.New("idempotency_key_conflict")
var ErrNotFound = errors.New("operation_not_found")

type Command struct {
	RecipientUserID string `json:"recipientUserId"`
	Type            string `json:"type"`
	Text            string `json:"text,omitempty"`
	StickerID       string `json:"stickerId,omitempty"`
}
type Operation struct {
	ID           string         `json:"operationId"`
	Command      Command        `json:"command"`
	State        string         `json:"state"`
	Reason       string         `json:"reason,omitempty"`
	MessageID    string         `json:"messageId,omitempty"`
	Conversation g.Conversation `json:"-"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
}

// Private persistence envelope keeps conversation routing out of HTTP responses.
type savedOperation struct {
	Operation
	Conversation g.Conversation `json:"conversation"`
}
type Meta struct {
	AccountID string    `json:"accountId"`
	Paused    string    `json:"pauseReason"`
	Started   time.Time `json:"startedAt"`
	LastPoll  time.Time `json:"lastPollAt"`
}
type Event struct {
	SchemaVersion string    `json:"schemaVersion"`
	EventID       string    `json:"eventId"`
	Type          string    `json:"type"`
	AccountID     string    `json:"accountId"`
	OccurredAt    time.Time `json:"occurredAt"`
	DetectedAt    time.Time `json:"detectedAt"`
	Data          EventData `json:"data"`
}
type EventData struct {
	UserID   string `json:"userId"`
	Username string `json:"username,omitempty"`
}
type Store struct {
	db   *sql.DB
	lock *os.File
	mu   sync.Mutex
}

func Open(path, account string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock, err := g.Lock(path + ".lock")
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version > 1 {
		s.Close()
		return nil, errors.New("unsupported_database_version")
	}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS meta (id INTEGER PRIMARY KEY CHECK(id=1), body BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY, key_hash TEXT NOT NULL UNIQUE, command_hash TEXT NOT NULL, state TEXT NOT NULL, body BLOB NOT NULL, created TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS seen (id TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, body BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS outbox (id TEXT PRIMARY KEY REFERENCES events(id), attempts INTEGER NOT NULL DEFAULT 0, next_at INTEGER NOT NULL DEFAULT 0, delivered INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '');
 CREATE TABLE IF NOT EXISTS imports (digest TEXT PRIMARY KEY);
 CREATE TABLE IF NOT EXISTS legacy_jobs (id TEXT PRIMARY KEY, body BLOB NOT NULL);
 PRAGMA user_version=1;`)
	if err != nil {
		s.Close()
		return nil, err
	}
	b, _ := json.Marshal(Meta{AccountID: account, Paused: "initial_pause"})
	if _, err = db.Exec(`INSERT OR IGNORE INTO meta VALUES (1,?)`, b); err != nil {
		s.Close()
		return nil, err
	}
	m, err := s.Meta()
	if err != nil || m.AccountID != account {
		s.Close()
		return nil, fmt.Errorf("database_account_mismatch_or_invalid")
	}
	if err = os.Chmod(path, 0600); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { err := s.db.Close(); s.lock.Close(); return err }
func (s *Store) Meta() (Meta, error) {
	var b []byte
	var m Meta
	err := s.db.QueryRow(`SELECT body FROM meta WHERE id=1`).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &m)
	}
	return m, err
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func marshalOperation(o Operation) []byte {
	b, _ := json.Marshal(savedOperation{o, o.Conversation})
	return b
}
func readOperation(b []byte) (Operation, error) {
	var v savedOperation
	err := json.Unmarshal(b, &v)
	v.Operation.Conversation = v.Conversation
	return v.Operation, err
}
func (s *Store) Enqueue(key string, c Command) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cb, _ := json.Marshal(c)
	kh, ch := hash([]byte(key)), hash(cb)
	var existing string
	var b []byte
	err := s.db.QueryRow(`SELECT command_hash,body FROM operations WHERE key_hash=?`, kh).Scan(&existing, &b)
	if err == nil {
		if existing != ch {
			return Operation{}, ErrConflict
		}
		return readOperation(b)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, err
	}
	now := time.Now().UTC()
	o := Operation{ID: uuid.NewString(), Command: c, State: "queued", CreatedAt: now, UpdatedAt: now}
	_, err = s.db.Exec(`INSERT INTO operations VALUES(?,?,?,?,?,?)`, o.ID, kh, ch, o.State, marshalOperation(o), now.Format(time.RFC3339Nano))
	return o, err
}
func (s *Store) Get(id string) (Operation, error) {
	var b []byte
	err := s.db.QueryRow(`SELECT body FROM operations WHERE id=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	return readOperation(b)
}
func (s *Store) Next() (Operation, error) {
	var b []byte
	err := s.db.QueryRow(`SELECT body FROM operations WHERE state='queued' ORDER BY rowid LIMIT 1`).Scan(&b)
	if err != nil {
		return Operation{}, err
	}
	return readOperation(b)
}
func (s *Store) Save(o Operation, pause string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	o.UpdatedAt = time.Now().UTC()
	_, err = tx.Exec(`UPDATE operations SET state=?,body=? WHERE id=?`, o.State, marshalOperation(o), o.ID)
	if err != nil {
		return err
	}
	if pause != "" {
		var b []byte
		var m Meta
		if err = tx.QueryRow(`SELECT body FROM meta WHERE id=1`).Scan(&b); err != nil {
			return err
		}
		if err = json.Unmarshal(b, &m); err != nil {
			return err
		}
		m.Paused = pause
		b, _ = json.Marshal(m)
		if _, err = tx.Exec(`UPDATE meta SET body=? WHERE id=1`, b); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Pause(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.Meta()
	if err != nil {
		return err
	}
	m.Paused = reason
	b, _ := json.Marshal(m)
	_, err = s.db.Exec(`UPDATE meta SET body=? WHERE id=1`, b)
	return err
}
func (s *Store) Counts() (map[string]int, error) {
	rows, err := s.db.Query(`SELECT state,count(*) FROM operations GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err = rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}
func (s *Store) Recover() error {
	rows, err := s.db.Query(`SELECT body FROM operations WHERE state IN ('sending','verifying')`)
	if err != nil {
		return err
	}
	var ops []Operation
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return err
		}
		o, e := readOperation(b)
		if e != nil {
			rows.Close()
			return e
		}
		ops = append(ops, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, o := range ops {
		o.State = "uncertain"
		o.Reason = "interrupted_send_reconcile_required"
		if err = s.Save(o, "uncertain_operations"); err != nil {
			return err
		}
	}
	counts, err := s.Counts()
	if err != nil {
		return err
	}
	if counts["uncertain"] > 0 {
		return s.Pause("uncertain_operations")
	}
	return nil
}
func (s *Store) Ingest(events []g.Event, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.Meta()
	if err != nil {
		return err
	}
	baseline := m.Started.IsZero()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, v := range events {
		r, e := tx.Exec(`INSERT OR IGNORE INTO seen VALUES(?)`, v.ID)
		if e != nil {
			return e
		}
		n, e := r.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 || baseline || v.Created <= m.Started.Unix() {
			continue
		}
		id := hash([]byte(m.AccountID + ":" + v.ID))
		ev := Event{"1", id, "follower.created", m.AccountID, time.Unix(v.Created, 0).UTC(), at, EventData{v.UserID, v.Username}}
		b, _ := json.Marshal(ev)
		if _, err = tx.Exec(`INSERT INTO events VALUES(?,?)`, id, b); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO outbox(id) VALUES(?)`, id); err != nil {
			return err
		}
	}
	if baseline {
		m.Started = at
	}
	m.LastPoll = at
	b, _ := json.Marshal(m)
	if _, err = tx.Exec(`UPDATE meta SET body=? WHERE id=1`, b); err != nil {
		return err
	}
	return tx.Commit()
}

// Import is offline, transactional, idempotent, and never enqueues legacy work.
func (s *Store) Import(raw []byte) error {
	var old g.State
	if json.Unmarshal(raw, &old) != nil || old.Version != 1 || old.SelfID == "" || old.Seen == nil || old.Jobs == nil || old.Started.IsZero() || old.LastPoll.Before(old.Started) {
		return errors.New("invalid_legacy_state")
	}
	for _, j := range old.Jobs {
		if j == nil {
			return errors.New("invalid_legacy_job")
		}
		switch j.Phase {
		case "sent", "skipped", "text_only":
		default:
			return fmt.Errorf("legacy_job_unresolved: phase %q; finish or reconcile in old bot before import", j.Phase)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM imports WHERE digest=?`, hash(raw)).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	m, err := s.Meta()
	if err != nil {
		return err
	}
	if m.AccountID != old.SelfID {
		return errors.New("import_account_mismatch")
	}
	if !m.Started.IsZero() {
		return errors.New("import_requires_fresh_database")
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM operations`).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return errors.New("import_requires_empty_queue")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, seen := range old.Seen {
		if seen {
			if _, err = tx.Exec(`INSERT OR IGNORE INTO seen VALUES(?)`, id); err != nil {
				return err
			}
		}
	}
	for id, j := range old.Jobs {
		b, _ := json.Marshal(j)
		if _, err = tx.Exec(`INSERT INTO legacy_jobs VALUES(?,?)`, id, b); err != nil {
			return err
		}
	}
	m.Started = old.Started
	m.LastPoll = old.LastPoll
	m.Paused = "imported_pause"
	b, _ := json.Marshal(m)
	if _, err = tx.Exec(`UPDATE meta SET body=? WHERE id=1`, b); err != nil {
		return err
	}
	if _, err = tx.Exec(`INSERT INTO imports VALUES(?)`, hash(raw)); err != nil {
		return err
	}
	return tx.Commit()
}
