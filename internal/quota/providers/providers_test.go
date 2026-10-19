package providers

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

func TestAll(t *testing.T) {
	list := All()
	if len(list) != 11 {
		t.Fatalf("providers = %d, want 11", len(list))
	}
	registry, err := plan.NewRegistry(list...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ids := []string{"codex", "claude", "cursor", "copilot", "glm", "deepseek", "trae-cn", "trae-global", "workbuddy", "workbuddy-ai", "qoder-cn"}
	for _, id := range ids {
		if _, ok := registry.Lookup(id); !ok {
			t.Errorf("missing provider %q", id)
		}
	}
}
