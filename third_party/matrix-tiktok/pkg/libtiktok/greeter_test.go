package libtiktok

import (
	"errors"
	"google.golang.org/protobuf/encoding/protowire"
	"strings"
	"testing"
)

func TestSendChecksStatusBeforeMessageID(t *testing.T) {
	for _, v := range []struct{ status, check, id uint64 }{{3, 0, 1}, {0, 7283, 1}, {0, 0, 0}, {0, 0, 123}} {
		inner := wInt(nil, 1, v.id)
		inner = wInt(inner, 3, v.status)
		inner = wInt(inner, 5, v.check)
		body := wBytes(nil, 6, wBytes(nil, 100, inner))
		id, err := parseSendResponse(body)
		if v.status != 0 || v.check != 0 || v.id == 0 {
			if err == nil {
				t.Fatal("accepted rejected send", id)
			}
		} else if err != nil || id != "123" {
			t.Fatal(id, err)
		}
	}
	body := wInt(nil, 3, 7289)
	body = wBytes(body, 6, wBytes(nil, 100, wInt(nil, 1, 123)))
	if _, err := parseSendResponse(body); err == nil {
		t.Fatal("accepted envelope rejection")
	}
}
func TestStickerTemplateRejectsMixedAttachments(t *testing.T) {
	valid := wBytes(nil, 7, []byte{8, 1})
	if _, err := NewSticker(Message{MessageSubtype: "sticker", RawAttachment: valid}); err != nil {
		t.Fatal(err)
	}
	for _, b := range [][]byte{nil, wBytes(nil, 14, []byte{8, 1}), append(valid, wBytes(nil, 14, []byte{8, 1})...)} {
		if _, err := NewSticker(Message{MessageSubtype: "sticker", RawAttachment: b}); err == nil {
			t.Fatal("invalid sticker accepted")
		}
	}
}

func TestDetailedSendDenial(t *testing.T) {
	inner := wInt(wInt(wInt(nil, 1, 123), 3, 3), 5, 2)
	inner = wBytes(inner, 6, []byte(`{"status_code":7283,"status_msg":"private diagnostic"}`))
	id, err := parseSendResponse(wBytes(nil, 6, wBytes(nil, 100, inner)))
	var denied *MessagingDenied
	if id != "" || !errors.As(err, &denied) || denied.Status != 3 || denied.Check != 2 || denied.BusinessCode != 7283 {
		t.Fatalf("wrong denial: id=%q err=%v", id, err)
	}
	if strings.Contains(err.Error(), "private") {
		t.Fatal("leaked response text")
	}
}

func TestCheckMessageCodeValidation(t *testing.T) {
	for _, field := range []protowire.Number{3, 6} {
		for _, raw := range []string{"", "null", "[]", "not json", `{"status_code":"7283"}`, `{"status_code":-1}`, `{"status_code":7283.1}`, `{"status_code":18446744073709551616}`, `{"status_code":7283} trailing`, strings.Repeat(" ", 4097) + `{"status_code":7283}`} {
			if got := messagingDenial(3, 2, wBytes(nil, field, []byte(raw)), field); got.BusinessCode != 0 {
				t.Fatalf("accepted malformed detail: %q", raw)
			}
		}
		if got := messagingDenial(3, 2, wBytes(nil, field, []byte(`{"status_code":7289}`)), field); got.BusinessCode != 7289 {
			t.Fatal(got)
		}
	}
}
