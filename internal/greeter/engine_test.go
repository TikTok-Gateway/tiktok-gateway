package greeter

import (
	"context"
	"errors"
	tt "github.com/httpjamesm/matrix-tiktok/pkg/libtiktok"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeAPI struct {
	profile                         Profile
	profileErr, sendErr, prepareErr error
	sent                            []string
	history                         []Message
	beforeSend                      func(string)
	profiles                        int
}

func (f *fakeAPI) Events(context.Context, time.Time) ([]Event, error) { return nil, nil }
func (f *fakeAPI) Profile(context.Context, string) (Profile, error) {
	f.profiles++
	return f.profile, f.profileErr
}
func (f *fakeAPI) Prepare(context.Context, string) (Conversation, error) {
	return Conversation{"conv", 1}, f.prepareErr
}
func (f *fakeAPI) History(context.Context, Conversation) ([]Message, error) { return f.history, nil }
func (f *fakeAPI) ReadySticker(context.Context) error                       { return nil }
func (f *fakeAPI) Send(_ context.Context, _ Conversation, kind, message string) (string, error) {
	if f.beforeSend != nil {
		f.beforeSend(kind)
	}
	f.sent = append(f.sent, kind)
	if f.sendErr != nil {
		return "", f.sendErr
	}
	id := kind + "-id"
	text := ""
	if kind == "text" {
		text = message
	}
	f.history = append(f.history, Message{id, "self", text, kind})
	return id, nil
}
func fixture(t *testing.T) (*Engine, *fakeAPI, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	s := &State{Version: 1, SelfID: "self", Started: now.Add(-time.Minute), LastPoll: now.Add(-time.Minute), Seen: map[string]bool{}, Jobs: map[string]*Job{}, StickerMessage: 1}
	f := &fakeAPI{profile: Profile{Follows: true}}
	e := &Engine{State: s, API: f, Save: func(*State) error { return nil }, Now: func() time.Time { return now }, Delay: func() (time.Duration, error) { return time.Minute, nil }, Log: func(string) {}}
	return e, f, &now
}
func enqueue(t *testing.T, e *Engine, now time.Time) {
	t.Helper()
	if err := e.Ingest([]Event{{"notice", "target", "test", now.Unix()}}, now); err != nil {
		t.Fatal(err)
	}
}
func TestDelayDedupAndExactSequence(t *testing.T) {
	e, f, now := fixture(t)
	enqueue(t, e, *now)
	due := e.State.Jobs["target"].Due
	if err := e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 {
		t.Fatal("sent before delay")
	}
	*now = now.Add(30 * time.Second)
	enqueue(t, e, *now)
	if !e.State.Jobs["target"].Due.Equal(due) {
		t.Fatal("deadline changed")
	}
	*now = now.Add(29 * time.Second)
	e.Work(context.Background())
	if len(f.sent) != 0 {
		t.Fatal("sent early")
	}
	*now = now.Add(time.Second)
	if err := e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 2 || f.sent[0] != "text" || f.sent[1] != "sticker" || e.State.Jobs["target"].Phase != "sent" {
		t.Fatal("sequence failed", f.sent)
	}
	e.Ingest([]Event{{"refollow", "target", "test", now.Unix()}}, *now)
	e.Work(context.Background())
	if len(f.sent) != 2 {
		t.Fatal("duplicate greeting")
	}
}
func TestBaselineStaleAndFutureExcluded(t *testing.T) {
	e, _, now := fixture(t)
	events := []Event{{"old", "a", "", e.State.Started.Unix()}, {"future", "b", "", now.Add(time.Minute).Unix()}, {"current", "c", "", now.Unix()}}
	if err := e.Ingest(events, *now); err != nil {
		t.Fatal(err)
	}
	if len(e.State.Jobs) != 1 || e.State.Jobs["c"] == nil {
		t.Fatal("wrong baseline filter")
	}
}
func TestPreflightAndRecipientDenials(t *testing.T) {
	for _, kind := range []string{"blocked", "unfollowed", "deleted", "closed", "own_restriction"} {
		t.Run(kind, func(t *testing.T) {
			e, f, now := fixture(t)
			enqueue(t, e, *now)
			*now = now.Add(time.Minute)
			switch kind {
			case "blocked":
				f.profile.Blocked = true
			case "unfollowed":
				f.profile.Follows = false
			case "deleted":
				f.profile.Deleted = true
			case "closed":
				f.prepareErr = ErrRecipientUnavailable
			case "own_restriction":
				f.prepareErr = ErrDenied
			}
			if err := e.Work(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.sent) != 0 {
				t.Fatal("sent despite preflight")
			}
			if kind == "own_restriction" && e.State.Paused == "" {
				t.Fatal("did not stop on restriction")
			}
		})
	}
}
func TestCommittedBeforeSendAndCrashRecovery(t *testing.T) {
	e, f, now := fixture(t)
	enqueue(t, e, *now)
	*now = now.Add(time.Minute)
	path := filepath.Join(t.TempDir(), "state.json")
	e.Save = func(s *State) error { return Save(path, s) }
	f.beforeSend = func(kind string) {
		s, err := Load(path)
		if err != nil || s.Jobs["target"].Phase != kind+"_attempted" {
			t.Fatal("send not durably recorded")
		}
	}
	f.sendErr = errors.New("connection lost after server accepted")
	if err := e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e.State = s
	if err = e.Recover(); err != nil {
		t.Fatal(err)
	}
	e.Work(context.Background())
	if len(f.sent) != 1 || s.Paused == "" {
		t.Fatal("ambiguous send retried")
	}
}
func TestDiskFailurePreventsSend(t *testing.T) {
	e, f, now := fixture(t)
	enqueue(t, e, *now)
	*now = now.Add(time.Minute)
	e.Save = func(s *State) error {
		if s.Jobs["target"].Phase == "text_attempted" {
			return errors.New("disk full")
		}
		return nil
	}
	if e.Work(context.Background()) == nil || len(f.sent) != 0 {
		t.Fatal("sent without durable write")
	}
}
func TestRestartAfterVerifiedTextSendsOnlySticker(t *testing.T) {
	e, f, now := fixture(t)
	enqueue(t, e, *now)
	j := e.State.Jobs["target"]
	j.Phase = "text_verified"
	j.TextID = "text-id"
	*now = now.Add(time.Minute)
	e.Recover()
	e.Work(context.Background())
	if len(f.sent) != 1 || f.sent[0] != "sticker" || j.Phase != "sent" {
		t.Fatal("repeated text on restart")
	}
}
func TestSleepDropsStaleJob(t *testing.T) {
	e, f, now := fixture(t)
	enqueue(t, e, *now)
	*now = now.Add(2 * time.Hour)
	e.Work(context.Background())
	if len(f.sent) != 0 || e.State.Jobs["target"].Phase != "skipped" {
		t.Fatal("sent backlog")
	}
}
func TestNoDuplicateForExistingGreeting(t *testing.T) {
	e, f, now := fixture(t)
	enqueue(t, e, *now)
	*now = now.Add(time.Minute)
	f.history = []Message{{"existing", "self", Greeting, "text"}}
	e.Work(context.Background())
	if len(f.sent) != 0 {
		t.Fatal("repeated existing greeting")
	}
}
func TestSendRecipientDenialContinuesAndAccountDenialStops(t *testing.T) {
	for _, denied := range []error{ErrRecipientUnavailable, ErrDenied} {
		e, f, now := fixture(t)
		enqueue(t, e, *now)
		*now = now.Add(time.Minute)
		f.sendErr = denied
		e.Work(context.Background())
		if len(f.sent) != 1 {
			t.Fatal("sticker sent after text denied")
		}
		if (e.State.Paused != "") != (denied == ErrDenied) {
			t.Fatal("wrong denial scope")
		}
	}
}
func TestRandomDelayBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		d, e := RandomDelay()
		if e != nil || d < time.Minute || d > 5*time.Minute {
			t.Fatal("bad delay", d, e)
		}
	}
}
func TestRestrictionClassification(t *testing.T) {
	if !errors.Is(classifyDenied(&tt.MessagingDenied{Check: 7283}), ErrRecipientUnavailable) {
		t.Fatal("closed DM")
	}
	for _, code := range []uint64{7289, 7284, 7170, 7195, 9999} {
		if !errors.Is(classifyDenied(&tt.MessagingDenied{Check: code}), ErrDenied) {
			t.Fatal("sender restriction ignored")
		}
	}
}
func TestStatePermissionsAndLock(t *testing.T) {
	e, _, _ := fixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Save(path, e.State); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("state permissions")
	}
	lock, err := Lock(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Lock(path + ".lock"); err == nil {
		t.Fatal("concurrent process allowed")
	}
	lock.Close()
	lock, err = Lock(path + ".lock")
	if err != nil {
		t.Fatal("stale lock after exit")
	}
	lock.Close()
}

