package greeter

import (
	"context"
	"errors"
	"time"
)

// One block per worker tick. Pauses persist a deadline instead of blocking other jobs.
func (e *Engine) workFlow(ctx context.Context, j *Job) error {
	blocks := j.Settings.Blocks
	if j.Step >= len(blocks) {
		return e.change(j, "sent", "")
	}
	if e.Now().Sub(j.Due) > time.Hour {
		return e.change(j, "skipped", "stale_after_downtime")
	}
	b := blocks[j.Step]
	if b.Type == "pause" {
		d, err := (Settings{DelayMin: b.Min, DelayMax: b.Max}).Delay()
		if err != nil {
			return err
		}
		j.Results = append(j.Results, StepResult{j.Step, b.Type, e.Now().UTC()})
		j.Step++
		j.Due = e.Now().UTC().Add(d)
		return e.change(j, "queued", "")
	}
	p, err := e.API.Profile(ctx, j.Event.UserID)
	if err != nil {
		return e.pause("profile_check_failed")
	}
	if !p.Follows || p.Blocked || p.Deleted {
		return e.change(j, "skipped", "not_following_blocked_or_deleted")
	}
	conv, err := e.API.Prepare(ctx, j.Event.UserID)
	if err != nil {
		if errors.Is(err, ErrRecipientUnavailable) {
			return e.change(j, "skipped", "recipient_unavailable")
		}
		if errors.Is(err, ErrDenied) {
			return e.pause("tiktok_rejected_conversation")
		}
		return e.pause("conversation_preflight_failed")
	}
	j.Conversation = conv
	history, err := e.API.History(ctx, conv)
	if err != nil {
		return e.pause("history_preflight_failed")
	}
	sent := false
	for _, r := range j.Results {
		if r.Type != "pause" {
			sent = true
		}
	}
	if !sent && b.Type == "message" {
		for _, m := range history {
			if m.Sender == e.State.SelfID && m.Text == b.Text {
				return e.change(j, "skipped", "greeting_already_exists")
			}
		}
	}
	kind := "text"
	if b.Type == "sticker" {
		kind = "sticker"
		if b.StickerID != "" {
			if selector, ok := e.API.(interface {
				ReadyStickerID(context.Context, string) error
			}); ok {
				err = selector.ReadyStickerID(ctx, b.StickerID)
			} else {
				err = errors.New("sticker selection unavailable")
			}
		} else {
			err = e.API.ReadySticker(ctx)
		}
		if err != nil {
			return e.pause("sticker_source_unavailable")
		}
	}
	return e.send(ctx, j, kind)
}
