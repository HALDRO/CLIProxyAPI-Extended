package registry

import "testing"

func TestNormalizeKiroRoute(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		canonical string
		upstream  string
		origin    string
	}{
		{name: "kiro sonnet 4 6", input: "kiro-claude-sonnet-4-6", canonical: "kiro-claude-sonnet-4-6", upstream: "claude-sonnet-4.6", origin: "AI_EDITOR"},
		{name: "bare sonnet 4.6", input: "claude-sonnet-4.6", canonical: "kiro-claude-sonnet-4-6", upstream: "claude-sonnet-4.6", origin: "AI_EDITOR"},
		{name: "bare sonnet 4-5", input: "claude-sonnet-4-5", canonical: "kiro-claude-sonnet-4-5", upstream: "claude-sonnet-4.5", origin: "AI_EDITOR"},
		{name: "amazonq sonnet 4-6", input: "amazonq-claude-sonnet-4-6", canonical: "kiro-claude-sonnet-4-6", upstream: "claude-sonnet-4.6", origin: "CLI"},
		{name: "kiro auto", input: "kiro-auto", canonical: "kiro-auto", upstream: "auto", origin: "AI_EDITOR"},
		{name: "amazonq auto", input: "amazonq-auto", canonical: "kiro-auto", upstream: "auto", origin: "CLI"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeKiroRoute(tt.input)
			if got.CanonicalID != tt.canonical || got.UpstreamID != tt.upstream || got.Origin != tt.origin {
				t.Fatalf("NormalizeKiroRoute(%q) = %#v, want canonical=%q upstream=%q origin=%q", tt.input, got, tt.canonical, tt.upstream, tt.origin)
			}
		})
	}
}
