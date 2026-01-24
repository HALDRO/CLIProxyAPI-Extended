package to_ir

import (
	"encoding/json"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
)

// ParseAntigravityResponse parses a non-streaming Antigravity v1internal response into unified format.
// Antigravity wraps Gemini responses inside an envelope: {"response":{...},"traceId":"..."}.
func ParseAntigravityResponse(rawJSON []byte) (*ir.UnifiedChatRequest, []ir.Message, *ir.Usage, error) {
	messages, usage, _, err := ParseAntigravityResponseMeta(rawJSON)
	return nil, messages, usage, err
}

// ParseAntigravityResponseMeta parses a non-streaming Antigravity response and returns response meta.
func ParseAntigravityResponseMeta(rawJSON []byte) ([]ir.Message, *ir.Usage, *ir.ResponseMeta, error) {
	if !gjson.ValidBytes(rawJSON) {
		return nil, nil, nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}

	parsed := gjson.ParseBytes(rawJSON)
	inner := parsed
	if r := parsed.Get("response"); r.Exists() {
		inner = r
	}

	return ParseGeminiResponseMeta([]byte(inner.Raw))
}

// ParseAntigravityChunk parses an Antigravity streaming chunk into unified events.
// Streaming chunks are SSE data lines, but executors often pass the JSON payload directly.
func ParseAntigravityChunk(raw []byte) ([]ir.UnifiedEvent, error) {
	// Accept both SSE lines and raw JSON.
	raw = ir.ExtractSSEData(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if string(raw) == "[DONE]" {
		return []ir.UnifiedEvent{{Type: ir.EventTypeFinish}}, nil
	}
	if !gjson.ValidBytes(raw) {
		return nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}

	parsed := gjson.ParseBytes(raw)
	inner := parsed
	if r := parsed.Get("response"); r.Exists() {
		inner = r
	}

	return ParseGeminiChunk([]byte(inner.Raw))
}
