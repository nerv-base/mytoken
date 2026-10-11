package codebuff

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzstar101/mytoken/internal/harness"
	"github.com/zzstar101/mytoken/internal/harness/harnesstest"
	"github.com/zzstar101/mytoken/internal/model"
)

const conformanceFixture = "testdata/conformance/codebuff"

// conformanceSeed holds the chat definitions Grow publishes.
const conformanceSeed = conformanceFixture + "/seed"

// conformanceChats maps each seeded chat to how many of its messages are on
// disk at every growth step. chat-messages.json is rewritten in place, so the
// seed keeps the full array and Grow truncates it to a growing prefix.
var conformanceChats = []conformanceChat{
	{seed: "chat-a.json", messages: map[int]int{0: 2, 1: 4, 2: 6}},
	{seed: "chat-b.json", messages: map[int]int{1: 1, 2: 3}},
}

// conformanceSteps is the number of growth steps (docs/HARNESS.md §6).
const conformanceSteps = 3

type conformanceChat struct {
	seed string
	// messages is step -> number of chat messages to publish at that step.
	messages map[int]int
}

// conformanceSeedChat is the on-disk shape of a seed file.
type conformanceSeedChat struct {
	Channel  string            `json:"channel"`
	Project  string            `json:"project"`
	ChatID   string            `json:"chatId"`
	RunState json.RawMessage   `json:"runState"`
	Messages []json.RawMessage `json:"messages"`
}

func TestConformance(t *testing.T) {
	harnesstest.Run(t, conformanceCase())
}

func BenchmarkConformance(b *testing.B) {
	harnesstest.Bench(b, conformanceCase())
}

func conformanceCase() harnesstest.Case {
	return harnesstest.Case{
		Name:    string(model.Codebuff),
		New:     func(root string) harness.Parser { return NewWithRoots(root) },
		Fixture: conformanceFixture,
		Steps:   conformanceSteps,
		Grow:    growConformance,
	}
}

// growConformance rebuilds the chat directories with the prefix of messages the
// step is allowed to see.
func growConformance(tb testing.TB, dir string, step int) error {
	tb.Helper()
	for _, chat := range conformanceChats {
		count, ok := chat.messages[step]
		if !ok {
			continue
		}
		seed, err := loadConformanceChat(chat.seed)
		if err != nil {
			return err
		}
		if count > len(seed.Messages) {
			count = len(seed.Messages)
		}
		chatDir := filepath.Join(dir, seed.Channel, "projects", seed.Project, "chats", seed.ChatID)
		if err := os.MkdirAll(chatDir, 0o755); err != nil {
			return err
		}
		payload, err := json.Marshal(seed.Messages[:count])
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(chatDir, "chat-messages.json"), payload, 0o644); err != nil {
			return err
		}
		if len(seed.RunState) > 0 {
			if err := os.WriteFile(filepath.Join(chatDir, "run-state.json"), seed.RunState, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func loadConformanceChat(name string) (conformanceSeedChat, error) {
	raw, err := os.ReadFile(filepath.Join(conformanceSeed, name))
	if err != nil {
		return conformanceSeedChat{}, err
	}
	var seed conformanceSeedChat
	if err := json.Unmarshal(raw, &seed); err != nil {
		return conformanceSeedChat{}, err
	}
	return seed, nil
}
