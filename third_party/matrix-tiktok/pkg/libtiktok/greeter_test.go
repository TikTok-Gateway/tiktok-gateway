package libtiktok

import "testing"

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