func TestFollowerSchemaFailsClosed(t *testing.T) {
	valid := map[string]any{"notice_lists": []any{map[string]any{"group": float64(7), "has_more": float64(0), "notice_list": []any{map[string]any{"nid_str": "event", "create_time": float64(100), "follow": map[string]any{"from_user": map[string]any{"uid": "1000000000000000002", "unique_id": "test"}}}}}}}
	events, more, oldest, err := parseEvents(valid)
	if err != nil || len(events) != 1 || more || oldest != 100 || events[0].UserID != "1000000000000000002" {
		t.Fatal("valid event lost", err)
	}
	if _, _, _, err = parseEvents(map[string]any{}); err == nil {
		t.Fatal("accepted missing group")
	}
	group := valid["notice_lists"].([]any)[0].(map[string]any)
	delete(group, "has_more")
	if _, _, _, err = parseEvents(valid); err == nil {
		t.Fatal("accepted missing pagination")
	}
}

func TestRejectionCodesSurviveRestart(t *testing.T) {
	e, f, now := fixture(t)
	enqueue(t, e, *now)
	*now = now.Add(time.Minute)
	f.sendErr = &APIRejection{Status: 3, Check: 7170, Cause: ErrDenied}
	if err := e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, e.State); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e.State = s
	if err = e.Recover(); err != nil {
		t.Fatal(err)
	}
	j := s.Jobs["target"]
	if j.Rejection == nil || j.Rejection.Check != 7170 || j.Rejection.Status != 3 || j.Reason != "tiktok_rejected_send" || s.Paused != "tiktok_rejected_send" {
		t.Fatal("lost rejection diagnostics")
	}
	e.Work(context.Background())
	if len(f.sent) != 1 {
		t.Fatal("retried rejected send")
	}
}

