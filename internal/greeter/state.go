package greeter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const Greeting = "Привет! Спасибо большое за подписочку ❤️ А ты знаешь про мой тгк?)"

type Event struct {
	ID       string
	UserID   string
	Username string
	Created  int64
}
type Conversation struct {
	ID     string
	Source uint64
}
type Rejection struct {
	Status       uint64
	Check        uint64
	BusinessCode uint64 `json:"businessCode,omitempty"`
}

type StepResult struct {
	Step int       `json:"step"`
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}
type Job struct {
	Step         int          `json:",omitempty"`
	Results      []StepResult `json:",omitempty"`
	Settings     *Settings    `json:",omitempty"`
	Rejection    *Rejection   `json:",omitempty"`
	Prechecked   bool
	Event        Event
	Detected     time.Time
	Due          time.Time
	Phase        string
	Reason       string `json:",omitempty"`
	Conversation Conversation
	TextID       string `json:",omitempty"`
	StickerID    string `json:",omitempty"`
	Updated      time.Time
}
type State struct {
	Version             int
	SelfID              string
	Started             time.Time
	LastPoll            time.Time
	Heartbeat           time.Time
	Paused              string
	Seen                map[string]bool
	Jobs                map[string]*Job
	StickerConversation Conversation
	StickerMessage      uint64
}

func Load(path string) (*State, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	var s State
	if json.Unmarshal(b, &s) != nil || s.Version != 1 || s.SelfID == "" || s.Seen == nil || s.Jobs == nil || s.StickerMessage == 0 {
		return nil, fmt.Errorf("invalid state; refusing to reset history")
	}
	return &s, nil
}

// The on-disk transition must commit before any externally visible send.
func Save(path string, s *State) error {
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	return AtomicWrite(path, b)
}
func AtomicWrite(path string, b []byte) error {
	d := filepath.Dir(path)
	if e := os.MkdirAll(d, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(d, ".state-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	dir, e := os.Open(d)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func Lock(path string) (*os.File, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("another greeter is running")
	}
	return f, nil
}
