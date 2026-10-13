package gateway

import (
	"context"
	"errors"
	tt "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	g "tiktok-chat-bot/internal/greeter"
	"time"
)

// Reuse the existing protocol implementation; serialize its mutable session and sticker cache.
type TikTok struct {
	mu                                 sync.Mutex
	Client                             *tt.Client
	AccountID, Username, StickerSource string
	adapter                            *g.TikTok
	historyRoutes                      map[string]tt.Conversation
	historyRoutesAt                    time.Time
}

func (t *TikTok) base() *g.TikTok {
	if t.adapter == nil {
		t.adapter = &g.TikTok{Client: t.Client, State: &g.State{SelfID: t.AccountID}}
	}
	return t.adapter
}
func (t *TikTok) Check(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, err := t.Client.GetSelfFresh(ctx)
	if err != nil || s == nil || s.UserID != t.AccountID || (t.Username != "" && s.UniqueID != t.Username) {
		return errors.New("session_check_failed")
	}
	return nil
}
func (t *TikTok) Events(ctx context.Context, after time.Time) ([]g.Event, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.base().Events(ctx, after)
}
func (t *TikTok) Profile(ctx context.Context, id string) (g.Profile, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.base().Profile(ctx, id)
}
func (t *TikTok) Prepare(ctx context.Context, id string) (g.Conversation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.base().Prepare(ctx, id)
}
func (t *TikTok) source(ctx context.Context) (*tt.Conversation, error) {
	if t.StickerSource == "" {
		return nil, errors.New("sticker_source_not_configured")
	}
	return t.Client.PrepareDirectConversation(ctx, t.AccountID, t.StickerSource)
}
func (t *TikTok) ReadyStickerID(ctx context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	c, err := t.source(ctx)
	if err != nil {
		return err
	}
	// A new adapter deliberately bypasses the legacy 30-minute cache: selected source must still exist.
	t.adapter = &g.TikTok{Client: t.Client, State: &g.State{SelfID: t.AccountID, StickerConversation: g.Conversation{ID: c.ID, Source: c.SourceID}}}
	return t.adapter.ReadyStickerID(ctx, id)
}
func (t *TikTok) Send(ctx context.Context, c g.Conversation, kind, text string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.base().Send(ctx, c, kind, text)
}
func (t *TikTok) Verify(ctx context.Context, o Operation) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cursor := ""
	conv := &tt.Conversation{ID: o.Conversation.ID, SourceID: o.Conversation.Source, ConversationType: 1}
	for page := 0; page < 20; page++ {
		rows, next, err := t.Client.GetMessages(ctx, conv, cursor)
		if err != nil {
			return false, err
		}
		for _, m := range rows {
			if strconv.FormatUint(m.ServerID, 10) == o.MessageID && m.SenderID == t.AccountID && ((o.Command.Type == "text" && m.Text == o.Command.Text) || (o.Command.Type == "sticker" && m.MessageSubtype == "sticker")) {
				return true, nil
			}
		}
		if next == "" || next == cursor {
			return false, nil
		}
		cursor = next
	}
	return false, nil
}
func (t *TikTok) choices(ctx context.Context) ([]g.StickerChoice, error) {
	c, err := t.source(ctx)
	if err != nil {
		return nil, err
	}
	a := &g.TikTok{Client: t.Client, State: &g.State{SelfID: t.AccountID, StickerConversation: g.Conversation{ID: c.ID, Source: c.SourceID}}}
	return a.StickerChoices(ctx)
}
func (t *TikTok) Stickers(ctx context.Context) ([]Sticker, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	choices, err := t.choices(ctx)
	if err != nil {
		return nil, err
	}
	out := []Sticker{}
	for _, s := range choices {
		out = append(out, Sticker{s.ID, "/v1/stickers/" + s.ID + "/preview"})
	}
	return out, nil
}
func (t *TikTok) Preview(ctx context.Context, id string) ([]byte, string, error) {
	t.mu.Lock()
	choices, err := t.choices(ctx)
	t.mu.Unlock()
	if err != nil {
		return nil, "", err
	}
	media := ""
	for _, s := range choices {
		if s.ID == id {
			media = s.URL
			break
		}
	}
	if media == "" {
		return nil, "", ErrNotFound
	}
	u, err := url.Parse(media)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return nil, "", errors.New("invalid_media_url")
	}
	host := strings.ToLower(u.Hostname())
	allowed := false
	for _, suffix := range []string{".tiktokcdn.com", ".tiktokcdn-us.com", ".tiktokcdn-eu.com", ".byteoversea.com", ".ibytedtos.com", ".ibyteimg.com", ".byteimg.com", ".tiktok.com"} {
		if strings.HasSuffix(host, suffix) {
			allowed = true
		}
	}
	if !allowed {
		return nil, "", errors.New("unsupported_media_host")
	}
	// Never attach account cookies to a CDN request, nor follow redirects.
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, "GET", media, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", errors.New("preview_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", errors.New("preview_unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
	if err != nil || len(b) > 8*1024*1024 {
		return nil, "", errors.New("invalid_preview")
	}
	mime := http.DetectContentType(b)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return b, mime, nil
	}
	return nil, "", errors.New("unsupported_preview")
}
