/**
 * @file Claude API request converter
 * @description Converts unified requests to Claude Messages API format.
 */

package from_ir

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// Claude user tracking
var (
	claudeUser    = ""
	claudeAccount = ""
	claudeSession = ""
)

// ClaudeProvider handles conversion to Claude Messages API format.
type ClaudeProvider struct{}

// ClaudeStreamState tracks state for streaming response conversion.
type ClaudeStreamState struct {
	MessageID              string
	Model                  string
	SessionID              string
	CurrentThinkingText    strings.Builder
	TextBlockIndex         int
	NextContentBlockIndex  int
	ActiveContentBlockType string
	ToolBlockCount         int
	CurrentToolBlockIndex  int
	MessageStartSent       bool
	TextBlockStarted       bool
	TextBlockStopped       bool
	HasToolCalls           bool
	HasContent             bool
	FinishSent             bool
}

func NewClaudeStreamState() *ClaudeStreamState {
	return &ClaudeStreamState{TextBlockIndex: 0, NextContentBlockIndex: 0, ToolBlockCount: 0}
}

func NewClaudeStreamStateWithSessionID(sessionID string) *ClaudeStreamState {
	return &ClaudeStreamState{TextBlockIndex: 0, NextContentBlockIndex: 0, ToolBlockCount: 0, SessionID: sessionID}
}

// DeriveSessionID generates a stable session ID from the request.
func DeriveSessionID(rawJSON []byte) string {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.IsArray() {
		return ""
	}
	for _, msg := range messages.Array() {
		if msg.Get("role").String() == "user" {
			content := msg.Get("content").String()
			if content == "" {
				content = msg.Get("content.0.text").String()
			}
			if content != "" {
				h := sha256.Sum256([]byte(content))
				return hex.EncodeToString(h[:16])
			}
		}
	}
	return ""
}

