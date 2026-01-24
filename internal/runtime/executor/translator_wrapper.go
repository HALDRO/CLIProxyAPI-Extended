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

// =========================================================================================
// Unified State Management
// =========================================================================================

// UnifiedStreamState maintains state across streaming chunks for all providers.
// It consolidates fields from the previous GeminiCLIStreamState and OpenAIStreamState.
type UnifiedStreamState struct {
	// Common
	ClaudeState         *from_ir.ClaudeStreamState
	ReasoningCharsAccum int          // Track accumulated reasoning characters
	ToolCallSentHeader  map[int]bool // Track if tool call header (ID/Name) has been sent
	HasContent          bool         // Track if any actual content was output

	// Logic Handling
	ToolCallIndex  int               // Current linear index for tool calls (0, 1, 2...)
	FinishSent     bool              // Track if finish event was already sent
	ToolCallIDMap  map[string]string // Maps item_id -> call_id (specific to OpenAI Responses API input)
	OutputIndexMap map[int]int       // Maps source output_index -> target tool_index
}

// EnsureInitialized initializes maps and substructures if they are nil.
func (s *UnifiedStreamState) EnsureInitialized() {
	if s.ToolCallSentHeader == nil {
		s.ToolCallSentHeader = make(map[int]bool)
	}
	if s.ToolCallIDMap == nil {
		s.ToolCallIDMap = make(map[string]string)
	}
	if s.OutputIndexMap == nil {
		s.OutputIndexMap = make(map[int]int)
	}
	if s.ClaudeState == nil {
		s.ClaudeState = from_ir.NewClaudeStreamState()
	}
}

// Aliases for compatibility with existing codebase signatures.
// These allow existing code to continue working without changes to imports/types.
type GeminiCLIStreamState = UnifiedStreamState
type OpenAIStreamState = UnifiedStreamState

// NewAntigravityStreamState creates a new state (used by Antigravity/Gemini executors).
func NewAntigravityStreamState(originalRequest []byte) *UnifiedStreamState {
	s := &UnifiedStreamState{}
	s.EnsureInitialized()
	return s
}

// NewOpenAIStreamState creates a new state (used by OpenAI/Codex executors).
func NewOpenAIStreamState() *UnifiedStreamState {
	s := &UnifiedStreamState{}
	s.EnsureInitialized()
	return s
}

// =========================================================================================
// Request Translation (Client -> Provider)
// =========================================================================================

// TranslateToGeminiCLI converts request to Gemini CLI format.
func TranslateToGeminiCLI(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any) ([]byte, error) {
	return translateRequestCommon(cfg, from, model, payload, metadata, func(irReq *ir.UnifiedChatRequest) ([]byte, error) {
		return (&from_ir.GeminiCLIProvider{}).ConvertRequest(irReq)
	})
}

// TranslateToGemini converts request to Gemini (AI Studio) format.
func TranslateToGemini(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any) ([]byte, error) {
	return translateRequestCommon(cfg, from, model, payload, metadata, func(irReq *ir.UnifiedChatRequest) ([]byte, error) {
		return (&from_ir.GeminiProvider{}).ConvertRequest(irReq)
	})
}

// TranslateToClaude converts request to Claude format.
func TranslateToClaude(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any) ([]byte, error) {
	req, err := translateRequestCommon(cfg, from, model, payload, metadata, func(irReq *ir.UnifiedChatRequest) ([]byte, error) {
		return (&from_ir.ClaudeProvider{}).ConvertRequest(irReq)
	})
	if err != nil {
		return nil, err
	}
	if streaming {
		req, _ = sjson.SetBytes(req, "stream", true)
	}
	return req, nil
}

// TranslateToCodex converts request to Codex Responses API.
func TranslateToCodex(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any) ([]byte, error) {
	req, err := translateRequestCommon(cfg, from, model, payload, metadata, func(irReq *ir.UnifiedChatRequest) ([]byte, error) {
		return from_ir.ToCodexRequest(irReq)
	})
	if err != nil {
		return nil, err
	}
	if streaming {
		req, _ = sjson.SetBytes(req, "stream", true)
	}
	return req, nil
}

// TranslateToOpenAI converts request to OpenAI format (Chat Completions or Responses API).
func TranslateToOpenAI(cfg *config.Config, from sdktranslator.Format, model string, payload []byte, streaming bool, metadata map[string]any, format from_ir.OpenAIRequestFormat) ([]byte, error) {
	req, err := translateRequestCommon(cfg, from, model, payload, metadata, func(irReq *ir.UnifiedChatRequest) ([]byte, error) {
		return from_ir.ToOpenAIRequestFmt(irReq, format)
	})
	if err != nil {
		return nil, err
	}
	if streaming {
		req, _ = sjson.SetBytes(req, "stream", true)
	}
	return req, nil
}

