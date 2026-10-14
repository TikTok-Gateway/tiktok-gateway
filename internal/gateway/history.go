package gateway

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	tt "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok"
)

var ErrConversationNotFound = errors.New("conversation_not_found")

type DirectConversation struct {
	ID     string `json:"conversationId"`
	UserID string `json:"userId"`
}
type Message struct {
	ID        string    `json:"messageId"`
	SenderID  string    `json:"senderId"`
	Direction string    `json:"direction"`
	Type      string    `json:"type"`
	Text      string    `json:"text"`
	SentAt    time.Time `json:"sentAt"`
	ReplyTo   string    `json:"replyToMessageId,omitempty"`
}
type MessagePage struct {
	AccountID      string    `json:"accountId"`
	ConversationID string    `json:"conversationId"`
	UserID         string    `json:"userId"`
	Messages       []Message `json:"messages"`
	NextCursor     string    `json:"nextCursor"`
	Exhausted      bool      `json:"exhausted"`
	HasGaps        bool      `json:"hasGaps"`
}

// MessagingAPI is transport only: no welcome text, dialog phases or AI decisions.
type MessagingAPI interface {
	Conversations(context.Context) ([]DirectConversation, error)
	Messages(context.Context, string, string) (MessagePage, error)
}

func (t *TikTok) refreshConversations(ctx context.Context) ([]DirectConversation, error) {
	rows, err := t.Client.GetInbox(ctx)
	if err != nil {
		return nil, err
	}
	routes := map[string]tt.Conversation{}
	out := []DirectConversation{}
	for _, c := range rows {
		if c.ConversationType != 1 || c.SourceID == 0 || len(c.Participants) != 2 {
			continue
		}
		peer := ""
		if c.Participants[0] == t.AccountID {
			peer = c.Participants[1]
		} else if c.Participants[1] == t.AccountID {
			peer = c.Participants[0]
		}
		if !numeric(peer) || peer == t.AccountID {
			continue
		}
		if _, exists := routes[peer]; exists {
			return nil, errors.New("ambiguous_direct_conversation")
		}
		routes[peer] = c
		out = append(out, DirectConversation{c.ID, peer})
	}
	t.historyRoutes, t.historyRoutesAt = routes, time.Now()
	return out, nil
}
func (t *TikTok) Conversations(ctx context.Context) ([]DirectConversation, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.refreshConversations(ctx)
}
func (t *TikTok) Messages(ctx context.Context, userID, cursor string) (MessagePage, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.historyRoutes == nil || time.Since(t.historyRoutesAt) > time.Minute {
		if _, err := t.refreshConversations(ctx); err != nil {
			return MessagePage{}, err
		}
	}
	conv, ok := t.historyRoutes[userID]
	if !ok {
		return MessagePage{}, ErrConversationNotFound
	}
	// Do not use PrepareDirectConversation here: that method creates a conversation.
	page, err := t.Client.GetHistoryPage(ctx, &conv, cursor, t.AccountID)
	if err != nil {
		return MessagePage{}, err
	}
	out := MessagePage{AccountID: t.AccountID, ConversationID: conv.ID, UserID: userID, Messages: []Message{}, NextCursor: page.NextCursor, Exhausted: page.Exhausted, HasGaps: page.HasGaps}
	for _, m := range page.Messages {
		if m.ConvID != conv.ID || m.ServerID == 0 || (m.SenderID != userID && m.SenderID != t.AccountID) || m.TimestampUs == 0 || m.TimestampUs > uint64(1<<63-1) {
			return MessagePage{}, errors.New("invalid_history_message")
		}
		direction := "incoming"
		if m.SenderID == t.AccountID {
			direction = "outgoing"
		}
		kind := m.Type
		switch kind {
		case "text", "image", "video", "sticker":
		default:
			kind = "unknown"
			out.HasGaps = true
		}
		msg := Message{ID: strconv.FormatUint(m.ServerID, 10), SenderID: m.SenderID, Direction: direction, Type: kind, Text: m.Text, SentAt: time.UnixMicro(int64(m.TimestampUs)).UTC()}
		if m.ReplyToServerID != 0 {
			msg.ReplyTo = strconv.FormatUint(m.ReplyToServerID, 10)
		}
		out.Messages = append(out.Messages, msg)
	}
	sort.Slice(out.Messages, func(i, j int) bool {
		a, b := out.Messages[i], out.Messages[j]
		if a.SentAt.Equal(b.SentAt) {
			ai, _ := strconv.ParseUint(a.ID, 10, 64)
			bi, _ := strconv.ParseUint(b.ID, 10, 64)
			return ai < bi
		}
		return a.SentAt.Before(b.SentAt)
	})
	return out, nil
}
