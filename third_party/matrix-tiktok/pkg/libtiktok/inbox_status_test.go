package libtiktok

import (
	"errors"
	tiktokpb "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok/pb"
	"testing"
)

func TestInboxRejectsApplicationError(t *testing.T) {
	body, err := marshalProto(&tiktokpb.InboxResponse{Status: protoUint64(200005), Message: protoString("synthetic server error")})
	if err != nil {
		t.Fatal(err)
	}
	convs, err := parseInboxResponse(body)
	var status *InboxStatusError
	if !errors.As(err, &status) || status.Code != 200005 || len(convs) != 0 {
		t.Fatalf("must reject application error instead of returning an empty successful inbox")
	}
	if status.Category != "unclassified" {
		t.Fatal("unexpected category")
	}
}
