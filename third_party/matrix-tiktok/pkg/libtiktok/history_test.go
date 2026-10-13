package libtiktok

import (
	"context"
	pb "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok/pb"
	"google.golang.org/protobuf/encoding/protowire"
	"testing"
)

func historyBody(t *testing.T, entries ...*pb.ConversationMessageEntry) []byte {
	t.Helper()
	b, e := marshalProto(&pb.GetByConversationResponse{Payload: &pb.GetByConversationResponsePayload{Conversation: &pb.ConversationMessageList{Entries: entries}}})
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestStrictHistoryRequiresExplicitEmptyEnvelope(t *testing.T) {
	for _, body := range [][]byte{nil, {}, {0xff}} {
		if _, err := parseHistoryPage(context.Background(), nil, body); err == nil {
			t.Fatal("accepted malformed empty history")
		}
	}
	p, err := parseHistoryPage(context.Background(), nil, historyBody(t))
	if err != nil || !p.Exhausted || p.HasGaps {
		t.Fatalf("%+v %v", p, err)
	}
	body := historyBody(t)
	body = protowire.AppendTag(body, 3, protowire.VarintType)
	body = protowire.AppendVarint(body, 3)
	if _, err := parseHistoryPage(context.Background(), nil, body); err == nil {
		t.Fatal("accepted TikTok rejection")
	}
}
func TestStrictHistoryReportsHiddenRowsAndMissingCursor(t *testing.T) {
	row := &pb.ConversationMessageEntry{ConversationId: protoString("0:1:100:200"), ServerMessageId: protoUint64(20), SenderUserId: protoUint64(100), TimestampUs: protoUint64(1000000), CursorTsUs: protoUint64(50), ContentJson: []byte(`{"aweType":0,"text":"welcome"}`)}
	page, err := parseHistoryPage(context.Background(), nil, historyBody(t, row))
	if err != nil || page.Exhausted || page.HasGaps || page.NextCursor != "50" || len(page.Messages) != 1 {
		t.Fatalf("%+v %v", page, err)
	}
	row.Tags = []*pb.MetadataTag{{Key: protoString("s:is_recalled"), Value: []byte("1")}}
	page, err = parseHistoryPage(context.Background(), nil, historyBody(t, row))
	if err != nil || page.Exhausted || !page.HasGaps || page.NextCursor != "50" || len(page.Messages) != 0 {
		t.Fatalf("hidden row lost: %+v %v", page, err)
	}
	row.Tags = nil
	row.CursorTsUs = nil
	page, err = parseHistoryPage(context.Background(), nil, historyBody(t, row))
	if err != nil || !page.HasGaps || page.Exhausted {
		t.Fatalf("missing cursor: %+v %v", page, err)
	}
}
