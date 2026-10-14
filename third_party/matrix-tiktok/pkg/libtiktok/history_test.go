package libtiktok

import (
	"context"
	pb "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok/pb"
	"google.golang.org/protobuf/encoding/protowire"
	"testing"
)

func historyBody(t *testing.T, entries ...*pb.ConversationMessageEntry) []byte {
	t.Helper()
	list := &pb.ConversationMessageList{Entries: entries}
	list.ProtoReflect().SetUnknown(wInt(wInt(nil, 2, 50), 3, 0))
	b, e := marshalProto(&pb.GetByConversationResponse{Payload: &pb.GetByConversationResponsePayload{Conversation: list}})
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
func TestStrictHistoryReportsHiddenRowsAndUsesResponseCursor(t *testing.T) {
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
	if err != nil || page.HasGaps || page.Exhausted || page.NextCursor != "50" {
		t.Fatalf("entry predecessor is not required for pagination: %+v %v", page, err)
	}
}

func paginationBody(t *testing.T, metadata []byte, entries ...*pb.ConversationMessageEntry) []byte {
	t.Helper()
	list := &pb.ConversationMessageList{Entries: entries}
	list.ProtoReflect().SetUnknown(metadata)
	b, err := marshalProto(&pb.GetByConversationResponse{Payload: &pb.GetByConversationResponsePayload{Conversation: list}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestServerCursorWithNegativePredecessorStillRequiresEmptyTerminalPage(t *testing.T) {
	row := &pb.ConversationMessageEntry{ConversationId: protoString("0:1:100:200"), ServerMessageId: protoUint64(20), SenderUserId: protoUint64(100), TimestampUs: protoUint64(1000000), CursorTsUs: protoUint64(^uint64(0)), ContentJson: []byte(`{"aweType":0,"text":"welcome"}`)}
	raw := paginationBody(t, wInt(wInt(nil, 2, 1000000), 3, 0), row)
	p, err := parseHistoryPage(context.Background(), nil, raw)
	if err != nil || p.Exhausted || p.HasGaps || p.NextCursor != "1000000" || len(p.Messages) != 1 {
		t.Fatal(p, err)
	}
	_, next, err := parseGetByConversationResponse(context.Background(), nil, raw)
	if err != nil || next != "1000000" {
		t.Fatal("legacy path still uses predecessor", next, err)
	}
	p, err = parseHistoryPage(context.Background(), nil, paginationBody(t, wInt(wInt(nil, 2, 1000000), 3, 0)))
	if err != nil || !p.Exhausted || p.HasGaps || p.NextCursor != "" {
		t.Fatal("explicit terminal empty page", p, err)
	}
	row.Tags = []*pb.MetadataTag{{Key: protoString("s:is_recalled"), Value: []byte("1")}}
	p, err = parseHistoryPage(context.Background(), nil, paginationBody(t, wInt(wInt(nil, 2, 1000000), 3, 0), row))
	if err != nil || !p.HasGaps || p.Exhausted {
		t.Fatal("terminal flag hid a recalled row", p, err)
	}
}
func TestPaginationMetadataFailsClosed(t *testing.T) {
	valid := wInt(wInt(nil, 2, 50), 3, 0)
	for _, meta := range [][]byte{nil, wInt(nil, 2, 50), wInt(nil, 3, 0), wInt(wInt(nil, 2, ^uint64(0)), 3, 0), wInt(wInt(nil, 2, 50), 3, 2), wInt(valid, 2, 20), wInt(valid, 3, 1), append(wBytes(nil, 2, []byte("50")), wInt(nil, 3, 0)...), wBytes(valid, 4, []byte{1})} {
		if _, err := parseHistoryPage(context.Background(), nil, paginationBody(t, meta)); err == nil {
			t.Fatalf("accepted invalid metadata: %x", meta)
		}
	}
	p, err := parseHistoryPage(context.Background(), nil, paginationBody(t, wInt(wInt(nil, 2, 50), 3, 1)))
	if err != nil || p.Exhausted || !p.HasGaps || p.NextCursor != "50" {
		t.Fatal("empty nonterminal page marked complete", p, err)
	}
}
