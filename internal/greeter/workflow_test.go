package greeter

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkflowSequencePauseAndRestart(t *testing.T) {
	e, f, now := fixture(t)
	settings := Settings{PollSeconds: 45, Blocks: []Block{{Type: "trigger"}, {Type: "message", Text: "Первое"}, {Type: "pause", Min: 20, Max: 20}, {Type: "sticker"}, {Type: "message", Text: "Второе"}}}
	e.Settings = &settings
	enqueue(t, e, *now)
	settings.Blocks[1].Text = "Изменение будущих заданий"
	if err := e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.history[0].Text != "Первое" {
		t.Fatal("scenario was mutated")
	}
	if err := e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	j := e.State.Jobs["target"]
	if j.Step != 3 || j.Due.Sub(*now) != 20*time.Second {
		t.Fatal("pause not persisted")
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, e.State); err != nil {
		t.Fatal(err)
	}
	var err error
	e.State, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Recover(); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(19 * time.Second)
	e.Work(context.Background())
	if len(f.sent) != 1 {
		t.Fatal("sent during pause")
	}
	*now = now.Add(time.Second)
	e.Work(context.Background())
	e.Work(context.Background())
	j = e.State.Jobs["target"]
	if len(f.sent) != 3 || f.sent[1] != "sticker" || f.history[2].Text != "Второе" || j.Phase != "sent" || len(j.Results) != 4 {
		t.Fatal("wrong workflow result", j, f.history)
	}
	e.Work(context.Background())
	if len(f.sent) != 3 {
		t.Fatal("replayed completed workflow")
	}
}
func TestWorkflowTrailingPauseAndStickerFirst(t *testing.T) {
	e, f, now := fixture(t)
	settings := Settings{PollSeconds: 45, Blocks: []Block{{Type: "trigger"}, {Type: "sticker"}, {Type: "pause", Min: 10, Max: 10}}}
	e.Settings = &settings
	enqueue(t, e, *now)
	e.Work(context.Background())
	e.Work(context.Background())
	if e.State.Jobs["target"].Phase != "queued" || len(f.sent) != 1 {
		t.Fatal("trailing pause skipped")
	}
	*now = now.Add(10 * time.Second)
	e.Work(context.Background())
	if e.State.Jobs["target"].Phase != "sent" {
		t.Fatal("workflow not complete")
	}
}
func TestWorkflowUncertainSendNeverRepeats(t *testing.T) {
	e, f, now := fixture(t)
	settings := Settings{PollSeconds: 45, Blocks: []Block{{Type: "trigger"}, {Type: "message", Text: "Первое"}, {Type: "message", Text: "Второе"}}}
	e.Settings = &settings
	enqueue(t, e, *now)
	f.sendErr = errors.New("network interrupted")
	e.Work(context.Background())
	e.Recover()
	e.Work(context.Background())
	if len(f.sent) != 1 || e.State.Paused == "" || e.State.Jobs["target"].Step != 1 {
		t.Fatal("uncertain send advanced or repeated")
	}
}
func TestWorkflowValidation(t *testing.T) {
	for _, blocks := range [][]Block{
		{}, {{Type: "message", Text: "x"}}, {{Type: "trigger"}, {Type: "trigger"}}, {{Type: "trigger"}, {Type: "pause", Min: 0, Max: 1}}, {{Type: "trigger"}, {Type: "message", Text: " "}}, {{Type: "trigger"}, {Type: "unknown"}}, {{Type: "trigger"}, {Type: "pause", Min: 20, Max: 10}, {Type: "sticker"}}, {{Type: "trigger"}, {Type: "pause", Max: 1800}, {Type: "pause", Max: 1}, {Type: "sticker"}},
	} {
		if (Settings{PollSeconds: 45, Blocks: blocks}).Validate() == nil {
			t.Fatal("invalid workflow accepted", blocks)
		}
	}
	if err := (Settings{PollSeconds: 45, Blocks: []Block{{Type: "trigger"}, {Type: "sticker"}}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

type selectingAPI struct {
	*fakeAPI
	selected     []string
	selectionErr error
}

func (f *selectingAPI) ReadyStickerID(_ context.Context, id string) error {
	f.selected = append(f.selected, id)
	return f.selectionErr
}
func TestWorkflowUsesSavedStickerAndStopsOnMissingSource(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e, f, now := fixture(t)
		api := &selectingAPI{fakeAPI: f}
		e.API = api
		settings := Settings{PollSeconds: 45, Blocks: []Block{{Type: "trigger"}, {Type: "sticker", StickerID: "123"}}}
		e.Settings = &settings
		enqueue(t, e, *now)
		settings.Blocks[1].StickerID = "456"
		if fail {
			api.selectionErr = errors.New("missing sticker")
		}
		if err := e.Work(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.selected) != 1 || api.selected[0] != "123" {
			t.Fatal("job sticker mutated", api.selected)
		}
		if fail {
			if len(f.sent) != 0 || e.State.Paused != "sticker_source_unavailable" {
				t.Fatal("missing source sent")
			}
		} else if len(f.sent) != 1 {
			t.Fatal("selected sticker not sent")
		}
	}
}