// ConvertRequest transforms unified request into Claude Messages API JSON.
func (p *ClaudeProvider) ConvertRequest(req *ir.UnifiedChatRequest) ([]byte, error) {
	ensureClaudeUser()
	userID := fmt.Sprintf("user_%s_account_%s_session_%s", claudeUser, claudeAccount, claudeSession)

	root := map[string]interface{}{
		"model":      req.Model,
		"max_tokens": ir.ClaudeDefaultMaxTokens,
		"metadata":   map[string]interface{}{"user_id": userID},
		"messages":   []interface{}{},
	}

	if req.MaxTokens != nil {
		root["max_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		root["temperature"] = *req.Temperature
	} else if req.TopP != nil {
		root["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		root["top_k"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		root["stop_sequences"] = req.StopSequences
	}

	if req.Thinking != nil {
		applyThinkingConfig(root, req.Thinking)
	}

	messages := buildMessages(req.Messages)
	root["messages"] = messages

	if len(req.Tools) > 0 {
		root["tools"] = buildTools(req.Tools)
	}

	if len(req.Metadata) > 0 {
		meta := root["metadata"].(map[string]interface{})
		for k, v := range req.Metadata {
			meta[k] = v
		}
	}

	return json.Marshal(root)
}

func ensureClaudeUser() {
	if claudeAccount == "" {
		u, _ := uuid.NewRandom()
		claudeAccount = u.String()
	}
	if claudeSession == "" {
		u, _ := uuid.NewRandom()
		claudeSession = u.String()
	}
	if claudeUser == "" {
		sum := sha256.Sum256([]byte(claudeAccount + claudeSession))
		claudeUser = hex.EncodeToString(sum[:])
	}
}

func applyThinkingConfig(root map[string]interface{}, thinking *ir.ThinkingConfig) {
	t := map[string]interface{}{}
	if thinking.Effort != "" {
		// Adaptive/auto thinking with explicit effort level (Claude 4.6+).
		// Emit thinking.type=adaptive + output_config.effort instead of budget_tokens.
		t["type"] = "adaptive"
		root["output_config"] = map[string]interface{}{"effort": thinking.Effort}
	} else if thinking.IncludeThoughts && thinking.Budget != 0 {
		t["type"] = "enabled"
		if thinking.Budget > 0 {
			t["budget_tokens"] = thinking.Budget
		}
	} else if thinking.Budget == 0 {
		t["type"] = "disabled"
	}
	if len(t) > 0 {
		root["thinking"] = t
	}
}

func buildMessages(msgs []ir.Message) []interface{} {
	var messages []interface{}
	for _, msg := range msgs {
		switch msg.Role {
		case ir.RoleSystem:
			// System messages are handled at root level in ConvertRequest but IR structure keeps them in messages
		case ir.RoleUser:
			if parts := buildClaudeContentParts(msg, false); len(parts) > 0 {
				messages = append(messages, map[string]interface{}{"role": ir.ClaudeRoleUser, "content": parts})
			}
		case ir.RoleAssistant:
			if parts := buildClaudeContentParts(msg, true); len(parts) > 0 {
				messages = append(messages, map[string]interface{}{"role": ir.ClaudeRoleAssistant, "content": parts})
			}
		case ir.RoleTool:
			for _, part := range msg.Content {
				if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
					toolResultBlock := map[string]interface{}{
						"type": ir.ClaudeBlockToolResult, "tool_use_id": part.ToolResult.ToolCallID, "content": part.ToolResult.Result,
					}
					// Include images from tool results as content array with image blocks.
					if len(part.ToolResult.Images) > 0 {
						var contentParts []interface{}
						if part.ToolResult.Result != "" {
							contentParts = append(contentParts, map[string]interface{}{
								"type": "text", "text": part.ToolResult.Result,
							})
						}
						for _, img := range part.ToolResult.Images {
							contentParts = append(contentParts, map[string]interface{}{
								"type": "image",
								"source": map[string]interface{}{
									"type":       "base64",
									"media_type": img.MimeType,
									"data":       img.Data,
								},
							})
						}
						toolResultBlock["content"] = contentParts
					}
					messages = append(messages, map[string]interface{}{
						"role":    ir.ClaudeRoleUser,
						"content": []interface{}{toolResultBlock},
					})
				}
			}
		}
	}
	return messages
}

func (p *ClaudeProvider) ParseResponse(responseJSON []byte) ([]ir.Message, *ir.Usage, error) {
	if !gjson.ValidBytes(responseJSON) {
		return nil, nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}
	parsed := gjson.ParseBytes(responseJSON)
	usage := ir.ParseClaudeUsage(parsed.Get("usage"))

	content := parsed.Get("content")
	if !content.Exists() || !content.IsArray() {
		return nil, usage, nil
	}

	msg := ir.Message{Role: ir.RoleAssistant}
	for _, block := range content.Array() {
		ir.ParseClaudeContentBlock(block, &msg)
	}

	if len(msg.Content) == 0 && len(msg.ToolCalls) == 0 {
		return nil, usage, nil
	}
	return []ir.Message{msg}, usage, nil
}

func (p *ClaudeProvider) ParseStreamChunk(chunkJSON []byte) ([]ir.UnifiedEvent, error) {
	return p.ParseStreamChunkWithState(chunkJSON, nil)
}

func (p *ClaudeProvider) ParseStreamChunkWithState(chunkJSON []byte, state *ir.ClaudeStreamParserState) ([]ir.UnifiedEvent, error) {
	data := ir.ExtractSSEData(chunkJSON)
	if len(data) == 0 || !gjson.ValidBytes(data) {
		return nil, nil
	}

	parsed := gjson.ParseBytes(data)
	switch parsed.Get("type").String() {
	case ir.ClaudeSSEContentBlockStart:
		return ir.ParseClaudeContentBlockStart(parsed, state), nil
	case ir.ClaudeSSEContentBlockDelta:
		if state != nil {
			return ir.ParseClaudeStreamDeltaWithState(parsed, state), nil
		}
		return ir.ParseClaudeStreamDelta(parsed), nil
	case ir.ClaudeSSEContentBlockStop:
		return ir.ParseClaudeContentBlockStop(parsed, state), nil
	case ir.ClaudeSSEMessageDelta:
		return ir.ParseClaudeMessageDelta(parsed), nil
	case ir.ClaudeSSEMessageStop:
		return []ir.UnifiedEvent{{Type: ir.EventTypeFinish, FinishReason: ir.FinishReasonStop}}, nil
	case ir.ClaudeSSEError:
		msg := parsed.Get("error.message").String()
		if msg == "" {
			msg = "Unknown Claude API error"
		}
		return []ir.UnifiedEvent{{Type: ir.EventTypeError, Error: fmt.Errorf("%s", msg)}}, nil
	}
	return nil, nil
}

func ToClaudeSSE(event ir.UnifiedEvent, model, messageID string, state *ClaudeStreamState) ([]byte, error) {
	var result strings.Builder

	if state != nil && !state.MessageStartSent {
		state.MessageStartSent = true
		state.Model, state.MessageID = model, messageID
		result.WriteString(formatSSE(ir.ClaudeSSEMessageStart, map[string]interface{}{
			"type": ir.ClaudeSSEMessageStart,
			"message": map[string]interface{}{
				"id": messageID, "type": "message", "role": ir.ClaudeRoleAssistant,
				"content": []interface{}{}, "model": model, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]interface{}{"input_tokens": 0, "output_tokens": 0},
			},
		}))
	}

	switch event.Type {
	case ir.EventTypeToken:
		result.WriteString(emitTextDelta(event.Content, state))
	case ir.EventTypeReasoning:
		// Always emit thinking_delta for reasoning content
		// ThoughtSignature like "xai-responses-v1" is a format identifier, not a cryptographic signature
		// Real signatures are longer and used for verification (e.g., Claude's signature_delta)
		if event.Reasoning != "" {
			result.WriteString(emitThinkingDelta(event.Reasoning, state))
		}
		// Only emit signature_delta for real cryptographic signatures (not format identifiers)
		// Real signatures are typically longer than 30 chars and don't contain common format patterns
		if event.ThoughtSignature != "" && len(event.ThoughtSignature) > 30 &&
			!strings.Contains(event.ThoughtSignature, "responses-v") &&
			!strings.Contains(event.ThoughtSignature, "format") {
			result.WriteString(emitSignatureDelta(event.ThoughtSignature, state))
		}
	case ir.EventTypeToolCall:
		if event.ToolCall != nil {
			result.WriteString(emitToolCall(event.ToolCall, state))
		}
	case ir.EventTypeToolCallDelta:
		// Handle streaming tool call argument deltas
		if event.ToolCall != nil && state != nil {
			result.WriteString(emitToolCallDelta(event.ToolCall, state))
		}
	case ir.EventTypeFinish:
		if state != nil && state.FinishSent {
			return nil, nil
		}
		if state != nil {
			state.FinishSent = true
		}
		result.WriteString(emitFinish(event.Usage, event.FinishReason, state))
	case ir.EventTypeError:
		result.WriteString(formatSSE(ir.ClaudeSSEError, map[string]interface{}{
			"type": ir.ClaudeSSEError, "error": map[string]interface{}{"type": "api_error", "message": errMsg(event.Error)},
		}))
	}

	if result.Len() == 0 {
		return nil, nil
	}
	return []byte(result.String()), nil
}

