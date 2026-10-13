package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	g "tiktok-chat-bot/internal/greeter"
	"time"
)

type fake struct {
	Mock
	sends                    atomic.Int32
	sendErr                  error
	profile                  g.Profile
	prepareErr               error
	verify                   bool
	checkErr                 error
	sendStarted, sendRelease chan struct{}
}

func (f *fake) Check(context.Context) error                        { return f.checkErr }
func (f *fake) Profile(context.Context, string) (g.Profile, error) { return f.profile, nil }
func (f *fake) Prepare(ctx context.Context, id string) (g.Conversation, error) {
	if f.prepareErr != nil {
		return g.Conversation{}, f.prepareErr
	}
	return f.Mock.Prepare(ctx, id)
}
func (f *fake) Send(context.Context, g.Conversation, string, string) (string, error) {
	f.sends.Add(1)
	if f.sendStarted != nil {
		close(f.sendStarted)
		<-f.sendRelease
	}
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "123", nil
}
func (f *fake) Verify(context.Context, Operation) (bool, error) { return f.verify, nil }
func setup(t *testing.T) (*Store, *Engine, *fake) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"), "100")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	f := &fake{verify: true}
	e := &Engine{Store: s, API: f}
	must(t, s.Ingest(nil, time.Now().Add(-time.Hour)))
	must(t, e.Resume(context.Background()))
	return s, e, f
}
func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func command() Command { return Command{RecipientUserID: "200", Type: "text", Text: "hello"} }
func TestConcurrentIdempotencyAndNoFollowerRule(t *testing.T) {
	s, e, f := setup(t)
	var wg sync.WaitGroup
	ids := make(chan string, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := s.Enqueue("event:scenario:action", command())
			if err != nil {
				t.Error(err)
			}
			ids <- o.ID
		}()
	}
	wg.Wait()
	close(ids)
	id := ""
	for v := range ids {
		if id != "" && id != v {
			t.Fatal("duplicate operation")
		}
		id = v
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := e.Work(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.sends.Load() != 1 {
		t.Fatal("duplicate send")
	}
	o, err := s.Get(id)
	must(t, err)
	if o.State != "succeeded" {
		t.Fatal(o)
	}
	c := command()
	c.Text = "changed"
	_, err = s.Enqueue("event:scenario:action", c)
	if !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	_, err = s.Enqueue("another-action", command())
	must(t, err)
	must(t, e.Work(context.Background()))
	if f.sends.Load() != 2 {
		t.Fatal("identical text wrongly blocked")
	}
}
func TestRestartEveryPhase(t *testing.T) {
	for _, phase := range []string{"queued", "sending", "verifying", "succeeded", "failed", "uncertain"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			s, err := Open(path, "100")
			must(t, err)
			must(t, s.Ingest(nil, time.Now().Add(-time.Hour)))
			o, err := s.Enqueue("key", command())
			must(t, err)
			o.State = phase
			o.MessageID = "123"
			o.Conversation = g.Conversation{ID: "conv", Source: 5}
			must(t, s.Save(o, ""))
			must(t, s.Pause(""))
			must(t, s.Close())
			s, err = Open(path, "100")
			must(t, err)
			defer s.Close()
			must(t, s.Recover())
			f := &fake{verify: true}
			e := &Engine{Store: s, API: f}
			must(t, e.Work(context.Background()))
			got, err := s.Get(o.ID)
			must(t, err)
			if got.Conversation.ID != "conv" && phase != "queued" {
				t.Fatal("routing lost")
			}
			switch phase {
			case "queued":
				if got.State != "succeeded" || f.sends.Load() != 1 {
					t.Fatal(got)
				}
			case "sending", "verifying", "uncertain":
				if got.State != "uncertain" || f.sends.Load() != 0 {
					t.Fatal(got)
				}
				if e.Resume(context.Background()) == nil {
					t.Fatal("unsafe resume")
				}
				got, err = e.Reconcile(context.Background(), o.ID)
				must(t, err)
				if got.State != "succeeded" || f.sends.Load() != 0 {
					t.Fatal(got)
				}
				must(t, e.Resume(context.Background()))
			default:
				if got.State != phase || f.sends.Load() != 0 {
					t.Fatal(got)
				}
			}
		})
	}
}
func TestUnknownNeverResends(t *testing.T) {
	s, e, f := setup(t)
	f.sendErr = errors.New("timeout with secret response")
	o, err := s.Enqueue("key", command())
	must(t, err)
	must(t, e.Work(context.Background()))
	must(t, e.Work(context.Background()))
	got, err := e.Reconcile(context.Background(), o.ID)
	must(t, err)
	if got.State != "uncertain" || got.Reason != "missing_server_id_cannot_prove_delivery" || f.sends.Load() != 1 {
		t.Fatal(got)
	}
	if e.Resume(context.Background()) == nil {
		t.Fatal("resumed uncertain")
	}
}
func TestRejectionsAndSession(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		paused bool
	}{{"recipient", g.ErrRecipientUnavailable, false}, {"sender", g.ErrDenied, true}, {"timeout", errors.New("timeout"), true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, e, f := setup(t)
			f.sendErr = tc.err
			o, err := s.Enqueue("key", command())
			must(t, err)
			must(t, e.Work(context.Background()))
			o, err = s.Get(o.ID)
			must(t, err)
			m, err := s.Meta()
			must(t, err)
			if (m.Paused != "") != tc.paused {
				t.Fatal(m)
			}
			if tc.name != "timeout" && o.State != "failed" {
				t.Fatal(o)
			}
		})
	}
	s, e, f := setup(t)
	f.checkErr = errors.New("bad session")
	_, err := s.Enqueue("key", command())
	must(t, err)
	must(t, e.Work(context.Background()))
	if f.sends.Load() != 0 {
		t.Fatal("invalid session sent")
	}
	if e.Resume(context.Background()) == nil {
		t.Fatal("invalid resume")
	}
}
func TestStickerMissingNoSubstitution(t *testing.T) {
	s, e, f := setup(t)
	o, err := s.Enqueue("key", Command{RecipientUserID: "200", Type: "sticker", StickerID: "999"})
	must(t, err)
	must(t, e.Work(context.Background()))
	o, err = s.Get(o.ID)
	must(t, err)
	if o.State != "failed" || o.Reason != "sticker_source_unavailable" || f.sends.Load() != 0 {
		t.Fatal(o)
	}
}
func TestPauseDuringSendAndDetection(t *testing.T) {
	s, e, f := setup(t)
	f.sendStarted = make(chan struct{})
	f.sendRelease = make(chan struct{})
	_, err := s.Enqueue("first", command())
	must(t, err)
	_, err = s.Enqueue("second", command())
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- e.Work(context.Background()) }()
	<-f.sendStarted
	must(t, e.Pause())
	close(f.sendRelease)
	must(t, <-done)
	must(t, e.Work(context.Background()))
	if f.sends.Load() != 1 {
		t.Fatal("pause failed")
	}
	must(t, s.Ingest([]g.Event{{ID: "new", UserID: "200", Created: time.Now().Unix()}}, time.Now()))
	status, err := s.WebhookStatus()
	must(t, err)
	if status.Pending != 1 {
		t.Fatal(status)
	}
}
func TestOutboxBaselineRefollowRetryAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path, "100")
	must(t, err)
	at := time.Now().Add(-time.Hour)
	must(t, s.Ingest([]g.Event{{ID: "old", UserID: "200", Created: at.Unix() - 1}}, at))
	events := []g.Event{{ID: "a", UserID: "200", Created: at.Unix() + 10}, {ID: "b", UserID: "200", Created: at.Unix() + 20}}
	must(t, s.Ingest(events, time.Now()))
	must(t, s.Ingest(events, time.Now()))
	var ids []string
	var accept atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer hook-token" {
			t.Error("auth missing")
		}
		var ev Event
		if json.NewDecoder(r.Body).Decode(&ev) != nil {
			t.Error("invalid body")
		}
		ids = append(ids, ev.EventID)
		if accept.Load() {
			w.WriteHeader(204)
		} else {
			w.WriteHeader(503)
		}
	}))
	defer server.Close()
	h := Webhook{Store: s, URL: server.URL, Token: "hook-token"}
	must(t, h.Deliver(context.Background()))
	status, err := s.WebhookStatus()
	must(t, err)
	if status.Pending != 2 {
		t.Fatal(status)
	}
	must(t, s.Close())
	s, err = Open(path, "100")
	must(t, err)
	defer s.Close()
	h.Store = s
	_, err = s.db.Exec(`UPDATE outbox SET next_at=0`)
	must(t, err)
	must(t, h.Deliver(context.Background()))
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatal("unstable event ID", ids)
	}
	accept.Store(true)
	_, err = s.db.Exec(`UPDATE outbox SET next_at=0`)
	must(t, err)
	must(t, h.Deliver(context.Background()))
	must(t, h.Deliver(context.Background()))
	status, err = s.WebhookStatus()
	must(t, err)
	if status.Pending != 0 {
		t.Fatal(status)
	}
}
func TestImportAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path, "100")
	must(t, err)
	defer s.Close()
	if other, err := Open(path, "100"); err == nil {
		other.Close()
		t.Fatal("second instance admitted")
	}
	old := g.State{Version: 1, SelfID: "100", Seen: map[string]bool{"event": true}, Jobs: map[string]*g.Job{"200": {Phase: "text_attempted"}}, Started: time.Now().Add(-time.Hour), LastPoll: time.Now()}
	b, _ := json.Marshal(old)
	if s.Import(b) == nil {
		t.Fatal("unsafe import")
	}
	old.Jobs["200"].Phase = "sent"
	b, _ = json.Marshal(old)
	must(t, s.Import(b))
	must(t, s.Import(b))
	status, err := s.WebhookStatus()
	must(t, err)
	if status.Pending != 0 {
		t.Fatal("replayed import")
	}
	var n int
	must(t, s.db.QueryRow(`SELECT count(*) FROM legacy_jobs`).Scan(&n))
	if n != 1 {
		t.Fatal(n)
	}
	m, err := s.Meta()
	must(t, err)
	if m.Paused != "imported_pause" || !m.LastPoll.Equal(old.LastPoll) {
		t.Fatal(m)
	}
}
func TestHTTPContract(t *testing.T) {
	_, e, _ := setup(t)
	h := e.Handler("test-token")
	call := func(method, path, key, body, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Idempotency-Key", key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if call("GET", "/v1/status", "", "", "bad").Code != 401 {
		t.Fatal("auth bypass")
	}
	if call("GET", "/healthz", "", "", "").Code != 200 {
		t.Fatal("health")
	}
	body := `{"recipientUserId":"200","type":"text","text":"hello"}`
	if call("POST", "/v1/messages", "", body, "test-token").Code != 400 {
		t.Fatal("missing key")
	}
	a := call("POST", "/v1/messages", "same", body, "test-token")
	b := call("POST", "/v1/messages", "same", body, "test-token")
	if a.Code != 202 || a.Body.String() != b.Body.String() {
		t.Fatal(a, b)
	}
	if call("POST", "/v1/messages", "same", `{"recipientUserId":"200","type":"text","text":"other"}`, "test-token").Code != 409 {
		t.Fatal("missing conflict")
	}
	if call("POST", "/v1/messages", "other", body+` {}`, "test-token").Code != 400 {
		t.Fatal("trailing JSON")
	}
}

