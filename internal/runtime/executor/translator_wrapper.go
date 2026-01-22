package executor

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/from_ir"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/to_ir"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TranslateAntigravityResponseNonStream converts Antigravity non-streaming response to target format using new translator.
// Antigravity wraps responses in an envelope, so we unwrap it first using to_ir.ParseAntigravityResponse.
func TranslateAntigravityResponseNonStream(cfg *config.Config, to sdktranslator.Format, antigravityResponse []byte, model string) ([]byte, error) {
	// Parse Antigravity response to IR (handles envelope unwrapping)
	_, messages, usage, err := to_ir.ParseAntigravityResponse(antigravityResponse)
	if err != nil {
		return nil, err
	}

	return convertIRToNonStreamResponse(to, messages, usage, model, "chatcmpl-"+model)
}

// TranslateAntigravityResponseStream converts Antigravity streaming chunk to target format using new translator.
// Antigravity wraps chunks in an envelope, so we unwrap it first using to_ir.ParseAntigravityChunk.
// state parameter is optional but recommended for stateful conversions (e.g., Claude tool calls).
func TranslateAntigravityResponseStream(cfg *config.Config, to sdktranslator.Format, antigravityChunk []byte, model string, messageID string, state *GeminiCLIStreamState) ([][]byte, error) {
	// Parse Antigravity chunk to IR events
	events, err := to_ir.ParseAntigravityChunk(antigravityChunk)
	if err != nil {
		return nil, err
	}

	return convertGeminiEventsToChunks(events, to, model, messageID, state)
}

// OpenAI request format aliases for convenience.
const (
	FormatChatCompletions = from_ir.FormatChatCompletions
	FormatResponsesAPI    = from_ir.FormatResponsesAPI
)

// convertRequestToIR converts a request payload to unified format.
// This is the shared logic used by all Gemini-family translators.
// Returns (nil, nil) if the format is unsupported (caller should use fallback).
func convertRequestToIR(from sdktranslator.Format, model string, payload []byte, metadata map[string]any) (*ir.UnifiedChatRequest, error) {
	var irReq *ir.UnifiedChatRequest
	var err error

	// Determine source format and convert to IR
	switch from.String() {
	case "openai", "cline": // Cline uses OpenAI-compatible format
		irReq, err = to_ir.ParseOpenAIRequest(payload)
	case "ollama":
		irReq, err = to_ir.ParseOllamaRequest(payload)
	case "claude":
		irReq, err = to_ir.ParseClaudeRequest(payload)
	default:
		// Unsupported format
		return nil, fmt.Errorf("new translator: unsupported source format %q", from.String())
	}

	if err != nil {
		return nil, err
	}

	// Override model if specified
	if model != "" {
		irReq.Model = model
	}

	// Store metadata for provider-specific handling
	if metadata != nil {
		irReq.Metadata = metadata
	}

	// Apply thinking overrides from metadata if present (highest priority)
	if metadata != nil {
		budgetOverride, includeOverride, hasOverride := extractThinkingFromMetadata(metadata)
		if hasOverride {
			if irReq.Thinking == nil {
				irReq.Thinking = &ir.ThinkingConfig{}
			}
			if budgetOverride != nil {
				irReq.Thinking.Budget = *budgetOverride
			}
			if includeOverride != nil {
				irReq.Thinking.IncludeThoughts = *includeOverride
			}
		}
	}

	return irReq, nil
}

// TranslateToGeminiCLI converts request to Gemini CLI format using new translator.
// metadata contains additional context like thinking overrides from request metadata.
// Note: Antigravity uses the same format as Gemini CLI, so this function works for both.
func TranslateToGeminiCLI(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any) ([]byte, error) {
	// Convert to IR using shared helper
	irReq, err := convertRequestToIR(from, model, payload, metadata)
	if err != nil {
		return nil, err
	}
	if irReq == nil {
		// Unsupported format
		return nil, fmt.Errorf("new translator: unsupported source format %q for Gemini CLI conversion", from.String())
	}

	// Convert IR to Gemini CLI format
	geminiJSON, err := (&from_ir.GeminiCLIProvider{}).ConvertRequest(irReq)
	if err != nil {
		return nil, err
	}

	// Apply payload config overrides from YAML
	return applyPayloadConfigToIR(cfg, model, geminiJSON), nil
}