func ToClaudeResponse(messages []ir.Message, usage *ir.Usage, model, messageID string) ([]byte, error) {
	builder := ir.NewResponseBuilder(messages, usage, model)
	response := map[string]interface{}{
		"id": messageID, "type": "message", "role": ir.ClaudeRoleAssistant,
		"content": builder.BuildClaudeContentParts(), "model": model, "stop_reason": ir.ClaudeStopEndTurn,
	}
	if builder.HasToolCalls() {
		response["stop_reason"] = ir.ClaudeStopToolUse
	}
	if usage != nil {
		response["usage"] = map[string]interface{}{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens}
	}
	return json.Marshal(response)
}

func buildClaudeContentParts(msg ir.Message, includeToolCalls bool) []interface{} {
	var parts []interface{}
	for _, p := range msg.Content {
		switch p.Type {
		case ir.ContentTypeReasoning:
			if p.Reasoning != "" {
				parts = append(parts, map[string]interface{}{"type": ir.ClaudeBlockThinking, "thinking": p.Reasoning})
			}
		case ir.ContentTypeText:
			if p.Text != "" {
				parts = append(parts, map[string]interface{}{"type": ir.ClaudeBlockText, "text": p.Text})
			}
		case ir.ContentTypeImage:
			if p.Image != nil {
				parts = append(parts, map[string]interface{}{
					"type":   ir.ClaudeBlockImage,
					"source": map[string]interface{}{"type": "base64", "media_type": p.Image.MimeType, "data": p.Image.Data},
				})
			}
		case ir.ContentTypeFile:
			// Convert file content to Claude document format.
			// Supports data URI format (data:application/pdf;base64,...) and raw base64.
			if p.File != nil && p.File.FileData != "" {
				mediaType := "application/octet-stream"
				data := p.File.FileData
				if strings.HasPrefix(p.File.FileData, "data:") {
					trimmed := strings.TrimPrefix(p.File.FileData, "data:")
					mediaAndData := strings.SplitN(trimmed, ";base64,", 2)
					if len(mediaAndData) == 2 {
						if mediaAndData[0] != "" {
							mediaType = mediaAndData[0]
						}
						data = mediaAndData[1]
					}
				}
				parts = append(parts, map[string]interface{}{
					"type": "document",
					"source": map[string]interface{}{
						"type":       "base64",
						"media_type": mediaType,
						"data":       data,
					},
				})
			}
		case ir.ContentTypeToolResult:
			if p.ToolResult != nil {
				toolResultBlock := map[string]interface{}{
					"type": ir.ClaudeBlockToolResult, "tool_use_id": p.ToolResult.ToolCallID, "content": p.ToolResult.Result,
				}
				// Include images from tool results as content array with image blocks.
				if len(p.ToolResult.Images) > 0 {
					var contentParts []interface{}
					if p.ToolResult.Result != "" {
						contentParts = append(contentParts, map[string]interface{}{
							"type": "text", "text": p.ToolResult.Result,
						})
					}
					for _, img := range p.ToolResult.Images {
						contentParts = append(contentParts, map[string]interface{}{
							"type": "image",
							"source": map[string]interface{}{
								"type":       "base64",
								"media_type": img.MimeType,
								"data":       img.Data,
							},
						})
					}
					toolResultBlock["content"] = contentParts
				}
				parts = append(parts, toolResultBlock)
			}
		}
	}
	if includeToolCalls {
		for _, tc := range msg.ToolCalls {
			toolUse := map[string]interface{}{"type": ir.ClaudeBlockToolUse, "id": tc.ID, "name": tc.Name}
			input := ir.ParseToolCallArgs(tc.Args)
			// Remove null values from tool input (Roo/Kilo compatibility)
			cleanedInput := ir.RemoveNullsFromToolInput(input)
			if cleanedMap, ok := cleanedInput.(map[string]interface{}); ok {
				toolUse["input"] = cleanedMap
			} else {
				toolUse["input"] = input
			}
			parts = append(parts, toolUse)
		}
	}
	return parts
}

