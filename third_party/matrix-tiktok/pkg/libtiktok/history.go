package libtiktok

import (
	"context"
	"errors"
	"strconv"

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

func (c *Client) GetHistoryPage(ctx context.Context, conv *Conversation, cursor string) (HistoryPage, error) {
	body, err := c.fetchMessages(ctx, conv, cursor)
	if err != nil {
		return HistoryPage{}, err
	}
	return parseHistoryPage(ctx, c, body)
}

func parseHistoryPage(ctx context.Context, c *Client, body []byte) (HistoryPage, error) {
	var resp pb.GetByConversationResponse
	code, err := wireInt(body, 3)
	if err != nil || code != 0 || unmarshalProto(body, &resp) != nil || resp.GetPayload().GetConversation() == nil {
		return HistoryPage{}, errors.New("invalid_history_response")
	}
	entries := resp.GetPayload().GetConversation().GetEntries()
	page := HistoryPage{Messages: []Message{}, Exhausted: len(entries) == 0}
	var min uint64
	for _, entry := range entries {
		m, cursor, err := parseMessageEntry(ctx, c, entry)
		if cursor == 0 {
			page.HasGaps = true
		} else if min == 0 || cursor < min {
			min = cursor
		}
		if err != nil {
			page.HasGaps = true
			continue
		}
		if m.ServerID == 0 || m.SenderID == "0" || m.TimestampUs == 0 || m.ConvID == "" || m.Type == "" {
			page.HasGaps = true
		}
		page.Messages = append(page.Messages, m)
	}
	if min != 0 {
		page.NextCursor = strconv.FormatUint(min, 10)
	}
	return page, nil
}
