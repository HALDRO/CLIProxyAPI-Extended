package registry

import "testing"

func TestParseProviderPrefixedModelID(t *testing.T) {
	model, provider := ParseProviderPrefixedModelID("[Gemini CLI] gemini-2.5-pro")
	if model != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %q", model)
	}
	if provider != "gemini-cli" {
		t.Fatalf("expected provider gemini-cli, got %q", provider)
	}

	model2, provider2 := ParseProviderPrefixedModelID("gemini-2.5-pro")
	if model2 != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %q", model2)
	}
	if provider2 != "" {
		t.Fatalf("expected empty provider, got %q", provider2)
	}
}

func TestLabelToProviderID(t *testing.T) {
	tests := []struct {
		label    string
		expected string
	}{
		{"Gemini CLI", "gemini-cli"},
		{"gemini cli", "gemini-cli"},
		{"GEMINI CLI", "gemini-cli"},
		{"Antigravity", "antigravity"},
		{"AI Studio", "aistudio"},
		{"Claude", "claude"},
		{"OpenAI", "openai"},
		{"Vertex", "vertex"},
		{"Unknown Provider", "unknown-provider"},
	}

	for _, tt := range tests {
		result := labelToProviderID(tt.label)
		if result != tt.expected {
			t.Errorf("labelToProviderID(%q) = %q, want %q", tt.label, result, tt.expected)
		}
	}
}

func TestRoundTripProviderIDAndLabel(t *testing.T) {
	providerIDs := []string{"gemini-cli", "antigravity", "claude", "codex", "vertex", "aistudio"}

	for _, id := range providerIDs {
		label := providerIDToLabel(id)
		backToID := labelToProviderID(label)
		if backToID != id {
			t.Errorf("Round trip failed for %q: label=%q, backToID=%q", id, label, backToID)
		}
	}
}