// extractThinkingFromMetadata extracts thinking config overrides from request metadata
func extractThinkingFromMetadata(metadata map[string]any) (budget *int, include *bool, hasOverride bool) {
	if metadata == nil {
		return nil, nil, false
	}

	if v, ok := metadata["thinking_budget"].(int); ok {
		budget = &v
		hasOverride = true
	}
	if v, ok := metadata["include_thoughts"].(bool); ok {
		include = &v
		hasOverride = true
	}

	return budget, include, hasOverride
}
// applyPayloadConfigToIR applies YAML payload config rules to the generated JSON
func applyPayloadConfigToIR(cfg *config.Config, model string, payload []byte) []byte {
	if cfg == nil || len(payload) == 0 {
		return payload
	}

	// Apply default rules (only set if missing)
	for _, rule := range cfg.Payload.Default {
		if matchesPayloadRule(rule, model, "gemini") {
			for path, value := range rule.Params {
				fullPath := "request." + path
				if !gjson.GetBytes(payload, fullPath).Exists() {
					payload, _ = sjson.SetBytes(payload, fullPath, value)
				}
			}
		}
	}

	// Apply override rules (always set)
	for _, rule := range cfg.Payload.Override {
		if matchesPayloadRule(rule, model, "gemini") {
			for path, value := range rule.Params {
				fullPath := "request." + path
				payload, _ = sjson.SetBytes(payload, fullPath, value)
			}
		}
	}

	return payload
}

// matchesPayloadRule checks if a payload rule matches the given model and protocol
func matchesPayloadRule(rule config.PayloadRule, model, protocol string) bool {
	for _, m := range rule.Models {
		if m.Protocol != "" && m.Protocol != protocol {
			continue
		}
		if matchesPattern(m.Name, model) {
			return true
		}
	}
	return false
}

// matchesPattern checks if a model name matches a pattern (supports wildcards)
func matchesPattern(pattern, name string) bool {
	if pattern == name {
		return true
	}
	if pattern == "*" {
		return true
	}
	if strings.HasPrefix(pattern, "*") && strings.HasSuffix(pattern, "*") {
		return strings.Contains(name, pattern[1:len(pattern)-1])
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(name, pattern[1:])
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(name, pattern[:len(pattern)-1])
	}
	return false
}

// TranslateToGemini converts request to Gemini (AI Studio API) format using new translator.
// metadata contains additional context like thinking overrides from request metadata.
func TranslateToGemini(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any) ([]byte, error) {
	// Convert to IR using shared helper
	irReq, err := convertRequestToIR(from, model, payload, metadata)
	if err != nil {
		return nil, err
	}
	if irReq == nil {
		// Unsupported format
		return nil, fmt.Errorf("new translator: unsupported source format %q for Gemini conversion", from.String())
	}

	// Convert IR to Gemini format
	geminiJSON, err := (&from_ir.GeminiProvider{}).ConvertRequest(irReq)
	if err != nil {
		return nil, err
	}

	// Apply payload config overrides from YAML
	return applyPayloadConfigToIR(cfg, model, geminiJSON), nil
}

// TranslateGeminiCLIResponseNonStream converts Gemini CLI non-streaming response to target format using new translator.
func TranslateGeminiCLIResponseNonStream(cfg *config.Config, to sdktranslator.Format, geminiResponse []byte, model string) ([]byte, error) {
	// Step 1: Parse Gemini CLI response to IR
	messages, usage, err := (&from_ir.GeminiCLIProvider{}).ParseResponse(geminiResponse)
	if err != nil {
		return nil, err
	}

	return convertIRToNonStreamResponse(to, messages, usage, model, "chatcmpl-"+model)
}