func TestWebhookErrorsRetainEvents(t *testing.T) {
	for _, tc := range []struct {
		code   int
		reason string
	}{{401, "webhook_authorization_error"}, {403, "webhook_authorization_error"}, {404, "webhook_address_or_request_error"}, {302, "webhook_address_or_request_error"}, {429, "webhook_rate_limited"}, {500, "webhook_server_error"}} {
		t.Run(tc.reason, func(t *testing.T) {
			s, _, _ := setup(t)
			must(t, s.Ingest([]g.Event{{ID: "event", UserID: "200", Created: time.Now().Unix()}}, time.Now()))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "/never-follow")
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(tc.code)
			}))
			defer server.Close()
			hook := Webhook{Store: s, URL: server.URL, Token: "synthetic"}
			must(t, hook.Deliver(context.Background()))
			status, err := s.WebhookStatus()
			must(t, err)
			if status.Pending != 1 || status.LastError != tc.reason {
				t.Fatal(status)
			}
			var next int64
			must(t, s.db.QueryRow(`SELECT next_at FROM outbox`).Scan(&next))
			if next <= time.Now().Unix() {
				t.Fatal("missing retry delay")
			}
		})
	}
	s, _, _ := setup(t)
	must(t, s.Ingest([]g.Event{{ID: "event", UserID: "200", Created: time.Now().Unix()}}, time.Now()))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer server.Close()
	hook := Webhook{Store: s, URL: server.URL, Token: "synthetic", Client: &http.Client{Timeout: 20 * time.Millisecond}}
	must(t, hook.Deliver(context.Background()))
	status, err := s.WebhookStatus()
	must(t, err)
	if status.Pending != 1 || status.LastError != "webhook_network_error" {
		t.Fatal(status)
	}
}
func TestStoreFailureCannotDispatch(t *testing.T) {
	s, e, f := setup(t)
	_, err := s.Enqueue("key", command())
	must(t, err)
	must(t, s.db.Close())
	if e.Work(context.Background()) == nil {
		t.Fatal("storage error ignored")
	}
	if f.sends.Load() != 0 {
		t.Fatal("sent without persistence")
	}
}
