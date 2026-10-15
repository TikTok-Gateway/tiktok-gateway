package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	g "tiktok-chat-bot/internal/greeter"
	"time"
)

func TestSendErrorDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		input error
		want  string
	}{
		{nil, "missing server message ID in send response"},
		{errors.New("POST send message: unexpected EOF"), "POST send message: unexpected EOF"},
		{errors.New(`POST send message: Post "https://example.com/send?msToken=secret": context deadline exceeded`), `POST send message: Post "[URL omitted]": context deadline exceeded`},
		{errors.New("send API returned 503: secret body\nmore secret"), "send API returned 503: [response body omitted]"},
		{errors.New("transport failed: Cookie: sessionid=secret; other=secret"), "transport failed: [credentials omitted]"},
	} {
		if got := sendErrorDiagnostic(tc.input); got != tc.want {
			t.Fatalf("got %q, want %q", got, tc.want)
		}
	}
	if len([]rune(sendErrorDiagnostic(errors.New(strings.Repeat("я", 3000))))) != 2049 {
		t.Fatal("diagnostic not bounded")
	}
}

func TestSendErrorPersistsAndNeverResends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path, "100")
	must(t, err)
	defer func() { s.Close() }()
	f := &fake{sendErr: errors.New("POST send message: unexpected EOF")}
	e := &Engine{Store: s, API: f}
	must(t, s.Ingest(nil, time.Now().Add(-time.Hour)))
	must(t, e.Resume(context.Background()))
	o, err := s.Enqueue("diagnostic-key", command())
	must(t, err)
	must(t, e.Work(context.Background()))
	must(t, s.Close())
	s, err = Open(path, "100")
	must(t, err)
	e.Store = s
	got, err := s.Enqueue("diagnostic-key", command())
	must(t, err)
	if got.ID != o.ID || got.State != "uncertain" || got.Reason != "send_result_unknown" || got.SendError != f.sendErr.Error() {
		t.Fatal(got)
	}
	_, err = e.Reconcile(context.Background(), o.ID)
	must(t, err)
	req := httptest.NewRequest("GET", "/v1/operations/"+o.ID, nil)
	req.Header.Set("Authorization", "Bearer test")
	res := httptest.NewRecorder()
	e.Handler("test").ServeHTTP(res, req)
	must(t, json.Unmarshal(res.Body.Bytes(), &got))
	if res.Code != 200 || got.SendError != f.sendErr.Error() {
		t.Fatal(res.Body.String())
	}
	must(t, e.Work(context.Background()))
	if f.sends.Load() != 1 {
		t.Fatal("operation resent")
	}
}

type missingSendID struct{ *fake }

func (f missingSendID) Send(context.Context, g.Conversation, string, string) (string, error) {
	return "", nil
}

func TestMissingSendIDDiagnostic(t *testing.T) {
	s, e, f := setup(t)
	e.API = missingSendID{f}
	o, err := s.Enqueue("missing-id", command())
	must(t, err)
	must(t, e.Work(context.Background()))
	got, err := s.Get(o.ID)
	must(t, err)
	if got.State != "uncertain" || got.SendError != "missing server message ID in send response" {
		t.Fatal(got)
	}
}
