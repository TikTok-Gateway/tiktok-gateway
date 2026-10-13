package gateway

import (
	"context"
	"database/sql"
	"errors"
	tt "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok"
	"net/http"
	"strconv"
	"strings"
)

// Catalogue rows contain only media and reusable attachment metadata. They are
// account-bound through Store, and survive source-chat deletion and restarts.
type CatalogSticker struct {
	ID         string
	Name       string
	Attachment []byte
	Media      []byte
	MIME       string
}

func (s *Store) ImportSticker(v CatalogSticker) error {
	if !numeric(v.ID) || len(v.Name) > 120 || strings.TrimSpace(v.Name) == "" || len(v.Media) == 0 || len(v.Media) > 8*1024*1024 {
		return errors.New("invalid_sticker")
	}
	attachment, err := tt.StickerTemplate(v.Attachment)
	if err != nil {
		return errors.New("invalid_sticker")
	}
	mime := http.DetectContentType(v.Media)
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return errors.New("invalid_sticker_media")
	}
	old, err := s.CatalogSticker(v.ID)
	if err == nil {
		if hash(old.Attachment) == hash(attachment) && hash(old.Media) == hash(v.Media) && old.Name == v.Name {
			return nil
		}
		return ErrConflict
	}
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO stickers(id,name,attachment,media,mime) VALUES(?,?,?,?,?)`, v.ID, v.Name, attachment, v.Media, mime)
	return err
}
func (s *Store) CatalogSticker(id string) (CatalogSticker, error) {
	v := CatalogSticker{ID: id}
	err := s.db.QueryRow(`SELECT name,attachment,media,mime FROM stickers WHERE id=?`, id).Scan(&v.Name, &v.Attachment, &v.Media, &v.MIME)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return v, err
}
func (s *Store) CatalogStickers() ([]Sticker, error) {
	rows, err := s.db.Query(`SELECT id,name FROM stickers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sticker{}
	for rows.Next() {
		var v Sticker
		if err := rows.Scan(&v.ID, &v.Name); err != nil {
			return nil, err
		}
		v.Preview = "/v1/stickers/" + v.ID + "/preview"
		out = append(out, v)
	}
	return out, rows.Err()
}

// ImportSticker copies one owned sticker from an existing conversation. It does
// not create a chat or send anything; no source relationship is retained.
func (t *TikTok) ImportSticker(ctx context.Context, userID, messageID, name string) (Sticker, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.Catalog == nil {
		return Sticker{}, errors.New("catalog_unavailable")
	}
	if saved, err := t.Catalog.CatalogSticker(messageID); err == nil {
		if saved.Name != name {
			return Sticker{}, ErrConflict
		}
		return Sticker{ID: messageID, Name: saved.Name, Preview: "/v1/stickers/" + messageID + "/preview"}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Sticker{}, err
	}
	if _, err := t.refreshConversations(ctx); err != nil {
		return Sticker{}, err
	}
	conv, ok := t.historyRoutes[userID]
	if !ok {
		return Sticker{}, ErrConversationNotFound
	}
	cursor := ""
	for page := 0; page < 20; page++ {
		rows, next, err := t.Client.GetMessages(ctx, &conv, cursor)
		if err != nil {
			return Sticker{}, err
		}
		for _, m := range rows {
			if strconv.FormatUint(m.ServerID, 10) != messageID {
				continue
			}
			if m.SenderID != t.AccountID || m.ConvID != conv.ID || m.MessageSubtype != "sticker" {
				return Sticker{}, errors.New("sticker_not_owned")
			}
			mediaURL, err := tt.StickerMediaURL(m.RawAttachment)
			if err != nil {
				return Sticker{}, err
			}
			media, _, err := downloadSticker(ctx, mediaURL)
			if err != nil {
				return Sticker{}, err
			}
			if err = t.Catalog.ImportSticker(CatalogSticker{ID: messageID, Name: name, Attachment: m.RawAttachment, Media: media}); err != nil {
				return Sticker{}, err
			}
			return Sticker{ID: messageID, Name: name, Preview: "/v1/stickers/" + messageID + "/preview"}, nil
		}
		if next == "" || next == cursor {
			break
		}
		cursor = next
	}
	return Sticker{}, ErrNotFound
}