func TestCustomScenarioSurvivesEditsAndRestart(t *testing.T) {
	e, f, now := fixture(t)
	settings := DefaultSettings(45)
	settings.Text = "Спасибо за подписку!\nДобро пожаловать ❤️"
	settings.SendSticker = false
	settings.DelayMin = 12
	settings.DelayMax = 12
	e.Settings = &settings
	e.Delay = func() (time.Duration, error) { return settings.Delay() }
	enqueue(t, e, *now)
	j := e.State.Jobs["target"]
	if j.Due.Sub(*now) != 12*time.Second {
		t.Fatal("custom delay not used")
	}
	settings.Text = "Новое сообщение"
	settings.SendSticker = true
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, e.State); err != nil {
		t.Fatal(err)
	}
	var err error
	e.State, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(12 * time.Second)
	if err = e.Work(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "text" || e.State.Jobs["target"].Phase != "sent" || f.history[0].Text != "Спасибо за подписку!\nДобро пожаловать ❤️" {
		t.Fatal("saved scenario not respected", f.sent, e.State.Jobs["target"])
	}
}

func TestCustomTextWithStickerAndDuplicateCheck(t *testing.T) {
	for _, exists := range []bool{false, true} {
		e, f, now := fixture(t)
		settings := DefaultSettings(45)
		settings.Text = "Другой текст"
		e.Settings = &settings
		enqueue(t, e, *now)
		*now = now.Add(time.Minute)
		if exists {
			f.history = []Message{{"old", "self", settings.Text, "text"}}
		}
		if err := e.Work(context.Background()); err != nil {
			t.Fatal(err)
		}
		if exists && (len(f.sent) != 0 || e.State.Jobs["target"].Phase != "skipped") {
			t.Fatal("duplicate sent")
		}
		if !exists && (len(f.sent) != 2 || e.State.Jobs["target"].Phase != "sent") {
			t.Fatal("custom send failed")
		}
	}
}

func TestDetailedRestrictionClassification(t *testing.T) {
	for _, v := range []struct {
		code      uint64
		recipient bool
	}{
		{7202, true}, {7281, true}, {7282, true}, {7283, true}, {7290, true}, {7291, true}, {7394, true},
		{7284, false}, {7289, false}, {7393, false}, {7170, false}, {7195, false}, {7179, false}, {0, false}, {9999, false},
	} {
		err := classifyDenied(&tt.MessagingDenied{Status: 3, Check: 2, BusinessCode: v.code})
		if errors.Is(err, ErrRecipientUnavailable) != v.recipient || errors.Is(err, ErrDenied) == v.recipient {
			t.Fatalf("wrong scope for %d: %v", v.code, err)
		}
		var detail *APIRejection
		if !errors.As(err, &detail) || detail.BusinessCode != v.code || detail.Check != 2 || detail.Status != 3 {
			t.Fatal("lost diagnostics")
		}
	}
	for _, denial := range []*tt.MessagingDenied{
		{Status: 7202}, {Check: 7202}, {Status: 7289, Check: 2, BusinessCode: 7202}, {Status: 7289, Check: 7283}, {Status: 7289, Check: 2, BusinessCode: 7283}, {Status: 3, Check: 7283, BusinessCode: 9999},
	} {
		if !errors.Is(classifyDenied(denial), ErrDenied) {
			t.Fatal("conflicting or unknown restriction was ignored")
		}
	}
}
