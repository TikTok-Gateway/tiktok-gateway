package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

func numeric(id string) bool {
	v, e := strconv.ParseUint(id, 10, 64)
	return e == nil && v > 0 && strconv.FormatUint(v, 10) == id
}
func (c Command) Validate() bool {
	if !numeric(c.RecipientUserID) {
		return false
	}
	switch c.Type {
	case "text":
		return c.StickerID == "" && strings.TrimSpace(c.Text) != "" && utf8.ValidString(c.Text) && utf8.RuneCountInString(c.Text) <= 1000
	case "sticker":
		return c.Text == "" && numeric(c.StickerID)
	}
	return false
}
func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func problem(w http.ResponseWriter, code int, reason string) {
	reply(w, code, map[string]string{"error": reason})
}
func (e *Engine) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		m, err := e.Store.Meta()
		connected, _ := e.Connection()
		if err != nil || m.Started.IsZero() || !connected {
			problem(w, 503, "not_ready")
			return
		}
		reply(w, 200, map[string]bool{"ready": true})
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		m, err := e.Store.Meta()
		if err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		counts, err := e.Store.Counts()
		if err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		webhook, err := e.Store.WebhookStatus()
		if err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		incoming, err := e.IncomingStatus()
		if err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		connected, pollErr := e.Connection()
		reply(w, 200, map[string]any{"accountId": m.AccountID, "connected": connected, "paused": m.Paused != "", "pauseReason": m.Paused, "queue": counts, "lastPollAt": m.LastPoll, "pollError": pollErr, "webhook": webhook, "incoming": incoming})
	})
	mux.HandleFunc("GET /v1/conversations/{userId}/messages", func(w http.ResponseWriter, r *http.Request) {
		user, cursor := r.PathValue("userId"), r.URL.Query().Get("cursor")
		if !numeric(user) || (cursor != "" && !numeric(cursor)) {
			problem(w, 400, "invalid_history_query")
			return
		}
		api, ok := e.API.(MessagingAPI)
		if !ok {
			problem(w, 503, "messages_not_supported")
			return
		}
		page, err := api.Messages(r.Context(), user, cursor)
		if errors.Is(err, ErrConversationNotFound) {
			problem(w, 404, "conversation_not_found")
			return
		}
		if err != nil {
			problem(w, 503, "history_unavailable")
			return
		}
		reply(w, 200, page)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if strings.TrimSpace(key) == "" || len(key) > 256 {
			problem(w, 400, "idempotency_key_required_max_256")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 16384)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		var c Command
		if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || !c.Validate() {
			problem(w, 400, "invalid_command")
			return
		}
		m, err := e.Store.Meta()
		if err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		if c.RecipientUserID == m.AccountID {
			problem(w, 400, "self_recipient_not_supported")
			return
		}
		o, err := e.Store.Enqueue(key, c)
		if errors.Is(err, ErrConflict) {
			problem(w, 409, "idempotency_key_conflict")
			return
		}
		if err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		w.Header().Set("Location", "/v1/operations/"+o.ID)
		reply(w, 202, o)
	})
	mux.HandleFunc("GET /v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		o, err := e.Store.Get(r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			problem(w, 404, "operation_not_found")
			return
		}
		if err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		reply(w, 200, o)
	})
	mux.HandleFunc("POST /v1/operations/{id}/reconcile", func(w http.ResponseWriter, r *http.Request) {
		o, err := e.Reconcile(r.Context(), r.PathValue("id"))
		if errors.Is(err, ErrNotFound) {
			problem(w, 404, "operation_not_found")
			return
		}
		if err != nil {
			problem(w, 409, "reconcile_unavailable")
			return
		}
		reply(w, 200, o)
	})
	mux.HandleFunc("POST /v1/control/pause", func(w http.ResponseWriter, r *http.Request) {
		if err := e.Pause(); err != nil {
			problem(w, 503, "storage_unavailable")
			return
		}
		reply(w, 200, map[string]bool{"paused": true})
	})
	mux.HandleFunc("POST /v1/control/resume", func(w http.ResponseWriter, r *http.Request) {
		if err := e.Resume(r.Context()); err != nil {
			reason := "resume_check_failed"
			switch err.Error() {
			case "session_check_failed", "baseline_not_ready", "uncertain_operations_reconcile_required":
				reason = err.Error()
			}
			problem(w, 409, reason)
			return
		}
		reply(w, 200, map[string]bool{"paused": false})
	})
	mux.HandleFunc("POST /v1/stickers/import", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SourceUserID string `json:"sourceUserId"`
			MessageID    string `json:"messageId"`
			Name         string `json:"name"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF || !numeric(body.SourceUserID) || !numeric(body.MessageID) || strings.TrimSpace(body.Name) == "" || len(body.Name) > 120 {
			problem(w, 400, "invalid_sticker_import")
			return
		}
		api, ok := e.API.(interface {
			ImportSticker(context.Context, string, string, string) (Sticker, error)
		})
		if !ok {
			problem(w, 501, "sticker_import_unavailable")
			return
		}
		v, err := api.ImportSticker(r.Context(), body.SourceUserID, body.MessageID, body.Name)
		if errors.Is(err, ErrConflict) {
			problem(w, 409, "sticker_id_conflict")
			return
		}
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrConversationNotFound) {
			problem(w, 404, "sticker_not_found")
			return
		}
		if err != nil {
			problem(w, 503, "sticker_import_failed")
			return
		}
		reply(w, 200, v)
	})
	mux.HandleFunc("GET /v1/stickers", func(w http.ResponseWriter, r *http.Request) {
		v, err := e.API.Stickers(r.Context())
		if err != nil {
			problem(w, 503, "sticker_catalog_unavailable")
			return
		}
		reply(w, 200, map[string]any{"stickers": v})
	})
	mux.HandleFunc("GET /v1/stickers/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !numeric(id) {
			problem(w, 400, "invalid_sticker_id")
			return
		}
		b, mime, err := e.API.Preview(r.Context(), id)
		if errors.Is(err, ErrNotFound) {
			problem(w, 404, "sticker_not_found")
			return
		}
		if err != nil {
			problem(w, 503, "preview_unavailable")
			return
		}
		w.Header().Set("Content-Type", mime)
		w.Header().Set("Cache-Control", "private, no-store")
		w.Write(b)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				problem(w, 401, "unauthorized")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
