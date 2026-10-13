package gateway

import (
	"bytes"
	"context"
	"errors"
	"google.golang.org/protobuf/encoding/protowire"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func stickerFixture() []byte {
	b := func(n protowire.Number, v []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, n, 2), v)
	}
	size := []byte{8, 16, 16, 16}
	asset := append(b(1, []byte("test/sticker-object")), b(2, []byte("https://p16.tiktokcdn.com/test?x-expires=1&x-signature=private"))...)
	asset = append(asset, b(5, size)...)
	return b(7, b(1, asset))
}
func TestCatalogSurvivesRestartAndDoesNotUseSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.db")
	s, err := Open(path, "100")
	if err != nil {
		t.Fatal(err)
	}
	media := []byte("GIF89a\x01\x00\x01\x00")
	v := CatalogSticker{ID: "123", Name: "Test sticker", Attachment: stickerFixture(), Media: media}
	if err := s.ImportSticker(v); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportSticker(v); err != nil {
		t.Fatal("idempotent import", err)
	}
	v.Name = "different"
	if !errors.Is(s.ImportSticker(v), ErrConflict) {
		t.Fatal("overwrote ID")
	}
	s.Close()
	s, err = Open(path, "100")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stored, err := s.CatalogSticker("123")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Attachment, []byte("https:")) || bytes.Contains(stored.Attachment, []byte("private")) {
		t.Fatal("persisted expiring URL")
	}
	api := &TikTok{Catalog: s} // nil Client: any source lookup would panic.
	if err := api.ReadyStickerID(context.Background(), "123"); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(api.ReadyStickerID(context.Background(), "999"), ErrNotFound) {
		t.Fatal("missing ID substituted")
	}
	rows, err := api.Stickers(context.Background())
	if err != nil || len(rows) != 1 || rows[0].Name != "Test sticker" {
		t.Fatal(rows, err)
	}
	engine := &Engine{Store: s, API: api}
	handler := engine.Handler("secret")
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest("GET", "/v1/stickers/123/preview", nil)
		if authorized {
			req.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if !authorized {
			if w.Code != http.StatusUnauthorized {
				t.Fatal(w.Code)
			}
		} else if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), media) || w.Header().Get("Content-Type") != "image/gif" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestCatalogRejectsInvalidAttachmentsAndMedia(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "gateway.db"), "100")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, v := range []CatalogSticker{
		{ID: "../1", Name: "test", Attachment: stickerFixture(), Media: []byte("GIF89a")},
		{ID: "1", Name: "test", Attachment: []byte{58, 2, 8, 1}, Media: []byte("GIF89a")},
		{ID: "1", Name: "test", Attachment: append(stickerFixture(), 10, 0), Media: []byte("GIF89a")},
		{ID: "1", Name: "test", Attachment: stickerFixture(), Media: []byte("<html>not image</html>")},
	} {
		if s.ImportSticker(v) == nil {
			t.Fatal("accepted invalid import")
		}
	}
}

type importFake struct {
	Mock
	calls int
}

func (f *importFake) ImportSticker(_ context.Context, user, message, name string) (Sticker, error) {
	f.calls++
	return Sticker{ID: message, Name: name, Preview: "/v1/stickers/" + message + "/preview"}, nil
}
func TestImportStickerHTTPValidationAndAuthentication(t *testing.T) {
	api := &importFake{}
	handler := (&Engine{API: api}).Handler("secret")
	for _, tc := range []struct {
		body string
		auth bool
		code int
	}{
		{`{"sourceUserId":"200","messageId":"123","name":"Test"}`, false, 401},
		{`{"sourceUserId":"200","messageId":"123","name":"Test","url":"https://other.invalid"}`, true, 400},
		{`{"sourceUserId":"200","messageId":"123","name":" "}`, true, 400},
		{`{"sourceUserId":"200","messageId":"123","name":"Test"} {}`, true, 400},
		{`{"sourceUserId":"200","messageId":"123","name":"Test"}`, true, 200},
	} {
		r := httptest.NewRequest("POST", "/v1/stickers/import", bytes.NewBufferString(tc.body))
		if tc.auth {
			r.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if api.calls != 1 {
		t.Fatal("invalid import reached adapter", api.calls)
	}
}
