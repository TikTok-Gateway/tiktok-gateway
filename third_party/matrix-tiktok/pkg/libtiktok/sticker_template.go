package libtiktok

import (
	"errors"
	pb "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok/pb"
	"google.golang.org/protobuf/proto"
)

// StickerTemplate validates a reusable attachment and removes the expiring CDN
// URL. TikTok's stable asset path identifies the media independently of a chat.
func StickerTemplate(raw []byte) ([]byte, error) {
	if len(raw) > 65536 {
		return nil, errors.New("sticker_attachment_too_large")
	}
	if _, err := NewSticker(Message{MessageSubtype: "sticker", RawAttachment: raw}); err != nil {
		return nil, err
	}
	var p pb.MessageAttachmentPayload
	if err := proto.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("invalid_sticker_attachment")
	}
	a := p.GetSticker().GetAsset()
	if a == nil || a.GetPath() == "" || len(a.GetPath()) > 2048 || a.GetSize().GetWidth() == 0 || a.GetSize().GetHeight() == 0 {
		return nil, errors.New("invalid_sticker_asset")
	}
	a.Url = nil
	return proto.Marshal(&p)
}

// StickerMediaURL is used once during import, never as the catalogue's identity.
func StickerMediaURL(raw []byte) (string, error) {
	if _, err := StickerTemplate(raw); err != nil {
		return "", err
	}
	var p pb.MessageAttachmentPayload
	if err := proto.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	return p.GetSticker().GetAsset().GetUrl(), nil
}
