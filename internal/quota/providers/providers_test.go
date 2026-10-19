package providers

import (
	"context"
	"testing"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

type emptyStore struct{}

func (emptyStore) Setting(context.Context, string) (string, error) { return "", nil }

func TestAll(t *testing.T) {
	list := All(Options{Store: emptyStore{}})
	if len(list) != 5 {
		t.Fatalf("providers = %d, want 5", len(list))
	}
	registry, err := plan.NewRegistry(list...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ids := []string{"codex", "claude", "cursor", "copilot", "glm"}
	for _, id := range ids {
		if _, ok := registry.Lookup(id); !ok {
			t.Errorf("missing provider %q", id)
		}
	}
	// Codex is local-log backed and must stay pollable offline.
	codex, _ := registry.Lookup("codex")
	if local, ok := codex.(plan.LocalProvider); !ok || !local.Local() {
		t.Error("codex is not a local provider")
	}
}

func TestAllWithoutStore(t *testing.T) {
	list := All(Options{})
	if len(list) != 4 {
		t.Fatalf("providers = %d, want 4", len(list))
	}
	for _, provider := range list {
		if provider.ID() == "codex" {
			t.Fatal("codex included without a store")
		}
	}
}
