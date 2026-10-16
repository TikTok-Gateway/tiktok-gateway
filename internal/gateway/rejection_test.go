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
				rejection := fmt.Errorf("private response must not be persisted: %w", &g.APIRejection{Status: 3, Check: 2, BusinessCode: check, Cause: cause})
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
				if got.TikTokRejection == nil || got.TikTokRejection.Status != 3 || got.TikTokRejection.Check != 2 || got.TikTokRejection.BusinessCode != check {
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

func TestRecipientRejectionDoesNotStopOtherRecipient(t *testing.T) {
	for _, code := range []uint64{7192, 7202, 7409} {
		for _, stage := range []string{"send", "prepare"} {
			t.Run(fmt.Sprintf("%s/%d", stage, code), func(t *testing.T) {
				s, e, f := setup(t)
				denied := &g.APIRejection{Status: 3, Check: 2, BusinessCode: code, Cause: g.ErrRecipientUnavailable}
				if code == 7192 {
					denied.Status, denied.Check = 0, 0
				}
				if stage == "send" {
					f.sendErr = denied
				} else {
					f.prepareErr = denied
				}
				first, err := s.Enqueue("unavailable-recipient", command())
				must(t, err)
				must(t, e.Work(context.Background()))
				got, err := s.Get(first.ID)
				must(t, err)
				if got.State != "failed" || got.Reason != "recipient_unavailable" || got.TikTokRejection.BusinessCode != code {
					t.Fatal(got)
				}
				f.sendErr = nil
				f.prepareErr = nil
				next := command()
				next.RecipientUserID = "300"
				second, err := s.Enqueue("another-recipient", next)
				must(t, err)
				must(t, e.Work(context.Background()))
				got, err = s.Get(second.ID)
				must(t, err)
				if got.State != "succeeded" {
					t.Fatal("other recipient blocked", got)
				}
				duplicate, err := s.Enqueue("unavailable-recipient", command())
				must(t, err)
				if duplicate.ID != first.ID || duplicate.State != "failed" {
					t.Fatal("failed send retried")
				}
			})
		}
	}
}

func TestSticker7175IsolationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path, "100")
	must(t, err)
	defer func() { s.Close() }()
	f := &fake{verify: true, sendErr: &g.APIRejection{Status: 3, Check: 2, BusinessCode: 7175, Cause: g.ErrDenied}}
	e := &Engine{Store: s, API: f}
	must(t, s.Ingest(nil, time.Now().Add(-time.Hour)))
	must(t, e.Resume(context.Background()))
	c := Command{RecipientUserID: "200", Type: "sticker", StickerID: "1"}
	first, err := s.Enqueue("sticker-7175", c)
	must(t, err)
	must(t, e.Work(context.Background()))
	must(t, s.Close())
	s, err = Open(path, "100")
	must(t, err)
	e.Store = s
	got, err := s.Enqueue("sticker-7175", c)
	must(t, err)
	if got.ID != first.ID || got.State != "failed" || got.Reason != "tiktok_rejected_sticker" || got.TikTokRejection.BusinessCode != 7175 {
		t.Fatal(got)
	}
	meta, err := s.Meta()
	must(t, err)
	if meta.Paused != "" {
		t.Fatal(meta)
	}
	f.sendErr = nil
	next := command()
	next.RecipientUserID = "300"
	second, err := s.Enqueue("next-recipient", next)
	must(t, err)
	must(t, e.Work(context.Background()))
	got, err = s.Get(second.ID)
	must(t, err)
	if got.State != "succeeded" || f.sends.Load() != 2 {
		t.Fatal("next message blocked or failed sticker retried", got, f.sends.Load())
	}
}

func TestStickerExceptionDoesNotRelaxOtherRejections(t *testing.T) {
	for _, tc := range []struct {
		name, kind, stage       string
		status, check, business uint64
	}{
		{"text", "text", "send", 3, 2, 7175},
		{"preflight", "sticker", "prepare", 3, 2, 7175},
		{"unknown", "sticker", "send", 3, 2, 9999},
		{"sender", "sticker", "send", 3, 2, 7289},
		{"conflicting-status", "sticker", "send", 7289, 2, 7175},
		{"conflicting-check", "sticker", "send", 3, 7170, 7175},
		{"generic-status-only", "sticker", "send", 7175, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e, f := setup(t)
			denied := &g.APIRejection{Status: tc.status, Check: tc.check, BusinessCode: tc.business, Cause: g.ErrDenied}
			if tc.stage == "send" {
				f.sendErr = denied
			} else {
				f.prepareErr = denied
			}
			c := command()
			if tc.kind == "sticker" {
				c = Command{RecipientUserID: "200", Type: "sticker", StickerID: "1"}
			}
			first, err := s.Enqueue("denied", c)
			must(t, err)
			must(t, e.Work(context.Background()))
			got, err := s.Get(first.ID)
			must(t, err)
			meta, err := s.Meta()
			must(t, err)
			if got.State != "failed" || meta.Paused != "tiktok_restriction" {
				t.Fatal(got, meta)
			}
		})
	}
}