// GeminiCLIStreamState maintains state for stateful streaming conversions (e.g., Claude tool calls).
type GeminiCLIStreamState struct {
	ClaudeState          *from_ir.ClaudeStreamState
	ToolCallIndex        int  // Track tool call index across chunks for OpenAI format
	ReasoningTokensCount int  // Track accumulated reasoning tokens for final usage chunk
	ReasoningCharsAccum  int  // Track accumulated reasoning characters (for estimation if provider doesn't give count)
	FinishSent           bool // Track if finish event was already sent (prevent duplicates)
	ToolCallSentHeader   map[int]bool
	HasContent           bool // Track if any actual content was output (text, reasoning, or tool calls)
}

// NewAntigravityStreamState creates a new stream state for Antigravity provider.
func NewAntigravityStreamState(originalRequest []byte) *GeminiCLIStreamState {
	state := &GeminiCLIStreamState{
		ClaudeState:        from_ir.NewClaudeStreamState(),
		ToolCallSentHeader: make(map[int]bool),
	}

	return state
}

// TranslateGeminiCLIResponseStream converts Gemini CLI streaming chunk to target format using new translator.
// state parameter is optional but recommended for stateful conversions (e.g., Claude tool calls).
func TranslateGeminiCLIResponseStream(cfg *config.Config, to sdktranslator.Format, geminiChunk []byte, model string, messageID string, state *GeminiCLIStreamState) ([][]byte, error) {
	// Step 1: Parse Gemini CLI chunk to IR events
	events, err := (&from_ir.GeminiCLIProvider{}).ParseStreamChunk(geminiChunk)
	if err != nil {
		return nil, err
	}

	return convertGeminiEventsToChunks(events, to, model, messageID, state)
}

// TranslateGeminiResponseNonStream converts Gemini (AI Studio) non-streaming response to target format using new translator.
func TranslateGeminiResponseNonStream(cfg *config.Config, to sdktranslator.Format, geminiResponse []byte, model string) ([]byte, error) {
	// Step 1: Parse Gemini response to IR with metadata
	messages, usage, meta, err := to_ir.ParseGeminiResponseMeta(geminiResponse)
	if err != nil {
		return nil, err
	}

	// Step 2: Convert IR to target format
	toStr := to.String()

	// Use responseId from metadata if available, otherwise generate
	messageID := "chatcmpl-" + model
	if meta != nil && meta.ResponseID != "" {
		messageID = meta.ResponseID
	}

	if toStr == "openai" || toStr == "cline" {
		// Build OpenAI metadata from Gemini response metadata
		var openaiMeta *ir.OpenAIMeta
		if meta != nil {
			openaiMeta = &ir.OpenAIMeta{
				ResponseID:         meta.ResponseID,
				CreateTime:         meta.CreateTime,
				NativeFinishReason: meta.NativeFinishReason,
			}
			if usage != nil {
				openaiMeta.ThoughtsTokenCount = usage.ThoughtsTokenCount
			}
		}
		return from_ir.ToOpenAIChatCompletionMeta(messages, usage, model, messageID, openaiMeta)
	}

	return convertIRToNonStreamResponse(to, messages, usage, model, messageID)
}

// TranslateGeminiResponseStream converts Gemini (AI Studio) streaming chunk to target format using new translator.
func TranslateGeminiResponseStream(cfg *config.Config, to sdktranslator.Format, geminiChunk []byte, model string, messageID string, state *GeminiCLIStreamState) ([][]byte, error) {
	// Step 1: Parse Gemini chunk to IR events
	events, err := to_ir.ParseGeminiChunk(geminiChunk)
	if err != nil {
		return nil, err
	}

	return convertGeminiEventsToChunks(events, to, model, messageID, state)
}