func buildTools(tools []ir.ToolDefinition) []interface{} {
	var result []interface{}
	for _, t := range tools {
		tool := map[string]interface{}{"name": t.Name, "description": t.Description}
		if len(t.Parameters) > 0 {
			// Use centralized schema cleaner for Claude to ensure parity with AntigravityExecutor
			// This handles const->enum, type flattening, and unsupported keywords.
			// Since util.CleanJSONSchemaForAntigravity adds placeholder schemas which Claude supports,
			// it's a good fit here.
			if jsonBytes, err := json.Marshal(ir.CopyMap(t.Parameters)); err == nil {
				cleanedStr := util.CleanJSONSchemaForAntigravity(string(jsonBytes))
				var cleanedMap map[string]interface{}
				if err := json.Unmarshal([]byte(cleanedStr), &cleanedMap); err == nil {
					tool["input_schema"] = cleanedMap
				} else {
					tool["input_schema"] = ir.CleanJsonSchemaEnhanced(ir.CopyMap(t.Parameters))
				}
			} else {
				tool["input_schema"] = ir.CleanJsonSchemaEnhanced(ir.CopyMap(t.Parameters))
			}
		} else {
			tool["input_schema"] = map[string]interface{}{
				"type": "object", "properties": map[string]interface{}{}, "additionalProperties": false, "$schema": "http://json-schema.org/draft-07/schema#",
			}
		}
		result = append(result, tool)
	}
	return result
}

