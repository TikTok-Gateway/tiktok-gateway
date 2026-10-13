package greeter

import (
	"context"
	"errors"
	"fmt"
	tt "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok"
	"os"
	"strconv"
	"time"
)

type TikTok struct {
	PausePath string
	Client    *tt.Client
	State     *State
	sticker   *tt.Sticker
	stickerAt time.Time
	stickerID uint64
}

func parseEvents(data map[string]any) ([]Event, bool, int64, error) {
	groups, ok := data["notice_lists"].([]any)
	if !ok {
		return nil, false, 0, fmt.Errorf("missing follower groups")
	}
	for _, v := range groups {
		g, ok := v.(map[string]any)
		if !ok {
			return nil, false, 0, fmt.Errorf("invalid follower group")
		}
		if g["group"] != float64(7) {
			continue
		}
		list, ok := g["notice_list"].([]any)
		if !ok && g["notice_list"] != nil {
			return nil, false, 0, fmt.Errorf("invalid follower list")
		}
		more, ok := g["has_more"].(float64)
		if !ok {
			return nil, false, 0, fmt.Errorf("missing pagination flag")
		}
		var out []Event
		var oldest int64
		for _, item := range list {
			n, ok := item.(map[string]any)
			if !ok {
				return nil, false, 0, fmt.Errorf("invalid notice")
			}
			t, ok := n["create_time"].(float64)
			if !ok || t <= 0 || t != float64(int64(t)) {
				return nil, false, 0, fmt.Errorf("invalid timestamp")
			}
			if oldest == 0 || int64(t) < oldest {
				oldest = int64(t)
			}
			f, ok := n["follow"].(map[string]any)
			if !ok {
				continue
			}
			u, ok := f["from_user"].(map[string]any)
			if !ok {
				return nil, false, 0, fmt.Errorf("invalid follower")
			}
			id, _ := n["nid_str"].(string)
			uid, _ := u["uid"].(string)
			name, _ := u["unique_id"].(string)
			numeric, err := strconv.ParseUint(uid, 10, 64)
			if id == "" || err != nil || numeric == 0 {
				return nil, false, 0, fmt.Errorf("invalid follower identifiers")
			}
			out = append(out, Event{id, uid, name, int64(t)})
		}
		return out, more != 0, oldest, nil
	}
	return nil, false, 0, fmt.Errorf("follower group missing")
}
func (t *TikTok) Events(ctx context.Context, after time.Time) ([]Event, error) {
	var all []Event
	var maxTime int64
	seen := map[string]bool{}
	for page := 0; page < 20; page++ {
		raw, err := t.Client.GetFollowerNoticesPage(ctx, maxTime)
		if err != nil {
			return nil, err
		}
		events, more, oldest, err := parseEvents(raw)
		if err != nil {
			return nil, err
		}
		added := 0
		for _, v := range events {
			if !seen[v.ID] {
				seen[v.ID] = true
				all = append(all, v)
				added++
			}
		}
		// Initial baseline needs only the latest snapshot. Older events are excluded by Started.
		if after.IsZero() || !more || (oldest > 0 && oldest < after.Unix()) {
			return all, nil
		}
		if oldest == 0 || added == 0 || (maxTime != 0 && oldest >= maxTime) {
			return nil, ErrIncomplete
		}
		// Include the boundary second. Dedupe IDs; if pagination cannot advance, stop.
		maxTime = oldest
	}
	return nil, ErrIncomplete
}
func (t *TikTok) Profile(ctx context.Context, id string) (Profile, error) {
	p, e := t.Client.MessagingProfile(ctx, id)
	return Profile{p.FollowsSelf, p.Blocked, p.Deleted}, e
}
func (t *TikTok) Prepare(ctx context.Context, id string) (Conversation, error) {
	c, e := t.Client.PrepareDirectConversation(ctx, t.State.SelfID, id)
	if e != nil {
		var denied *tt.MessagingDenied
		if errors.As(e, &denied) {
			return Conversation{}, classifyDenied(denied)
		}
		return Conversation{}, e
	}
	return Conversation{c.ID, c.SourceID}, nil
}
func asConversation(c Conversation) *tt.Conversation {
	return &tt.Conversation{ID: c.ID, SourceID: c.Source, ConversationType: 1}
}
func (t *TikTok) History(ctx context.Context, c Conversation) ([]Message, error) {
	rows, _, e := t.Client.GetMessages(ctx, asConversation(c), "")
	if e != nil {
		return nil, e
	}
	out := make([]Message, 0, len(rows))
	for _, m := range rows {
		out = append(out, Message{strconv.FormatUint(m.ServerID, 10), m.SenderID, m.Text, m.MessageSubtype})
	}
	return out, nil
}
func (t *TikTok) ReadySticker(ctx context.Context) error {
	return t.ReadyStickerID(ctx, "")
}
func (t *TikTok) ReadyStickerID(ctx context.Context, selected string) error {
	id := t.State.StickerMessage
	if selected != "" {
		var err error
		id, err = strconv.ParseUint(selected, 10, 64)
		if err != nil || id == 0 {
			return fmt.Errorf("invalid sticker")
		}
	}
	if t.sticker != nil && t.stickerID == id && time.Since(t.stickerAt) < 30*time.Minute {
		return nil
	}
	cursor := ""
	for page := 0; page < 20; page++ {
		rows, next, e := t.Client.GetMessages(ctx, asConversation(t.State.StickerConversation), cursor)
		if e != nil {
			return e
		}
		for _, m := range rows {
			if m.ServerID == id && m.SenderID == t.State.SelfID {
				sticker, e := tt.NewSticker(m)
				if e != nil {
					return e
				}
				t.sticker = sticker
				t.stickerID = id
				t.stickerAt = time.Now()
				return nil
			}
		}
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return fmt.Errorf("original sticker missing")
}
func (t *TikTok) Send(ctx context.Context, c Conversation, kind, text string) (string, error) {
	if t.PausePath != "" {
		if _, e := os.Stat(t.PausePath); e == nil {
			return "", fmt.Errorf("manual pause")
		}
	}
	p := tt.SendMessageParams{ConvID: c.ID, ConvSourceID: c.Source}
	switch kind {
	case "text":
		p.Text = text
	case "sticker":
		if t.sticker == nil {
			return "", fmt.Errorf("sticker unavailable")
		}
		p.Sticker = t.sticker
	default:
		return "", fmt.Errorf("invalid message kind")
	}
	result, e := t.Client.SendMessage(ctx, p)
	if e != nil {
		var denied *tt.MessagingDenied
		if errors.As(e, &denied) {
			return "", classifyDenied(denied)
		}
		return "", e
	}
	if result == nil {
		return "", fmt.Errorf("missing send result")
	}
	return result.MessageID, nil
}

// Explicit recipient-side codes in the TikTok web SDK. Unknown codes never
// become permission to continue. Sender restriction codes intentionally stop.
// Preserve numerical diagnostics; never retain the raw API response or message.
type APIRejection struct {
	Status, Check, BusinessCode uint64
	Cause                       error
}

func (e *APIRejection) Error() string {
	return fmt.Sprintf("messaging rejected: status=%d check=%d business=%d", e.Status, e.Check, e.BusinessCode)
}
func (e *APIRejection) Unwrap() error { return e.Cause }
func classifyDenied(e *tt.MessagingDenied) error {
	cause := ErrDenied
	// Prefer the detailed business code. The small status/check values describe
	// the generic protocol rejection, not which user's permissions caused it.
	codes := []uint64{e.BusinessCode}
	if e.BusinessCode == 0 {
		codes = []uint64{e.Check, e.Status}
	}
	for _, code := range codes {
		switch code {
		case 7281, 7282, 7283, 7290, 7291, 7394:
			cause = ErrRecipientUnavailable
		}
	}
	// Observed with status=3/check=2 for recipients whose TikTok UI disallows
	// messaging (2026-10-14). Treat only the detailed code as recipient-scoped;
	// this does not establish which privacy/eligibility setting caused it.
	if e.BusinessCode == 7202 || e.BusinessCode == 7409 {
		cause = ErrRecipientUnavailable
	}
	// A conflicting explicit sender restriction always wins.
	for _, code := range []uint64{e.Status, e.Check, e.BusinessCode} {
		switch code {
		case 7284, 7289, 7393, 7170, 7195, 7179:
			cause = ErrDenied
		}
	}
	return &APIRejection{Status: e.Status, Check: e.Check, BusinessCode: e.BusinessCode, Cause: cause}
}

// StickerChoices reads only the configured source conversation; it never sends.
type StickerChoice struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Default bool   `json:"default"`
}

func (t *TikTok) StickerChoices(ctx context.Context) ([]StickerChoice, error) {
	out := []StickerChoice{}
	seen := map[uint64]bool{}
	cursor := ""
	for page := 0; page < 20; page++ {
		rows, next, err := t.Client.GetMessages(ctx, asConversation(t.State.StickerConversation), cursor)
		if err != nil {
			return nil, err
		}
		for _, m := range rows {
			if m.SenderID != t.State.SelfID || seen[m.ServerID] {
				continue
			}
			if _, err := tt.NewSticker(m); err != nil {
				continue
			}
			seen[m.ServerID] = true
			out = append(out, StickerChoice{strconv.FormatUint(m.ServerID, 10), m.MediaURL, m.ServerID == t.State.StickerMessage})
		}
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return out, nil
}

// SetSticker supplies an already validated catalogue template for the next send.
func (t *TikTok) SetSticker(sticker *tt.Sticker) { t.sticker = sticker }
