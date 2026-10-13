package greeter

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Settings are copied into each job, so edits never change an in-flight send.
type Block struct {
	StickerID string `json:"stickerId,omitempty"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Min       int    `json:"min,omitempty"`
	Max       int    `json:"max,omitempty"`
}
type Settings struct {
	ID          string  `json:"id,omitempty"`
	Name        string  `json:"name,omitempty"`
	Blocks      []Block `json:"blocks,omitempty"`
	Text        string  `json:"text"`
	DelayMin    int     `json:"delayMin"`
	DelayMax    int     `json:"delayMax"`
	SendSticker bool    `json:"sendSticker"`
	PollSeconds int     `json:"pollSeconds"`
}

func DefaultSettings(poll int) Settings {
	return Settings{Text: Greeting, DelayMin: 60, DelayMax: 300, SendSticker: true, PollSeconds: poll}
}
func (s Settings) Validate() error {
	if len(s.ID) > 100 || utf8.RuneCountInString(s.Name) > 80 {
		return fmt.Errorf("Слишком длинное название сценария")
	}
	if s.Blocks != nil {
		if s.PollSeconds < 30 || s.PollSeconds > 300 {
			return fmt.Errorf("Проверка подписок: от 30 до 300 секунд")
		}
		if len(s.Blocks) < 2 || len(s.Blocks) > 21 || s.Blocks[0].Type != "trigger" {
			return fmt.Errorf("Начните с триггера и добавьте от 1 до 20 блоков")
		}
		sends, total := 0, 0
		for i, b := range s.Blocks {
			switch b.Type {
			case "trigger":
				if i != 0 {
					return fmt.Errorf("Триггер должен быть только в начале")
				}
			case "message":
				if strings.TrimSpace(b.Text) == "" || !utf8.ValidString(b.Text) || utf8.RuneCountInString(b.Text) > 1000 {
					return fmt.Errorf("Блок %d: текст от 1 до 1000 символов", i)
				}
				sends++
			case "sticker":
				if b.StickerID != "" {
					id, err := strconv.ParseUint(b.StickerID, 10, 64)
					if err != nil || id == 0 {
						return fmt.Errorf("Некорректный стикер")
					}
				}
				sends++
			case "pause":
				if b.Min < 0 || b.Max < b.Min || b.Max > 1800 {
					return fmt.Errorf("Блок %d: пауза от 0 до 1800 секунд, максимум не меньше минимума", i)
				}
				total += b.Max
			default:
				return fmt.Errorf("Неизвестный тип блока")
			}
		}
		if sends == 0 {
			return fmt.Errorf("Добавьте хотя бы одно сообщение или стикер")
		}
		if total > 1800 {
			return fmt.Errorf("Сумма пауз не должна превышать 30 минут")
		}
		return nil
	}

	if strings.TrimSpace(s.Text) == "" || !utf8.ValidString(s.Text) || utf8.RuneCountInString(s.Text) > 1000 {
		return fmt.Errorf("Введите текст от 1 до 1000 символов")
	}
	if s.DelayMin < 0 || s.DelayMax < s.DelayMin || s.DelayMax > 1800 {
		return fmt.Errorf("Задержка: от 0 до 1800 секунд; максимум не меньше минимума")
	}
	if s.PollSeconds < 30 || s.PollSeconds > 300 {
		return fmt.Errorf("Проверка подписок: от 30 до 300 секунд")
	}
	return nil
}
func LoadSettings(path string, poll int) (Settings, error) {
	s := DefaultSettings(poll)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	return s, s.Validate()
}
func (s Settings) Delay() (time.Duration, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(s.DelayMax-s.DelayMin+1)))
	if err != nil {
		return 0, err
	}
	return time.Duration(int64(s.DelayMin)+n.Int64()) * time.Second, nil
}
func (j *Job) Scenario() Settings {
	if j.Settings != nil {
		return *j.Settings
	}
	return DefaultSettings(45)
}

func (s Settings) Workflow() []Block {
	if s.Blocks != nil {
		return append([]Block(nil), s.Blocks...)
	}
	blocks := []Block{{Type: "trigger"}, {Type: "pause", Min: s.DelayMin, Max: s.DelayMax}, {Type: "message", Text: s.Text}}
	if s.SendSticker {
		blocks = append(blocks, Block{Type: "sticker"})
	}
	return blocks
}
func (j *Job) MessageText() string {
	if j.Settings != nil && j.Settings.Blocks != nil && j.Step < len(j.Settings.Blocks) {
		return j.Settings.Blocks[j.Step].Text
	}
	return j.Scenario().Text
}