// translateRequestCommon handles the parsing to IR, metadata injection, and config application.
func translateRequestCommon(
	cfg *config.Config,
	from sdktranslator.Format,
	model string,
	payload []byte,
	metadata map[string]any,
	converter func(*ir.UnifiedChatRequest) ([]byte, error),
) ([]byte, error) {
	// 1. Convert to IR
	irReq, err := convertRequestToIR(from, model, payload, metadata)
	if err != nil {
		return nil, err
	}
	if irReq == nil {
		return nil, fmt.Errorf("new translator: unsupported source format %q", from.String())
	}

	// 2. Convert IR to Target
	result, err := converter(irReq)
	if err != nil {
		return nil, err
	}

	// 3. Apply Config Overrides (Common for Gemini-family, harmless for others if no rules match)
	return applyPayloadConfigToIR(cfg, model, result), nil
}

// convertRequestToIR converts any supported source format to UnifiedChatRequest.
func convertRequestToIR(from sdktranslator.Format, model string, payload []byte, metadata map[string]any) (*ir.UnifiedChatRequest, error) {
	var irReq *ir.UnifiedChatRequest
	var err error

	switch from.String() {
	case "openai", "cline":
		irReq, err = to_ir.ParseOpenAIRequest(payload)
	case "ollama":
		irReq, err = to_ir.ParseOllamaRequest(payload)
	case "claude":
		irReq, err = to_ir.ParseClaudeRequest(payload)
	default:
		return nil, fmt.Errorf("new translator: unsupported source format %q", from.String())
	}

	if err != nil {
		return nil, err
	}

	// Apply overrides
	if model != "" {
		irReq.Model = model
	}
	if metadata != nil {
		irReq.Metadata = metadata
		applyThinkingOverrides(irReq, metadata)
	}

	return irReq, nil
}

func applyThinkingOverrides(irReq *ir.UnifiedChatRequest, metadata map[string]any) {
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

func extractThinkingFromMetadata(metadata map[string]any) (budget *int, include *bool, hasOverride bool) {
	if v, ok := metadata["thinking_budget"].(int); ok {
		budget = &v
		hasOverride = true
	}
	if v, ok := metadata["include_thoughts"].(bool); ok {
		include = &v
		hasOverride = true
	}
	return
}

// =========================================================================================
// Response Translation (Provider -> Client) - Streaming
// =========================================================================================

// TranslateAntigravityResponseStream handles Antigravity streaming (uses envelope).
func TranslateAntigravityResponseStream(cfg *config.Config, to sdktranslator.Format, chunk []byte, model, msgID string, state *UnifiedStreamState) ([][]byte, error) {
	events, err := to_ir.ParseAntigravityChunk(chunk)
	if err != nil {
		return nil, err
	}
	return convertUnifiedEventsToChunks(events, to, model, msgID, state)
}

// TranslateGeminiCLIResponseStream handles Gemini CLI streaming.
func TranslateGeminiCLIResponseStream(cfg *config.Config, to sdktranslator.Format, chunk []byte, model, msgID string, state *UnifiedStreamState) ([][]byte, error) {
	events, err := (&from_ir.GeminiCLIProvider{}).ParseStreamChunk(chunk)
	if err != nil {
		return nil, err
	}
	return convertUnifiedEventsToChunks(events, to, model, msgID, state)
}

// TranslateGeminiResponseStream handles Gemini API streaming.
func TranslateGeminiResponseStream(cfg *config.Config, to sdktranslator.Format, chunk []byte, model, msgID string, state *UnifiedStreamState) ([][]byte, error) {
	events, err := to_ir.ParseGeminiChunk(chunk)
	if err != nil {
		return nil, err
	}
	return convertUnifiedEventsToChunks(events, to, model, msgID, state)
}

// TranslateClaudeResponseStream handles Claude streaming.
func TranslateClaudeResponseStream(cfg *config.Config, to sdktranslator.Format, chunk []byte, model, msgID string, state *from_ir.ClaudeStreamState) ([][]byte, error) {
	// Claude uses its own specific state struct in the parser, which is fine to keep separate
	// as it's purely for the parser side.
	events, err := to_ir.ParseClaudeChunk(chunk)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}

	// For converting TO other formats, we technically could use the unified converter,
	// but Claude->Claude is a passthrough, and Claude->OpenAI is simple.

	toStr := to.String()
	var chunks [][]byte

	switch toStr {
	case "openai", "cline":
		for _, event := range events {
			chunk, err := from_ir.ToOpenAIChunk(event, model, msgID, event.ToolCallIndex)
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
		return [][]byte{chunk}, nil
	default:
		return nil, fmt.Errorf("new translator: unsupported target %q", toStr)
	}
	return chunks, nil
}

// TranslateOpenAIResponseStream handles OpenAI/Compatible streaming.
func TranslateOpenAIResponseStream(cfg *config.Config, to sdktranslator.Format, chunk []byte, model, msgID string, state *UnifiedStreamState) ([][]byte, error) {
	// Passthrough for Codex optimization
	if to.String() == "codex" {
		trimmed := bytes.TrimSpace(chunk)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("data: [DONE]")) || bytes.Equal(trimmed, []byte("[DONE]")) {
			return nil, nil
		}
		return [][]byte{trimmed}, nil
	}

	events, err := to_ir.ParseOpenAIChunk(chunk)
	if err != nil {
		return nil, err
	}
	return convertUnifiedEventsToChunks(events, to, model, msgID, state)
}

