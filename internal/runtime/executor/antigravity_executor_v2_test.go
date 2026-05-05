package executor

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestAntigravityExecutorV2_BypassesShortCooldownForCreditsRequests(t *testing.T) {
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	exec := NewAntigravityExecutorV2(&config.Config{
		QuotaExceeded: config.QuotaExceeded{AntigravityCredits: true},
	})
	auth := &cliproxyauth.Auth{
		ID:       "ag-v2-credits-bypass",
		Provider: "antigravity",
		Metadata: map[string]any{
			"access_token": "token",
			"expired":      time.Now().Add(1 * time.Hour).Format(time.RFC3339),
			"project_id":   "project-1",
		},
	}
	markAntigravityShortCooldown(auth, "claude-opus-4-6-thinking", time.Now(), 30*time.Second)

	ctx := cliproxyauth.WithAntigravityCredits(context.Background())
	ctx = context.WithValue(ctx, "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       http.NoBody,
		}, nil
	}))

	_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "claude-opus-4-6-thinking",
		Payload: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:    translator.FormatOpenAI,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	})
	if err == nil {
		return
	}
	if status, ok := err.(interface{ StatusCode() int }); ok && status.StatusCode() == http.StatusTooManyRequests {
		t.Fatalf("expected credits request to bypass short cooldown, got 429: %v", err)
	}
}
