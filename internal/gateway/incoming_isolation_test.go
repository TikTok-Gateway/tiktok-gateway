package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type isolatedMessages struct {
	Mock
	conversations []DirectConversation
	fail          string
	calls         []string
	at            time.Time
	broken        bool
}

func (f *isolatedMessages) Conversations(context.Context) ([]DirectConversation, error) {
	return f.conversations, nil
}
func (f *isolatedMessages) Messages(ctx context.Context, user, cursor string) (MessagePage, error) {
	f.calls = append(f.calls, user+":"+cursor)
	p := MessagePage{AccountID: "100", UserID: user, ConversationID: "0:1:100:" + user, Exhausted: true, Messages: []Message{{ID: user, SenderID: user, Direction: "incoming", Type: "text", Text: "reply", SentAt: f.at}}}
	if user == "200" && f.broken {
		switch f.fail {
		case "error":
			return MessagePage{}, errors.New("secret response must not leak")
		case "identity":
			p.UserID = "999"
		case "loop":
			p.Exhausted = false
			p.NextCursor = "18446744073709551615"
		}
	}
	return p, nil
}
func countIncoming(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	must(t, s.db.QueryRow(`SELECT count(*) FROM events WHERE json_extract(body,'$.type')='message.received'`).Scan(&n))
	return n
}
func TestConversationFailuresIsolatedAndRecoverAfterRestart(t *testing.T) {
	for _, reason := range []string{"loop", "error", "identity"} {
		for _, badFirst := range []bool{true, false} {
			t.Run(reason+map[bool]string{true: "/first", false: "/last"}[badFirst], func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "gateway.db")
				s, err := Open(path, "100")
				must(t, err)
				old := time.Now().Add(-time.Hour).UTC()
				must(t, s.ingestMessages(nil, old))
				convs := []DirectConversation{{"0:1:100:200", "200"}, {"0:1:100:300", "300"}}
				if !badFirst {
					convs[0], convs[1] = convs[1], convs[0]
				}
				f := &isolatedMessages{conversations: convs, fail: reason, broken: true, at: old.Add(10 * time.Minute)}
				e := &Engine{Store: s, API: f}
				must(t, e.PollMessages(context.Background()))
				if countIncoming(t, s) != 1 {
					t.Fatal("bad conversation blocked or emitted events")
				}
				checkpoint, err := s.conversationCheckpoint(DirectConversation{"0:1:100:200", "200"}, time.Now())
				must(t, err)
				if !checkpoint.Equal(old) {
					t.Fatal("failure advanced checkpoint")
				}
				status, err := e.IncomingStatus()
				must(t, err)
				if status.Error != "message_history_partial" || len(status.FailedConversations) != 1 || status.FailedConversations[0].UserID != "200" || !status.LastPollAt.After(old) {
					t.Fatal(status)
				}
				must(t, s.Close())
				s, err = Open(path, "100")
				must(t, err)
				defer s.Close()
				e = &Engine{Store: s, API: f}
				status, err = e.IncomingStatus()
				must(t, err)
				if len(status.FailedConversations) != 1 || status.Error != "message_history_partial" {
					t.Fatal("failure lost on restart", status)
				}
				f.broken = false
				must(t, e.PollMessages(context.Background()))
				must(t, e.PollMessages(context.Background()))
				if countIncoming(t, s) != 2 {
					t.Fatal("lost older recovered event or duplicated healthy event")
				}
				var n int
				must(t, s.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n))
				if n != 2 {
					t.Fatal("outbox mismatch", n)
				}
				status, err = e.IncomingStatus()
				must(t, err)
				if status.Error != "" || len(status.FailedConversations) != 0 {
					t.Fatal("recovery did not clear failure", status)
				}
			})
		}
	}
}
func TestConversationEventsAndCheckpointAreAtomic(t *testing.T) {
	s, e, _ := setup(t)
	old := time.Now().Add(-time.Hour)
	must(t, s.ingestMessages(nil, old))
	conv := DirectConversation{"0:1:100:200", "200"}
	e.API = &isolatedMessages{conversations: []DirectConversation{conv}, at: old.Add(time.Minute)}
	_, err := s.db.Exec(`CREATE TRIGGER fail_message_outbox BEFORE INSERT ON outbox BEGIN SELECT RAISE(ABORT,'test failure'); END;`)
	must(t, err)
	if e.PollMessages(context.Background()) == nil {
		t.Fatal("storage error swallowed")
	}
	checkpoint, err := s.conversationCheckpoint(conv, time.Now())
	must(t, err)
	if !checkpoint.Equal(old) || countIncoming(t, s) != 0 {
		t.Fatal("partial transaction committed")
	}
}
func TestCanceledConversationCannotCommitPartialEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &isolatedMessages{}
	events, reason := pollConversation(ctx, f, "100", DirectConversation{"0:1:100:200", "200"}, time.Now().Add(-time.Hour), time.Now())
	if len(events) != 0 || reason != "message_history_timeout" || len(f.calls) != 0 {
		t.Fatal(events, reason)
	}
}