// Shared helper to convert IR events to chunks for Gemini providers (CLI and API)
func convertGeminiEventsToChunks(events []ir.UnifiedEvent, to sdktranslator.Format, model, messageID string, state *GeminiCLIStreamState) ([][]byte, error) {
	if len(events) == 0 {
		return nil, nil
	}

	var chunks [][]byte
	toStr := to.String()

	switch toStr {
	case "openai", "cline":
		if state == nil {
			state = &GeminiCLIStreamState{ToolCallSentHeader: make(map[int]bool)}
		}
		if state.ToolCallSentHeader == nil {
			state.ToolCallSentHeader = make(map[int]bool)
		}

		for i := range events {
			event := &events[i]

			// Track content
			switch event.Type {
			case ir.EventTypeToken:
				if event.Content != "" {
					state.HasContent = true
				}
			case ir.EventTypeReasoning:
				if event.Reasoning != "" {
					state.HasContent = true
					state.ReasoningCharsAccum += len(event.Reasoning)
				}
			case ir.EventTypeToolCall:
				state.HasContent = true
			}

			// Handle finish event logic
			if event.Type == ir.EventTypeFinish {
				if state.FinishSent {
					continue // Skip duplicate finish
				}
				// CRITICAL: Prevent empty STOP events
				if !state.HasContent {
					continue
				}
				state.FinishSent = true

				// Override finish reason if we have tools
				if state.ToolCallIndex > 0 {
					event.FinishReason = ir.FinishReasonToolCalls
				}

				// Estimate reasoning tokens if needed
				if state.ReasoningCharsAccum > 0 {
					if event.Usage == nil {
						event.Usage = &ir.Usage{}
					}
					if event.Usage.ThoughtsTokenCount == 0 {
						event.Usage.ThoughtsTokenCount = (state.ReasoningCharsAccum + 2) / 3
					}
				}
			}

			// Handle Tool Call Indexing
			idx := 0
			if event.Type == ir.EventTypeToolCall {
				idx = state.ToolCallIndex
				state.ToolCallIndex++
			}

			if event.ToolCall != nil {
				event.ToolCallIndex = idx
				if state.ToolCallSentHeader[idx] {
					event.ToolCall.ID = ""
					event.ToolCall.Name = ""
				} else {
					state.ToolCallSentHeader[idx] = true
				}
			}

			chunk, err := from_ir.ToOpenAIChunk(*event, model, messageID, idx)
			if err != nil {
				return nil, err
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		}

	case "claude":
		if state == nil {
			state = &GeminiCLIStreamState{ClaudeState: from_ir.NewClaudeStreamState()}
		}
		if state.ClaudeState == nil {
			state.ClaudeState = from_ir.NewClaudeStreamState()
		}
		for _, event := range events {
			claudeChunks, err := from_ir.ToClaudeSSE(event, model, messageID, state.ClaudeState)
			if err != nil {
				return nil, err
			}
			if claudeChunks != nil {
				chunks = append(chunks, claudeChunks)
			}
		}

	case "ollama":
		for _, event := range events {
			chunk, err := from_ir.ToOllamaChatChunk(event, model)
			if err != nil {
				return nil, err
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		}

	default:
		return nil, fmt.Errorf("new translator: unsupported target format %q for Gemini stream conversion", toStr)
	}

	return chunks, nil
}

// Shared helper to convert IR to non-stream response for common formats
func convertIRToNonStreamResponse(to sdktranslator.Format, messages []ir.Message, usage *ir.Usage, model, messageID string) ([]byte, error) {
	switch to.String() {
	case "openai", "cline":
		return from_ir.ToOpenAIChatCompletion(messages, usage, model, messageID)
	case "claude":
		return from_ir.ToClaudeResponse(messages, usage, model, messageID)
	case "ollama":
		// Ollama has two formats: chat and generate. Default to chat for compatibility.
		return from_ir.ToOllamaChatResponse(messages, usage, model)
	default:
		return nil, fmt.Errorf("new translator: unsupported target format %q", to.String())
	}
}

// TranslateClaudeResponseNonStream converts Claude non-streaming response to target format using new translator.
func TranslateClaudeResponseNonStream(cfg *config.Config, to sdktranslator.Format, claudeResponse []byte, model string) ([]byte, error) {
	// Step 1: Parse Claude response to IR
	messages, usage, err := to_ir.ParseClaudeResponse(claudeResponse)
	if err != nil {
		return nil, err
	}

	// Step 2: Convert IR to target format
	if to.String() == "claude" {
		return claudeResponse, nil
	}
	return convertIRToNonStreamResponse(to, messages, usage, model, "msg-"+model)
}

// TranslateClaudeResponseStream converts Claude streaming chunk to target format using new translator.
func TranslateClaudeResponseStream(cfg *config.Config, to sdktranslator.Format, claudeChunk []byte, model string, messageID string, state *from_ir.ClaudeStreamState) ([][]byte, error) {
	// Step 1: Parse Claude chunk to IR events
	events, err := to_ir.ParseClaudeChunk(claudeChunk)
	if err != nil {
		return nil, err
	}

	if len(events) == 0 {
		return nil, nil
	}

	// Step 2: Convert IR events to target format chunks
	toStr := to.String()
	var chunks [][]byte

	switch toStr {
	case "openai", "cline":
		for _, event := range events {
			// Use ToolCallIndex from event for proper tool call indexing
			idx := event.ToolCallIndex
			chunk, err := from_ir.ToOpenAIChunk(event, model, messageID, idx)
			if err != nil {
				return nil, err
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		}
	case "ollama":
		for _, event := range events {
			chunk, err := from_ir.ToOllamaChatChunk(event, model)
			if err != nil {
				return nil, err
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		}
	case "claude":
		// Passthrough - already in Claude format
		return [][]byte{claudeChunk}, nil
	default:
		// Unsupported target format
		return nil, fmt.Errorf("new translator: unsupported target format %q for Claude stream conversion", toStr)
	}

	return chunks, nil
}

// OpenAIStreamState maintains state for OpenAI → OpenAI streaming conversions.
type OpenAIStreamState struct {
	ReasoningCharsAccum int // Track accumulated reasoning characters for token estimation
	// ToolCallIDMap maps Codex item_id to call_id for proper tool call ID consistency.
	// Codex API uses item_id in delta events but call_id is what clients expect.
	ToolCallIDMap      map[string]string
	ToolCallSentHeader map[int]bool // Track if tool call header (ID/Name) has been sent
	// ResponsesState holds state for Responses API streaming (used by Codex)
	ResponsesState *from_ir.ResponsesStreamState
	// ToolCallIsCustom tracks which tool call indices are custom tools
	ToolCallIsCustom []int
	// OutputIndexToToolIndex maps Responses API output_index to Chat Completions tool_calls index.
	// Responses API uses output_index as global index (0=reasoning, 1=message, 2+=tool_calls),
	// but Chat Completions expects tool_calls[].index to start from 0 for first tool call.
	OutputIndexToToolIndex map[int]int
	// NextToolCallIndex tracks the next available tool call index for Chat Completions format.
	NextToolCallIndex int
	// ClaudeState holds state for OpenAI → Claude streaming conversions.
	// Used when Claude CLI sends requests through OpenAI-compatible providers (like Cline).
	ClaudeState *from_ir.ClaudeStreamState
}

// NewOpenAIStreamState creates a new stream state for OpenAI provider.
func NewOpenAIStreamState() *OpenAIStreamState {
	return &OpenAIStreamState{
		ToolCallIDMap:          make(map[string]string),
		ToolCallSentHeader:     make(map[int]bool),
		OutputIndexToToolIndex: make(map[int]int),
		NextToolCallIndex:      0,
		ClaudeState:            from_ir.NewClaudeStreamState(),
	}
}

// TranslateToOpenAI converts request to OpenAI API format (Chat Completions or Responses API) using new translator.
// format specifies the target OpenAI format (FormatChatCompletions or FormatResponsesAPI).
func TranslateToOpenAI(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any, format from_ir.OpenAIRequestFormat) ([]byte, error) {
	// Convert to IR using shared helper
	irReq, err := convertRequestToIR(from, model, payload, metadata)
	if err != nil {
		return nil, err
	}
	if irReq == nil {
		// Unsupported format
		return nil, fmt.Errorf("new translator: unsupported source format %q for OpenAI conversion", from.String())
	}

	// Convert IR to OpenAI format
	openaiJSON, err := from_ir.ToOpenAIRequestFmt(irReq, format)
	if err != nil {
		return nil, err
	}

	// Add stream parameter if streaming is requested
	if streaming {
		openaiJSON, _ = sjson.SetBytes(openaiJSON, "stream", true)
	}

	return openaiJSON, nil
}

// TranslateToClaude converts request to Claude Messages API format using new translator.
// metadata contains additional context like thinking overrides from request metadata.
func TranslateToClaude(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any) ([]byte, error) {
	// Convert to IR using shared helper
	irReq, err := convertRequestToIR(from, model, payload, metadata)
	if err != nil {
		return nil, err
	}
	if irReq == nil {
		// Unsupported format
		return nil, fmt.Errorf("new translator: unsupported source format %q for Claude conversion", from.String())
	}

	// Convert IR to Claude format
	claudeJSON, err := (&from_ir.ClaudeProvider{}).ConvertRequest(irReq)
	if err != nil {
		return nil, err
	}

	// Add stream parameter if streaming is requested
	if streaming {
		claudeJSON, _ = sjson.SetBytes(claudeJSON, "stream", true)
	}

	return claudeJSON, nil
}

// TranslateOpenAIResponseStream converts OpenAI streaming chunk to target format using new translator.
// This is used for OpenAI-compatible providers (like Ollama) to ensure reasoning_tokens is properly set.
func TranslateOpenAIResponseStream(cfg *config.Config, to sdktranslator.Format, openaiChunk []byte, model string, messageID string, state *OpenAIStreamState) ([][]byte, error) {
	return TranslateOpenAIResponseStreamForced(to, openaiChunk, model, messageID, state)
}

// TranslateOpenAIResponseStreamForced converts OpenAI streaming chunk to target format.
// Always uses new translator regardless of config (for providers like Cline that require it).
func TranslateOpenAIResponseStreamForced(to sdktranslator.Format, openaiChunk []byte, model string, messageID string, state *OpenAIStreamState) ([][]byte, error) {
	toStr := to.String()

	// PASSTHROUGH for Codex: upstream already sends correct Responses API SSE format.
	// We should NOT parse and re-create events - just pass them through as-is.
	// This preserves original sequence_numbers, item_ids, call_ids, and event ordering.
	if toStr == "codex" {
		trimmed := bytes.TrimSpace(openaiChunk)
		if len(trimmed) == 0 {
			return nil, nil
		}
		// Skip [DONE] marker - it will be added by WriteDone() in stream forwarder
		// to avoid duplication
		if bytes.Equal(trimmed, []byte("data: [DONE]")) || bytes.Equal(trimmed, []byte("[DONE]")) {
			return nil, nil
		}
		return [][]byte{trimmed}, nil
	}

	// Step 1: Parse OpenAI chunk to IR events
	events, err := to_ir.ParseOpenAIChunk(openaiChunk)
	if err != nil {
		return nil, err
	}

	if len(events) == 0 {
		return nil, nil
	}

	// Step 2: Convert IR events to target format chunks
	var chunks [][]byte

	switch toStr {
	case "openai", "cline":
		if state == nil {
			state = &OpenAIStreamState{
				ToolCallIDMap:          make(map[string]string),
				ToolCallSentHeader:     make(map[int]bool),
				OutputIndexToToolIndex: make(map[int]int),
				NextToolCallIndex:      0,
			}
		}
		if state.ToolCallIDMap == nil {
			state.ToolCallIDMap = make(map[string]string)
		}
		if state.ToolCallSentHeader == nil {
			state.ToolCallSentHeader = make(map[int]bool)
		}
		if state.OutputIndexToToolIndex == nil {
			state.OutputIndexToToolIndex = make(map[int]int)
		}
		for i := range events {
			event := &events[i]

			// Track reasoning content for token estimation
			if event.Type == ir.EventTypeReasoning && event.Reasoning != "" {
				state.ReasoningCharsAccum += len(event.Reasoning)
			}

			// Handle tool call ID mapping for Codex API compatibility
			// Codex uses item_id in delta events but call_id is what clients expect
			if event.ToolCall != nil {
				if event.Type == ir.EventTypeToolCall && event.ToolCall.ItemID != "" && event.ToolCall.ID != "" {
					// This is from response.output_item.added - save the mapping
					state.ToolCallIDMap[event.ToolCall.ItemID] = event.ToolCall.ID
				} else if event.ToolCall.ItemID != "" && event.ToolCall.ID == "" {
					// This is from delta/done event - lookup the call_id
					if callID, ok := state.ToolCallIDMap[event.ToolCall.ItemID]; ok {
						event.ToolCall.ID = callID
					}
				}
			}

			// On finish, handle reasoning tokens and fix finish_reason for tool calls
			if event.Type == ir.EventTypeFinish {
				// Fix finish_reason: if we had tool calls, change "stop" to "tool_calls"
				// This is critical for Cursor to know it should execute the tool calls
				if state.NextToolCallIndex > 0 && event.FinishReason == ir.FinishReasonStop {
					event.FinishReason = ir.FinishReasonToolCalls
				}

				// Ensure reasoning_tokens is set if we had reasoning content
				if state.ReasoningCharsAccum > 0 {
					if event.Usage == nil {
						event.Usage = &ir.Usage{}
					}
					if event.Usage.ThoughtsTokenCount == 0 {
						// Estimate: ~3 chars per token (conservative for mixed languages)
						event.Usage.ThoughtsTokenCount = (state.ReasoningCharsAccum + 2) / 3
					}
				}
			}

			// Handle Tool Call Indexing
			// Responses API uses output_index as global index (0=reasoning, 1=message, 2+=tool_calls),
			// but Chat Completions expects tool_calls[].index to start from 0 for first tool call.
			outputIdx := event.ToolCallIndex
			idx := outputIdx // Default to original index

			// Map output_index to tool_call_index for tool call events
			if event.Type == ir.EventTypeToolCall || event.Type == ir.EventTypeToolCallDelta {
				if mappedIdx, exists := state.OutputIndexToToolIndex[outputIdx]; exists {
					// Use existing mapping
					idx = mappedIdx
				} else if event.Type == ir.EventTypeToolCall {
					// First time seeing this output_index for a tool call - assign new tool_call_index
					idx = state.NextToolCallIndex
					state.OutputIndexToToolIndex[outputIdx] = idx
					state.NextToolCallIndex++
				}
				// For ToolCallDelta without prior ToolCall event, keep original index (shouldn't happen normally)
			}

			// Special handling for Responses API tool calls to prevent duplication
			// The stream sends:
			// 1. response.output_item.added (Type=ToolCall) -> has ID and Name, no Args
			// 2. response.function_call_arguments.delta (Type=ToolCallDelta) -> has Args

			// For Delta events, we NEVER want to send ID/Type, only arguments
			if event.Type == ir.EventTypeToolCallDelta {
				event.ToolCall.ID = ""
				event.ToolCall.Name = ""
			} else if event.Type == ir.EventTypeToolCall {
				// For ToolCall events (start of tool call), we send ID/Name/Type ONCE
				// However, if we mapped a CallID from ItemID, we must ensure it's set
				if event.ToolCall.ItemID != "" && event.ToolCall.ID == "" {
					if callID, ok := state.ToolCallIDMap[event.ToolCall.ItemID]; ok {
						event.ToolCall.ID = callID
					}
				}

				if state.ToolCallSentHeader[idx] {
					event.ToolCall.ID = ""
					event.ToolCall.Name = ""
				} else {
					state.ToolCallSentHeader[idx] = true
				}
			}

			chunk, err := from_ir.ToOpenAIChunk(*event, model, messageID, idx)
			if err != nil {
				return nil, err
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		}
	case "ollama":
		for _, event := range events {
			chunk, err := from_ir.ToOllamaChatChunk(event, model)
			if err != nil {
				return nil, err
			}
			if chunk != nil {
				chunks = append(chunks, chunk)
			}
		}
	case "claude":
		// Convert OpenAI streaming chunks to Claude SSE format
		// This is needed when Claude Code CLI sends requests through providers
		// that use OpenAI-compatible APIs (like Cline)
		var claudeState *from_ir.ClaudeStreamState
		if state != nil && state.ClaudeState != nil {
			claudeState = state.ClaudeState
		} else {
			claudeState = from_ir.NewClaudeStreamState()
			claudeState.Model = model
			claudeState.MessageID = messageID
			if state != nil {
				state.ClaudeState = claudeState
			}
		}

		for _, event := range events {
			chunkBytes, err := from_ir.ToClaudeSSE(event, model, messageID, claudeState)
			if err != nil {
				return nil, err
			}
			if len(chunkBytes) > 0 {
				chunks = append(chunks, chunkBytes)
			}
		}

		// If we have events but no chunks yet, we might need to emit message_start
		if len(chunks) == 0 && len(events) > 0 {
			// Check if any event has content that should trigger output
			for _, event := range events {
				if event.Type == ir.EventTypeFinish {
					// Emit finish events
					finishBytes, err := from_ir.ToClaudeSSE(event, model, messageID, claudeState)
					if err != nil {
						return nil, err
					}
					if len(finishBytes) > 0 {
						chunks = append(chunks, finishBytes)
					}
				}
			}
		}
	default:
		// Unsupported target format
		return nil, fmt.Errorf("new translator: unsupported target format %q for OpenAI stream conversion", toStr)
	}

	return chunks, nil
}

// TranslateOpenAIResponseNonStream converts OpenAI non-streaming response to target format using new translator.
func TranslateOpenAIResponseNonStream(cfg *config.Config, to sdktranslator.Format, openaiResponse []byte, model string) ([]byte, error) {
	return TranslateOpenAIResponseNonStreamForced(to, openaiResponse, model)
}

// TranslateResponseNonStreamAuto translates non-streaming response with automatic provider detection.
// Returns formatted response ready to send to client.
func TranslateResponseNonStreamAuto(cfg *config.Config, provider string, to sdktranslator.Format, upstreamResp []byte, model string) ([]byte, error) {
	var translated []byte
	var err error

	switch provider {
	case "gemini-cli":
		translated, err = TranslateGeminiCLIResponseNonStream(cfg, to, upstreamResp, model)
	case "antigravity":
		translated, err = TranslateAntigravityResponseNonStream(cfg, to, upstreamResp, model)
	case "gemini", "aistudio":
		translated, err = TranslateGeminiResponseNonStream(cfg, to, upstreamResp, model)
	case "claude":
		translated, err = TranslateClaudeResponseNonStream(cfg, to, upstreamResp, model)
	case "openai", "codex", "cline", "ollama":
		translated, err = TranslateOpenAIResponseNonStream(cfg, to, upstreamResp, model)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}

	if err != nil {
		return nil, err
	}
	return ensureColonSpacedJSON(translated), nil
}

// TranslateResponseStreamAuto translates streaming response chunk with automatic provider detection.
// Returns formatted chunks ready to send to client.
func TranslateResponseStreamAuto(cfg *config.Config, provider string, to sdktranslator.Format, upstreamChunk []byte, model string, messageID string, state interface{}) ([][]byte, error) {
	var chunks [][]byte
	var err error

	switch provider {
	case "gemini-cli":
		chunks, err = TranslateGeminiCLIResponseStream(cfg, to, upstreamChunk, model, messageID, state.(*GeminiCLIStreamState))
	case "antigravity":
		chunks, err = TranslateAntigravityResponseStream(cfg, to, upstreamChunk, model, messageID, state.(*GeminiCLIStreamState))
	case "gemini", "aistudio":
		chunks, err = TranslateGeminiResponseStream(cfg, to, upstreamChunk, model, messageID, state.(*GeminiCLIStreamState))
	case "claude":
		chunks, err = TranslateClaudeResponseStream(cfg, to, upstreamChunk, model, messageID, state.(*from_ir.ClaudeStreamState))
	case "openai", "codex", "cline", "ollama":
		chunks, err = TranslateOpenAIResponseStream(cfg, to, upstreamChunk, model, messageID, state.(*OpenAIStreamState))
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}

	if err != nil {
		return nil, err
	}

	// Apply formatting to all chunks
	for i := range chunks {
		chunks[i] = ensureColonSpacedJSON(chunks[i])
	}
	return chunks, nil
}

// Always uses new translator regardless of config (for providers like Cline that require it).
func TranslateOpenAIResponseNonStreamForced(to sdktranslator.Format, openaiResponse []byte, model string) ([]byte, error) {
	// Step 1: Parse OpenAI response to IR
	messages, usage, err := to_ir.ParseOpenAIResponse(openaiResponse)
	if err != nil {
		return nil, err
	}

	// Step 2: Convert IR to target format
	return convertIRToNonStreamResponse(to, messages, usage, model, "chatcmpl-"+model)
}
