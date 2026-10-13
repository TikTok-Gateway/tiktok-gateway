package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	g "tiktok-chat-bot/internal/greeter"
	"time"
)

func TestRejectionCodesSurviveRestartAndDuplicate(t *testing.T) {
	for _, stage := range []string{"send", "prepare"} {
		for _, recipient := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recipient=%t", stage, recipient), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "gateway.db")
				s, err := Open(path, "100")
				must(t, err)
				defer func() { s.Close() }()
				cause, check := g.ErrDenied, uint64(7170)
				pause, reason := "tiktok_restriction", "tiktok_rejected_send"
				if stage == "prepare" {
					reason = "tiktok_rejected_conversation"
				}
				if recipient {
					cause, check, pause, reason = g.ErrRecipientUnavailable, 7281, "", "recipient_unavailable"
				}
				rejection := fmt.Errorf("private response must not be persisted: %w", &g.APIRejection{Status: 3, Check: check, Cause: cause})
				f := &fake{}
				if stage == "send" {
					f.sendErr = rejection
				} else {
					f.prepareErr = rejection
				}
				e := &Engine{Store: s, API: f}
				must(t, s.Ingest(nil, time.Now().Add(-time.Hour)))
				must(t, e.Resume(context.Background()))
				original, err := s.Enqueue("original-key", command())
				must(t, err)
				must(t, e.Work(context.Background()))
				must(t, s.Close())
				s, err = Open(path, "100")
				must(t, err)
				e.Store = s
				duplicate, err := s.Enqueue("original-key", command())
				must(t, err)
				if duplicate.ID != original.ID || duplicate.State != "failed" || duplicate.Reason != reason {
					t.Fatal(duplicate)
				}
				m, err := s.Meta()
				must(t, err)
				if m.Paused != pause {
					t.Fatal(m)
				}
				before := f.sends.Load()
				must(t, e.Work(context.Background()))
				if f.sends.Load() != before {
					t.Fatal("retried failed operation")
				}
				req := httptest.NewRequest("GET", "/v1/operations/"+original.ID, nil)
				req.Header.Set("Authorization", "Bearer test-token")
				response := httptest.NewRecorder()
				e.Handler("test-token").ServeHTTP(response, req)
				if response.Code != 200 || strings.Contains(response.Body.String(), "private response") {
					t.Fatal(response.Body.String())
				}
				var got Operation
				must(t, json.Unmarshal(response.Body.Bytes(), &got))
				if got.TikTokRejection == nil || got.TikTokRejection.Status != 3 || got.TikTokRejection.Check != check {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestOperationWithoutRejectionRemainsCompatible(t *testing.T) {
	var o Operation
	must(t, json.Unmarshal([]byte(`{"operationId":"old","state":"failed","reason":"tiktok_rejected_send"}`), &o))
	b, err := json.Marshal(o)
	must(t, err)
	if o.TikTokRejection != nil || strings.Contains(string(b), "tiktokRejection") {
		t.Fatal(string(b))
	}
	if rejectionCodes(fmt.Errorf("ordinary transport error")) != nil {
		t.Fatal("invented rejection codes")
	}
	codes := rejectionCodes(&g.APIRejection{Status: 0, Check: 0, Cause: g.ErrDenied})
	b, err = json.Marshal(codes)
	must(t, err)
	if string(b) != `{"status":0,"check":0}` {
		t.Fatal(string(b))
	}
}
