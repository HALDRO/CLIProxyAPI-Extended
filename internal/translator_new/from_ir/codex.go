package from_ir

import (
	"encoding/json"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
)

// ToCodexRequest converts unified request to Codex (Responses API) JSON.
//
// Codex endpoint (chatgpt.com/backend-api/codex/responses) is stricter than generic
// OpenAI /v1/responses. It requires certain fields (e.g., store=false) and rejects
// some standard generation parameters.
func ToCodexRequest(req *ir.UnifiedChatRequest) ([]byte, error) {
	m := map[string]interface{}{"model": req.Model}

	// Build tool call context: map tool_call_id -> tool_name for custom tool detection
	toolCallContext := buildToolCallContext(req.Messages, req.Tools)

	// Build input array - convert system messages to user messages.
	// Codex doesn't support role:system in input[], and instructions are validated.
	var input []interface{}
	for _, msg := range req.Messages {
		if msg.Role == ir.RoleSystem {
			if text := ir.CombineTextParts(msg); text != "" {
				input = append(input, map[string]interface{}{
					"type": "message",
					"role": "user",
					"content": []interface{}{
						map[string]interface{}{"type": "input_text", "text": text},
					},
				})
			}
			continue
		}
		items := convertMessageToResponsesInputWithContext(msg, toolCallContext)
		input = append(input, items...)
	}
	if len(input) > 0 {
		m["input"] = input
	}

	if req.Thinking != nil {
		applyResponsesThinking(m, req.Thinking)
	}

	if len(req.Tools) > 0 {
		m["tools"] = buildResponsesTools(req.Tools)
	}
	if req.ToolChoice != "" {
		m["tool_choice"] = req.ToolChoice
	}

	// Codex expects include reasoning.encrypted_content.
	m["include"] = []string{"reasoning.encrypted_content"}

	// Codex expects parallel_tool_calls to be true.
	m["parallel_tool_calls"] = true

	if req.PreviousResponseID != "" {
		m["previous_response_id"] = req.PreviousResponseID
	}
	if req.PromptID != "" {
		applyPromptConfig(m, req)
	}
	if req.PromptCacheKey != "" {
		m["prompt_cache_key"] = req.PromptCacheKey
	}

	// Codex requires store=false.
	m["store"] = false

	// Intentionally do NOT emit temperature/top_p/max_output_tokens for Codex.
	// (Codex upstream rejects them.)

	return json.Marshal(m)
}
