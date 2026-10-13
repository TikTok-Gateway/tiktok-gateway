package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	g "tiktok-chat-bot/internal/greeter"
	"time"
)

// Mock never contacts TikTok. Optional fixture files let isolated smoke tests inject followers.
type Mock struct{ EventsFile string }

func (m *Mock) Check(context.Context) error { return nil }
func (m *Mock) Events(context.Context, time.Time) ([]g.Event, error) {
	if m.EventsFile == "" {
		return nil, nil
	}
	b, err := os.ReadFile(m.EventsFile)
	if err != nil {
		return nil, err
	}
	var events []g.Event
	err = json.Unmarshal(b, &events)
	return events, err
}
func (m *Mock) Profile(context.Context, string) (g.Profile, error) { return g.Profile{}, nil }
func (m *Mock) Prepare(_ context.Context, id string) (g.Conversation, error) {
	return g.Conversation{ID: id, Source: 1}, nil
}
func (m *Mock) ReadyStickerID(_ context.Context, id string) error {
	if id != "1" {
		return errors.New("source_missing")
	}
	return nil
}
func (m *Mock) Send(context.Context, g.Conversation, string, string) (string, error) {
	return "mock-ack", nil
}
func (m *Mock) Verify(_ context.Context, o Operation) (bool, error) {
	return o.MessageID == "mock-ack", nil
}
func (m *Mock) Stickers(context.Context) ([]Sticker, error) {
	return []Sticker{{"1", "/v1/stickers/1/preview"}}, nil
}
func (m *Mock) Preview(context.Context, string) ([]byte, string, error) { return nil, "", ErrNotFound }
