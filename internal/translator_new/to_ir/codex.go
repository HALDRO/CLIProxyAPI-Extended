package to_ir

import (
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ParseCodexChunk parses streaming SSE chunk FROM Codex API into events.
// It wraps ParseOpenAIChunk and applies Codex-specific sanitization (e.g. grep args).
func ParseCodexChunk(rawJSON []byte) ([]ir.UnifiedEvent, error) {
	events, err := ParseOpenAIChunk(rawJSON)
	if err != nil {
		return nil, err
	}

	for i := range events {
		sanitizeEvent(&events[i])
	}

	return events, nil
}

// ParseCodexResponse parses non-streaming response FROM Codex API into unified format.
// It wraps ParseOpenAIResponse and applies Codex-specific sanitization.
func ParseCodexResponse(rawJSON []byte) ([]ir.Message, *ir.Usage, error) {
	messages, usage, err := ParseOpenAIResponse(rawJSON)
	if err != nil {
		return nil, nil, err
	}

	for i := range messages {
		sanitizeMessage(&messages[i])
	}

	return messages, usage, nil
}

func sanitizeEvent(e *ir.UnifiedEvent) {
	if e.ToolCall != nil {
		e.ToolCall.Args = sanitizeCodexGrepArgs(e.ToolCall.Name, e.ToolCall.Args)
	}
}

func sanitizeMessage(m *ir.Message) {
	for i := range m.ToolCalls {
		m.ToolCalls[i].Args = sanitizeCodexGrepArgs(m.ToolCalls[i].Name, m.ToolCalls[i].Args)
	}
}

// sanitizeCodexGrepArgs cleans up grep arguments to ensure compatibility with ripgrep.
// Codex sometimes generates conflicting arguments like -C with -A/-B.
// IMPORTANT: Cursor considers -A/-B present even when they are 0, so we must treat
// "exists" + "zero" as effectively not set.
func sanitizeCodexGrepArgs(toolName, args string) string {
	if !gjson.Valid(args) {
		return args
	}

	// We sanitize in two cases:
	// 1) We know it's grep/ripgrep_raw_search by tool name
	// 2) Tool name is missing (common for streaming deltas), but args clearly look like grep args
	isKnownGrepTool := toolName == "grep" || toolName == "ripgrep_raw_search"
	if !isKnownGrepTool && toolName != "" {
		return args
	}
	looksLikeGrepArgs := gjson.Get(args, "pattern").Exists() && gjson.Get(args, "-C").Exists() && (gjson.Get(args, "-A").Exists() || gjson.Get(args, "-B").Exists())
	if !isKnownGrepTool && !looksLikeGrepArgs {
		return args
	}

	parsed := gjson.Parse(args)

	c := parsed.Get("-C")
	a := parsed.Get("-A")
	b := parsed.Get("-B")

	hasC := c.Exists()
	hasA := a.Exists()
	hasB := b.Exists()

	if !hasC || (!hasA && !hasB) {
		return args
	}

	isZero := func(v gjson.Result) bool {
		if !v.Exists() {
			return true
		}
		switch v.Type {
		case gjson.Number:
			return v.Int() == 0
		case gjson.String:
			// Be defensive: sometimes models serialize numbers as strings.
			return v.String() == "0" || v.String() == "0.0" || v.String() == ""
		default:
			return v.Int() == 0
		}
	}

	cZero := isZero(c)

	// Cursor validation treats the PRESENCE of -A/-B/-C as "specified" even when values are 0.
	// So if -C is present together with -A/-B, we must remove the conflicting keys deterministically.
	//
	// Policy (mirrors the older fork behavior, adapted for Cursor validation):
	// - If -C is non-zero: keep -C, remove -A/-B
	// - If -C is zero: remove -C, keep -A/-B (even if they are 0)
	if !cZero {
		cleaned := args
		cleaned, _ = sjson.Delete(cleaned, "-A")
		cleaned, _ = sjson.Delete(cleaned, "-B")
		return cleaned
	}

	cleaned, _ := sjson.Delete(args, "-C")
	return cleaned
}
