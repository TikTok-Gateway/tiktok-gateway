package libtiktok

import (
	"context"
	"errors"

	pb "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok/pb"
)

// HistoryPage distinguishes an explicit empty page from a malformed response
// and reports rows hidden by the bridge parser. Exhaustion means only that the
// server returned no more currently visible history, not that nothing was deleted.
type HistoryPage struct {
	Messages   []Message
	NextCursor string
	Exhausted  bool
	HasGaps    bool
}

func (c *Client) GetHistoryPage(ctx context.Context, conv *Conversation, cursor string, viewerID ...string) (HistoryPage, error) {
	body, err := c.fetchMessages(ctx, conv, cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	return parseHistoryPage(ctx, c, body, viewerID...)
}

func parseHistoryPage(ctx context.Context, c *Client, body []byte, viewerID ...string) (HistoryPage, error) {
	var resp pb.GetByConversationResponse
	code, err := wireInt(body, 3)
	if err != nil || code != 0 || unmarshalProto(body, &resp) != nil || resp.GetPayload().GetConversation() == nil {
		return HistoryPage{}, errors.New("invalid_history_response")
	}
	list := resp.GetPayload().GetConversation()
	next, more, err := historyPagination(list)
	if err != nil {
		return HistoryPage{}, err
	}
	entries := list.GetEntries()
	// Preserve the gateway/n8n contract: even has_more=false on a nonempty
	// page requires one more request confirming an empty terminal page.
	page := HistoryPage{Messages: []Message{}, NextCursor: next, Exhausted: len(entries) == 0 && !more}
	if page.Exhausted {
		page.NextCursor = ""
	}
	if len(entries) == 0 && more {
		page.HasGaps = true
	}
	for _, entry := range entries {
		m, _, err := parseMessageEntry(ctx, c, entry, viewerID...)
		if err != nil {
			page.HasGaps = true
			continue
		}
		if m.ServerID == 0 || m.SenderID == "0" || m.TimestampUs == 0 || m.ConvID == "" || m.Type == "" {
			page.HasGaps = true
		}
		page.Messages = append(page.Messages, m)
	}
	return page, nil
}