// convertUnifiedEventsToChunks is the SINGLE source of truth for converting IR events
// to any target chunk format. It merges logic from previous Gemini and OpenAI converters.
func convertUnifiedEventsToChunks(events []ir.UnifiedEvent, to sdktranslator.Format, model, messageID string, state *UnifiedStreamState) ([][]byte, error) {
	if len(events) == 0 {
		return nil, nil
	}

	// Ensure state is initialized
	if state == nil {
		// Should not happen if caller uses constructors, but safe fallback
		state = &UnifiedStreamState{}
	}
	state.EnsureInitialized()

	var chunks [][]byte
	toStr := to.String()

	switch toStr {
	case "openai", "cline":
		for i := range events {
			event := &events[i]

			// 1. Update State (Reasoning & Content)
			if event.Content != "" || event.Reasoning != "" || event.ToolCall != nil {
				state.HasContent = true
			}
			if event.Type == ir.EventTypeReasoning && event.Reasoning != "" {
				state.ReasoningCharsAccum += len(event.Reasoning)
			}

			// 2. Handle Tool Call Identity Mapping (Responses API -> OpenAI quirk)
			//    Only relevant if input events have ItemID but no CallID (OpenAI input),
			//    but harmless for others.
			if event.ToolCall != nil {
				tc := event.ToolCall
				if event.Type == ir.EventTypeToolCall && tc.ItemID != "" && tc.ID != "" {
					// Register mapping
					state.ToolCallIDMap[tc.ItemID] = tc.ID
				} else if tc.ItemID != "" && tc.ID == "" {
					// Lookup mapping
					if callID, ok := state.ToolCallIDMap[tc.ItemID]; ok {
						tc.ID = callID
					}
				}
			}

			// 3. Handle Tool Call Indexing (Linearizing output indices)
			//    Map arbitrary input ToolCallIndex to 0, 1, 2...
			outputIdx := event.ToolCallIndex
			effectiveIdx := outputIdx

			if event.Type == ir.EventTypeToolCall || event.Type == ir.EventTypeToolCallDelta {
				if mappedIdx, exists := state.OutputIndexMap[outputIdx]; exists {
					effectiveIdx = mappedIdx
				} else if event.Type == ir.EventTypeToolCall {
					// Assign new linear index
					effectiveIdx = state.ToolCallIndex
					state.OutputIndexMap[outputIdx] = effectiveIdx
					state.ToolCallIndex++
				}
			}

			// 4. Handle Finish Events
			if event.Type == ir.EventTypeFinish {
				if state.FinishSent {
					continue // Duplicate finish prevention
				}
				if !state.HasContent {
					continue // Empty STOP prevention
				}
				state.FinishSent = true

				// Fix finish_reason for tool calls
				if state.ToolCallIndex > 0 && event.FinishReason == ir.FinishReasonStop {
					event.FinishReason = ir.FinishReasonToolCalls
				}

				// Synthesize reasoning usage if missing
				if state.ReasoningCharsAccum > 0 {
					if event.Usage == nil {
						event.Usage = &ir.Usage{}
					}
					if event.Usage.ThoughtsTokenCount == 0 {
						event.Usage.ThoughtsTokenCount = (state.ReasoningCharsAccum + 2) / 3
					}
				}
			}

			// 5. Cleanup Tool Call Headers for Delta events
			if event.Type == ir.EventTypeToolCallDelta {
				event.ToolCall.ID = ""
				event.ToolCall.Name = ""
			} else if event.Type == ir.EventTypeToolCall {
				if state.ToolCallSentHeader[effectiveIdx] {
					event.ToolCall.ID = ""
					event.ToolCall.Name = ""
				} else {
					state.ToolCallSentHeader[effectiveIdx] = true
				}
			}

			// 6. Emit Chunk
			chunk, err := from_ir.ToOpenAIChunk(*event, model, messageID, effectiveIdx)
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
		if state.ClaudeState != nil {
			state.ClaudeState.Model = model
			state.ClaudeState.MessageID = messageID
		}

		for _, event := range events {
			chunkBytes, err := from_ir.ToClaudeSSE(event, model, messageID, state.ClaudeState)
			if err != nil {
				return nil, err
			}
			if len(chunkBytes) > 0 {
				chunks = append(chunks, chunkBytes)
			}
		}

		// If events finished but no chunks produced (e.g. pure finish event), ensure clean closure
		if len(chunks) == 0 && len(events) > 0 {
			for _, event := range events {
				if event.Type == ir.EventTypeFinish {
					finishBytes, _ := from_ir.ToClaudeSSE(event, model, messageID, state.ClaudeState)
					if len(finishBytes) > 0 {
						chunks = append(chunks, finishBytes)
					}
				}
			}
		}

	default:
		return nil, fmt.Errorf("new translator: unsupported target format %q", toStr)
	}

	return chunks, nil
}

// =========================================================================================
// Response Translation (Provider -> Client) - Non-Streaming
// =========================================================================================

// TranslateAntigravityResponseNonStream handles Antigravity response.
func TranslateAntigravityResponseNonStream(cfg *config.Config, to sdktranslator.Format, resp []byte, model string) ([]byte, error) {
	_, messages, usage, err := to_ir.ParseAntigravityResponse(resp)
	if err != nil {
		return nil, err
	}
	return convertIRToNonStreamResponse(to, messages, usage, model, "chatcmpl-"+model)
}

// TranslateGeminiCLIResponseNonStream handles Gemini CLI response.
func TranslateGeminiCLIResponseNonStream(cfg *config.Config, to sdktranslator.Format, resp []byte, model string) ([]byte, error) {
	messages, usage, err := (&from_ir.GeminiCLIProvider{}).ParseResponse(resp)
	if err != nil {
		return nil, err
	}
	return convertIRToNonStreamResponse(to, messages, usage, model, "chatcmpl-"+model)
}

// TranslateGeminiResponseNonStream handles Gemini API response.
func TranslateGeminiResponseNonStream(cfg *config.Config, to sdktranslator.Format, resp []byte, model string) ([]byte, error) {
	messages, usage, meta, err := to_ir.ParseGeminiResponseMeta(resp)
	if err != nil {
		return nil, err
	}

	messageID := "chatcmpl-" + model
	if meta != nil && meta.ResponseID != "" {
		messageID = meta.ResponseID
	}

	// Special handling for OpenAI/Cline targets to include Gemini metadata
	if to.String() == "openai" || to.String() == "cline" {
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

// TranslateClaudeResponseNonStream handles Claude response.
func TranslateClaudeResponseNonStream(cfg *config.Config, to sdktranslator.Format, resp []byte, model string) ([]byte, error) {
	messages, usage, err := to_ir.ParseClaudeResponse(resp)
	if err != nil {
		return nil, err
	}
	if to.String() == "claude" {
		return resp, nil
	}
	return convertIRToNonStreamResponse(to, messages, usage, model, "msg-"+model)
}

// TranslateOpenAIResponseNonStream handles OpenAI response.
func TranslateOpenAIResponseNonStream(cfg *config.Config, to sdktranslator.Format, resp []byte, model string) ([]byte, error) {
	messages, usage, err := to_ir.ParseOpenAIResponse(resp)
	if err != nil {
		return nil, err
	}
	return convertIRToNonStreamResponse(to, messages, usage, model, "chatcmpl-"+model)
}

// convertIRToNonStreamResponse is the common finisher for non-stream responses.
func convertIRToNonStreamResponse(to sdktranslator.Format, messages []ir.Message, usage *ir.Usage, model, messageID string) ([]byte, error) {
	switch to.String() {
	case "openai", "cline":
		return from_ir.ToOpenAIChatCompletion(messages, usage, model, messageID)
	case "claude":
		return from_ir.ToClaudeResponse(messages, usage, model, messageID)
	case "ollama":
		return from_ir.ToOllamaChatResponse(messages, usage, model)
	default:
		return nil, fmt.Errorf("new translator: unsupported target format %q", to.String())
	}
}

// =========================================================================================
// Auto-Detection Entrypoints
// =========================================================================================

func TranslateResponseNonStreamAuto(cfg *config.Config, provider string, to sdktranslator.Format, resp []byte, model string) ([]byte, error) {
	var translated []byte
	var err error

	switch provider {
	case "gemini-cli":
		translated, err = TranslateGeminiCLIResponseNonStream(cfg, to, resp, model)
	case "antigravity":
		translated, err = TranslateAntigravityResponseNonStream(cfg, to, resp, model)
	case "gemini", "aistudio":
		translated, err = TranslateGeminiResponseNonStream(cfg, to, resp, model)
	case "claude":
		translated, err = TranslateClaudeResponseNonStream(cfg, to, resp, model)
	case "openai", "cline", "ollama":
		translated, err = TranslateOpenAIResponseNonStream(cfg, to, resp, model)
	case "codex":
		messages, usage, err := to_ir.ParseCodexResponse(resp)
		if err == nil {
			translated, err = convertIRToNonStreamResponse(to, messages, usage, model, "chatcmpl-"+model)
		}
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}

	if err != nil {
		return nil, err
	}
	return ensureColonSpacedJSON(translated), nil
}

func TranslateResponseStreamAuto(cfg *config.Config, provider string, to sdktranslator.Format, chunk []byte, model, msgID string, state interface{}) ([][]byte, error) {
	var chunks [][]byte
	var err error

	// Cast state safely
	var unifiedState *UnifiedStreamState
	if s, ok := state.(*UnifiedStreamState); ok {
		unifiedState = s
	}

	switch provider {
	case "gemini-cli":
		chunks, err = TranslateGeminiCLIResponseStream(cfg, to, chunk, model, msgID, unifiedState)
	case "antigravity":
		chunks, err = TranslateAntigravityResponseStream(cfg, to, chunk, model, msgID, unifiedState)
	case "gemini", "aistudio":
		chunks, err = TranslateGeminiResponseStream(cfg, to, chunk, model, msgID, unifiedState)
	case "openai", "cline", "ollama":
		chunks, err = TranslateOpenAIResponseStream(cfg, to, chunk, model, msgID, unifiedState)
	case "claude":
		// Claude wrapper still uses specific state type for consistency with parser
		if s, ok := state.(*from_ir.ClaudeStreamState); ok {
			chunks, err = TranslateClaudeResponseStream(cfg, to, chunk, model, msgID, s)
		} else {
			// Fallback/Error if state mismatch
			return nil, fmt.Errorf("invalid state type for claude stream")
		}
	case "codex":
		events, err := to_ir.ParseCodexChunk(chunk)
		if err == nil {
			chunks, err = convertUnifiedEventsToChunks(events, to, model, msgID, unifiedState)
		}
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}

	if err != nil {
		return nil, err
	}
	for i := range chunks {
		chunks[i] = ensureColonSpacedJSON(chunks[i])
	}
	return chunks, nil
}

// =========================================================================================
// OpenAI Request Format Constants (exported for SDK adapter)
// =========================================================================================

const (
	FormatChatCompletions = from_ir.FormatChatCompletions
	FormatResponsesAPI    = from_ir.FormatResponsesAPI
)

// =========================================================================================
// Utilities
// =========================================================================================

// applyPayloadConfigToIR applies YAML payload config rules.
func applyPayloadConfigToIR(cfg *config.Config, model string, payload []byte) []byte {
	if cfg == nil || len(payload) == 0 {
		return payload
	}

	// Default rules
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

	// Override rules
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

func matchesPattern(pattern, name string) bool {
	if pattern == name || pattern == "*" {
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

// TranslateOpenAIResponseStreamForced and others are deprecated wrappers
func TranslateOpenAIResponseStreamForced(to sdktranslator.Format, chunk []byte, model, msgID string, state *UnifiedStreamState) ([][]byte, error) {
	return TranslateOpenAIResponseStream(nil, to, chunk, model, msgID, state)
}

func TranslateOpenAIResponseNonStreamForced(to sdktranslator.Format, resp []byte, model string) ([]byte, error) {
	return TranslateOpenAIResponseNonStream(nil, to, resp, model)
}
