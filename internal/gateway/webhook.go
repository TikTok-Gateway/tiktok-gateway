package gateway

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
)

type WebhookStatus struct {
	Pending   int    `json:"pending"`
	Blocked   int    `json:"blocked"`
	LastError string `json:"lastError,omitempty"`
}

func (s *Store) WebhookStatus() (WebhookStatus, error) {
	var v WebhookStatus
	err := s.db.QueryRow(`SELECT count(*),coalesce(sum(CASE WHEN error!='' THEN 1 ELSE 0 END),0),coalesce((SELECT error FROM outbox WHERE delivered=0 AND error!='' ORDER BY next_at DESC LIMIT 1),'') FROM outbox WHERE delivered=0`).Scan(&v.Pending, &v.Blocked, &v.LastError)
	return v, err
}

type Webhook struct {
	Store      *Store
	URL, Token string
	Client     *http.Client
}

func (w *Webhook) Deliver(ctx context.Context) error {
	var id string
	var body []byte
	var attempts int
	err := w.Store.db.QueryRow(`SELECT o.id,e.body,o.attempts FROM outbox o JOIN events e ON e.id=o.id WHERE o.delivered=0 AND o.next_at<=? ORDER BY o.next_at,o.id LIMIT 1`, time.Now().Unix()).Scan(&id, &body, &attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	reason := "webhook_not_configured"
	delivered := 0
	delay := time.Second * time.Duration(1<<min(attempts+1, 11))
	if delay > time.Hour {
		delay = time.Hour
	}
	if w.URL != "" {
		req, err := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(body))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+w.Token)
			req.Header.Set("X-Event-ID", id)
			client := w.Client
			if client == nil {
				client = &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			}
			resp, err := client.Do(req)
			reason = "webhook_network_error"
			if err == nil {
				io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				switch {
				case resp.StatusCode >= 200 && resp.StatusCode < 300:
					delivered = 1
					reason = ""
				case resp.StatusCode == 401 || resp.StatusCode == 403:
					reason = "webhook_authorization_error"
					delay = time.Hour
				case resp.StatusCode == 429:
					reason = "webhook_rate_limited"
					if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds > 0 {
						delay = max(delay, min(time.Duration(seconds)*time.Second, 24*time.Hour))
					}
				case resp.StatusCode >= 500:
					reason = "webhook_server_error"
				default:
					reason = "webhook_address_or_request_error"
					delay = time.Hour
				}
			}
		} else {
			reason = "webhook_address_error"
			delay = time.Hour
		}
	}
	_, err = w.Store.db.Exec(`UPDATE outbox SET attempts=attempts+1,next_at=?,delivered=?,error=? WHERE id=?`, time.Now().Add(delay).Unix(), delivered, reason, id)
	return err
}
