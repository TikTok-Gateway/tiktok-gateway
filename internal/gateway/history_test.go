package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type messageFake struct {
	Mock
	pages map[string]MessagePage
	err   error
	calls []string
}

func (f *messageFake) Conversations(context.Context) ([]DirectConversation, error) {
	return []DirectConversation{{"0:1:100:200", "200"}}, f.err
}
func (f *messageFake) Messages(_ context.Context, user, cursor string) (MessagePage, error) {
	f.calls = append(f.calls, user+":"+cursor)
	if f.err != nil {
		return MessagePage{}, f.err
	}
	p, ok := f.pages[cursor]
	if !ok {
		return MessagePage{}, ErrConversationNotFound
	}
	return p, nil
}
func historyFixture(at time.Time) (*messageFake, Message) {
	m := Message{ID: "22", SenderID: "200", Direction: "incoming", Type: "text", Text: "да", SentAt: at}
	p := MessagePage{AccountID: "100", UserID: "200", ConversationID: "0:1:100:200", Messages: []Message{m}, NextCursor: "10"}
	empty := p
	empty.Messages = []Message{}
	empty.NextCursor = ""
	empty.Exhausted = true
	return &messageFake{pages: map[string]MessagePage{"": p, "10": empty}}, m
}
func TestHistoryHTTPReadOnlyAndSanitized(t *testing.T) {
	_, e, _ := setup(t)
	f, _ := historyFixture(time.Now())
	e.API = f
	h := e.Handler("secret")
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/v1/conversations/200/messages", "", 401},
		{"/v1/conversations/no/messages", "secret", 400},
		{"/v1/conversations/200/messages?cursor=0", "secret", 400},
		{"/v1/conversations/200/messages?cursor=oops", "secret", 400},
		{"/v1/conversations/200/messages", "secret", 200},
		{"/v1/conversations/200/messages?cursor=10", "secret", 200},
		{"/v1/conversations/200/messages?cursor=999", "secret", 404},
	} {
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	counts, err := e.Store.Counts()
	must(t, err)
	for _, n := range counts {
		if n != 0 {
			t.Fatal("read created operation")
		}
	}
	f.err = errors.New("private signed URL or cookie")
	req := httptest.NewRequest(http.MethodGet, "/v1/conversations/200/messages", nil)
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 503 || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Body.String())
	}
}
func TestIncomingBaselineDedupAndCheckpointAtomicity(t *testing.T) {
	s, e, _ := setup(t)
	f, msg := historyFixture(time.Now().Add(-time.Hour))
	e.API = f
	must(t, e.PollMessages(context.Background()))
	var count int
	must(t, s.db.QueryRow(`SELECT count(*) FROM events WHERE json_extract(body,'$.type')='message.received'`).Scan(&count))
	if count != 0 {
		t.Fatal("baseline emitted old messages")
	}
	start, _, err := s.incomingTimes()
	must(t, err)
	// All rows are newer than baseline; own messages must not become events.
	msg.SentAt = start.Add(time.Microsecond)
	p := f.pages[""]
	p.Messages = []Message{msg, {ID: "23", SenderID: "100", Direction: "outgoing", Type: "text", Text: "manual reply", SentAt: msg.SentAt}}
	f.pages[""] = p
	time.Sleep(time.Millisecond)
	must(t, e.PollMessages(context.Background()))
	must(t, e.PollMessages(context.Background()))
	var body []byte
	must(t, s.db.QueryRow(`SELECT body FROM events WHERE json_extract(body,'$.type')='message.received'`).Scan(&body))
	var ev Event
	must(t, json.Unmarshal(body, &ev))
	if ev.Data.MessageID != msg.ID || ev.Data.Message.Text != "да" {
		t.Fatal(string(body))
	}
	must(t, s.db.QueryRow(`SELECT count(*) FROM events WHERE json_extract(body,'$.type')='message.received'`).Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
	_, last, err := s.incomingTimes()
	must(t, err)
	f.err = errors.New("private failure")
	must(t, e.PollMessages(context.Background()))
	_, after, err := s.incomingTimes()
	must(t, err)
	if !after.Equal(last) {
		t.Fatal("failed poll advanced checkpoint")
	}
	f.err = nil
	p = f.pages["10"]
	p.Exhausted = false
	p.NextCursor = "10"
	f.pages["10"] = p
	must(t, e.PollMessages(context.Background()))
	_, after, err = s.incomingTimes()
	must(t, err)
	if !after.After(last) {
		t.Fatal("completed partial poll did not advance status time")
	}
	checkpoint, err := s.conversationCheckpoint(DirectConversation{"0:1:100:200", "200"}, after)
	must(t, err)
	if !checkpoint.Equal(last) {
		t.Fatal("stalled conversation advanced its checkpoint")
	}
	status, err := e.IncomingStatus()
	must(t, err)
	if status.Error != "message_history_partial" || len(status.FailedConversations) != 1 || status.FailedConversations[0].Error != "message_history_incomplete" {
		t.Fatal(status)
	}
}