func formatSSE(eventType string, data interface{}) string {
	jsonData, _ := json.Marshal(data)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, string(jsonData))
}

func ensureContentBlock(state *ClaudeStreamState, blockType string) string {
	if state == nil {
		return ""
	}

	var result strings.Builder
	buildBlock := func(idx int, typ string) {
		contentBlock := map[string]interface{}{"type": typ}
		if typ == ir.ClaudeBlockThinking {
			contentBlock["thinking"] = ""
		} else {
			contentBlock["text"] = ""
		}
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStart, map[string]interface{}{
			"type":          ir.ClaudeSSEContentBlockStart,
			"index":         idx,
			"content_block": contentBlock,
		}))
	}

	if !state.TextBlockStarted || state.TextBlockStopped {
		state.TextBlockIndex = state.NextContentBlockIndex
		state.NextContentBlockIndex++
		state.TextBlockStarted = true
		state.TextBlockStopped = false
		state.ActiveContentBlockType = blockType
		buildBlock(state.TextBlockIndex, blockType)
		return result.String()
	}

	if state.ActiveContentBlockType != blockType {
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStop, map[string]interface{}{
			"type":  ir.ClaudeSSEContentBlockStop,
			"index": state.TextBlockIndex,
		}))
		state.TextBlockStopped = true

		state.TextBlockIndex = state.NextContentBlockIndex
		state.NextContentBlockIndex++
		state.TextBlockStopped = false
		state.ActiveContentBlockType = blockType
		buildBlock(state.TextBlockIndex, blockType)
	}

	return result.String()
}

func emitTextDelta(text string, state *ClaudeStreamState) string {
	var result strings.Builder
	idx := 0
	if state != nil {
		result.WriteString(ensureContentBlock(state, ir.ClaudeBlockText))
		idx = state.TextBlockIndex
		state.HasContent = true
	}
	result.WriteString(formatSSE(ir.ClaudeSSEContentBlockDelta, map[string]interface{}{
		"type": ir.ClaudeSSEContentBlockDelta, "index": idx,
		"delta": map[string]interface{}{"type": "text_delta", "text": text},
	}))
	return result.String()
}

func emitThinkingDelta(thinking string, state *ClaudeStreamState) string {
	var result strings.Builder
	idx := 0
	if state != nil {
		result.WriteString(ensureContentBlock(state, ir.ClaudeBlockThinking))
		idx = state.TextBlockIndex
		state.HasContent = true
		state.CurrentThinkingText.WriteString(thinking)
	}
	result.WriteString(formatSSE(ir.ClaudeSSEContentBlockDelta, map[string]interface{}{
		"type": ir.ClaudeSSEContentBlockDelta, "index": idx,
		"delta": map[string]interface{}{"type": "thinking_delta", "thinking": thinking},
	}))
	return result.String()
}

func emitSignatureDelta(signature string, state *ClaudeStreamState) string {
	var result strings.Builder
	idx := 0
	if state != nil {
		result.WriteString(ensureContentBlock(state, ir.ClaudeBlockThinking))
		idx = state.TextBlockIndex
		state.HasContent = true

		if state.SessionID != "" && state.CurrentThinkingText.Len() > 0 {
			cache.CacheSignature(state.SessionID, state.CurrentThinkingText.String(), signature)
			log.Debugf("Cached signature for thinking block (sessionID=%s, textLen=%d)", state.SessionID, state.CurrentThinkingText.Len())
			state.CurrentThinkingText.Reset()
		}
	}
	result.WriteString(formatSSE(ir.ClaudeSSEContentBlockDelta, map[string]interface{}{
		"type": ir.ClaudeSSEContentBlockDelta, "index": idx,
		"delta": map[string]interface{}{"type": "signature_delta", "signature": signature},
	}))
	return result.String()
}

