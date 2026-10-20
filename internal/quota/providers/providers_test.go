package providers

import (
	"testing"

	"github.com/zzstar101/mytoken/internal/quota/plan"
)

func TestAll(t *testing.T) {
	list := All()
	if len(list) != 12 {
		t.Fatalf("providers = %d, want 12", len(list))
	}
	registry, err := plan.NewRegistry(list...)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	ids := []string{"codex", "claude", "cursor", "copilot", "glm", "deepseek", "trae-cn", "trae-global", "workbuddy", "workbuddy-ai", "qoder-cn", "zcode"}
	for _, id := range ids {
		if _, ok := registry.Lookup(id); !ok {
			t.Errorf("missing provider %q", id)
		}
	}
}