func emitToolCall(tc *ir.ToolCall, state *ClaudeStreamState) string {
	var result strings.Builder

	// Claude API requires an initial content block before tool_use blocks.
	if state != nil && !state.TextBlockStarted {
		emptyIdx := state.NextContentBlockIndex
		state.NextContentBlockIndex++
		state.TextBlockStarted = true
		state.TextBlockStopped = true
		state.ActiveContentBlockType = ir.ClaudeBlockText
		state.TextBlockIndex = emptyIdx
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStart, map[string]interface{}{
			"type": ir.ClaudeSSEContentBlockStart, "index": emptyIdx,
			"content_block": map[string]interface{}{"type": ir.ClaudeBlockText, "text": ""},
		}))
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStop, map[string]interface{}{"type": ir.ClaudeSSEContentBlockStop, "index": emptyIdx}))
	} else if state != nil && state.TextBlockStarted && !state.TextBlockStopped {
		state.TextBlockStopped = true
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStop, map[string]interface{}{"type": ir.ClaudeSSEContentBlockStop, "index": state.TextBlockIndex}))
	}

	idx := 1
	if state != nil {
		state.HasToolCalls = true
		state.HasContent = true
		idx = state.NextContentBlockIndex
		state.NextContentBlockIndex++
		state.ToolBlockCount++
		state.CurrentToolBlockIndex = idx
	}

	result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStart, map[string]interface{}{
		"type": ir.ClaudeSSEContentBlockStart, "index": idx,
		"content_block": map[string]interface{}{"type": ir.ClaudeBlockToolUse, "id": tc.ID, "name": tc.Name, "input": map[string]interface{}{}},
	}))

	// Emit initial args delta if present
	args := tc.Args
	if args != "" {
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockDelta, map[string]interface{}{
			"type": ir.ClaudeSSEContentBlockDelta, "index": idx,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": args},
		}))
	}

	return result.String()
}

// emitToolCallDelta emits input_json_delta for streaming tool call arguments
func emitToolCallDelta(tc *ir.ToolCall, state *ClaudeStreamState) string {
	if tc == nil || state == nil {
		return ""
	}

	idx := state.CurrentToolBlockIndex
	if idx == 0 {
		// No tool block started yet, skip delta
		return ""
	}

	var result strings.Builder

	// Emit args delta if present
	if tc.Args != "" {
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockDelta, map[string]interface{}{
			"type": ir.ClaudeSSEContentBlockDelta, "index": idx,
			"delta": map[string]interface{}{"type": "input_json_delta", "partial_json": tc.Args},
		}))
	}

	// Close the content block if complete
	if tc.IsComplete {
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStop, map[string]interface{}{
			"type": ir.ClaudeSSEContentBlockStop, "index": idx,
		}))
		state.CurrentToolBlockIndex = 0
	}

	return result.String()
}

func emitFinish(usage *ir.Usage, finishReason ir.FinishReason, state *ClaudeStreamState) string {
	if state != nil && !state.HasContent {
		return ""
	}

	var result strings.Builder
	if state != nil && state.TextBlockStarted && !state.TextBlockStopped {
		result.WriteString(formatSSE(ir.ClaudeSSEContentBlockStop, map[string]interface{}{
			"type":  ir.ClaudeSSEContentBlockStop,
			"index": state.TextBlockIndex,
		}))
		state.TextBlockStopped = true
	}

	stopReason := ir.ClaudeStopEndTurn
	if state != nil && state.HasToolCalls {
		stopReason = ir.ClaudeStopToolUse
	} else if finishReason == ir.FinishReasonLength {
		stopReason = ir.ClaudeStopMaxTokens
	}

	delta := map[string]interface{}{"type": ir.ClaudeSSEMessageDelta, "delta": map[string]interface{}{"stop_reason": stopReason}}
	if usage != nil {
		delta["usage"] = map[string]interface{}{"input_tokens": usage.PromptTokens, "output_tokens": usage.CompletionTokens}
	}
	result.WriteString(formatSSE(ir.ClaudeSSEMessageDelta, delta))
	result.WriteString(formatSSE(ir.ClaudeSSEMessageStop, map[string]interface{}{"type": ir.ClaudeSSEMessageStop}))
	return result.String()
}

func errMsg(err error) string {
	if err != nil {
		return err.Error()
	}
	return "Unknown error"
}
