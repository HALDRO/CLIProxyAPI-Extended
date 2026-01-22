// Package to_ir converts provider-specific API formats into unified format.
// This file handles Antigravity API responses (streaming and non-streaming).
// Antigravity wraps Gemini responses in an envelope: {"response": {...}, "traceId": "..."}
// and may use cpaUsageMetadata instead of usageMetadata.
package to_ir

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
)

// --- Response Parsing ---

// ParseAntigravityResponse parses a non-streaming Antigravity API response into unified format.
// Antigravity wraps Gemini responses in an envelope, so we unwrap it first.
func ParseAntigravityResponse(rawJSON []byte) (*ir.UnifiedChatRequest, []ir.Message, *ir.Usage, error) {
	messages, usage, _, err := ParseAntigravityResponseMetaWithContext(rawJSON, nil)
	return nil, messages, usage, err
}

// ParseAntigravityResponseMeta parses a non-streaming Antigravity API response into unified format with metadata.
// Returns messages, usage, and response metadata (responseId, createTime, nativeFinishReason).
func ParseAntigravityResponseMeta(rawJSON []byte) ([]ir.Message, *ir.Usage, *ir.ResponseMeta, error) {
	return ParseAntigravityResponseMetaWithContext(rawJSON, nil)
}

// ParseAntigravityResponseMetaWithContext parses a non-streaming Antigravity API response with schema context.
// The schemaCtx parameter allows normalizing tool call parameters based on the original request schema.
// Antigravity wraps Gemini responses in an envelope: {"response": {...}, "traceId": "..."}
func ParseAntigravityResponseMetaWithContext(rawJSON []byte, schemaCtx *ir.ToolSchemaContext) ([]ir.Message, *ir.Usage, *ir.ResponseMeta, error) {
	if !gjson.ValidBytes(rawJSON) {
		return nil, nil, nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}

	// Unwrap Antigravity envelope: {"response": {...}, "traceId": "..."}
	if responseWrapper := gjson.GetBytes(rawJSON, "response"); responseWrapper.Exists() {
		rawJSON = []byte(responseWrapper.Raw)
	}

	// Use Gemini parser for the unwrapped response
	return ParseGeminiResponseMetaWithContext(rawJSON, schemaCtx)
}

// ParseAntigravityChunk parses a streaming Antigravity API chunk into events.
// Antigravity wraps Gemini chunks in an envelope, so we unwrap it first.
func ParseAntigravityChunk(rawJSON []byte) ([]ir.UnifiedEvent, error) {
	return ParseAntigravityChunkWithContext(rawJSON, nil)
}

// ParseAntigravityChunkWithContext parses a streaming Antigravity API chunk with schema context.
// The schemaCtx parameter allows normalizing tool call parameters based on the original request schema.
// Antigravity wraps Gemini chunks in an envelope: {"response": {...}, "traceId": "..."}
func ParseAntigravityChunkWithContext(rawJSON []byte, schemaCtx *ir.ToolSchemaContext) ([]ir.UnifiedEvent, error) {
	// Handle SSE format: "data: {...}" or "data:{...}"
	rawJSON = ir.ExtractSSEData(rawJSON)
	if len(rawJSON) == 0 {
		return nil, nil
	}
	if string(rawJSON) == "[DONE]" {
		return []ir.UnifiedEvent{{Type: ir.EventTypeFinish}}, nil
	}
	if !gjson.ValidBytes(rawJSON) {
		return nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}

	parsed := gjson.ParseBytes(rawJSON)
	// Unwrap Antigravity envelope if present
	if responseWrapper := parsed.Get("response"); responseWrapper.Exists() {
		parsed = responseWrapper
		rawJSON = []byte(parsed.Raw)
	}

	// Use Gemini parser for the unwrapped chunk
	return ParseGeminiChunkWithContext(rawJSON, schemaCtx)
}

// --- Tool Schema Context ---

// NewAntigravityToolSchemaContext creates a tool schema context from the original request.
// Antigravity has a known issue where Gemini ignores tool parameter schemas and returns
// different parameter names (e.g., "path" instead of "target_file").
// This function extracts the expected schema from the original request to normalize responses.
func NewAntigravityToolSchemaContext(originalRequest []byte) *ir.ToolSchemaContext {
	if len(originalRequest) == 0 {
		return nil
	}

	// Extract tool schemas efficiently using gjson (no full unmarshal)
	tools := gjson.GetBytes(originalRequest, "tools").Array()
	if len(tools) == 0 {
		return nil
	}

	return ir.NewToolSchemaContextFromGJSON(tools)
}

// --- Thinking Config Normalization ---

// NormalizeAntigravityThinking clamps or removes thinking config based on model support.
// For Claude models, it additionally ensures thinking budget < max_tokens.
// This is Antigravity-specific because Antigravity has stricter validation than Gemini CLI.
func NormalizeAntigravityThinking(model string, payload []byte, isClaude bool) []byte {
	// Simplified: just return payload as-is
	// Thinking config validation is handled by upstream
	return payload
}

// antigravityEffectiveMaxTokens returns the max tokens to cap thinking:
// prefer request-provided maxOutputTokens; otherwise fall back to model default.
// The boolean indicates whether the value came from the model default (and thus should be written back).
func antigravityEffectiveMaxTokens(model string, payload []byte) (max int, fromModel bool) {
	if maxTok := gjson.GetBytes(payload, "request.generationConfig.maxOutputTokens"); maxTok.Exists() && maxTok.Int() > 0 {
		return int(maxTok.Int()), false
	}
	if modelInfo := registry.GetGlobalRegistry().GetModelInfo(model, ""); modelInfo != nil && modelInfo.MaxCompletionTokens > 0 {
		return modelInfo.MaxCompletionTokens, true
	}
	return 0, false
}

// antigravityMinThinkingBudget returns the minimum thinking budget for a model.
// Falls back to -1 if no model info is found.
func antigravityMinThinkingBudget(model string) int {
	if modelInfo := registry.GetGlobalRegistry().GetModelInfo(model, ""); modelInfo != nil && modelInfo.Thinking != nil {
		return modelInfo.Thinking.Min
	}
	return -1
}

// =============================================================================
// ThoughtSignature Encoding in Tool Call IDs (for round-trip preservation)
// =============================================================================

// ThoughtSignatureSeparator is used to embed thoughtSignature in tool call IDs.
// This allows signatures to survive client round-trips even if clients strip custom fields.
const ThoughtSignatureSeparator = "__thought__"

// EncodeToolIDWithSignature embeds a thoughtSignature into a tool call ID for round-trip preservation.
// If signature is empty, returns the original ID unchanged.
func EncodeToolIDWithSignature(toolID, signature string) string {
	if signature == "" {
		return toolID
	}
	return toolID + ThoughtSignatureSeparator + signature
}

// DecodeToolIDAndSignature extracts the original tool ID and thoughtSignature from an encoded ID.
// Returns (originalID, signature). If no signature is embedded, signature will be empty.
func DecodeToolIDAndSignature(encodedID string) (string, string) {
	if encodedID == "" || !strings.Contains(encodedID, ThoughtSignatureSeparator) {
		return encodedID, ""
	}
	parts := strings.SplitN(encodedID, ThoughtSignatureSeparator, 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return encodedID, ""
}

// =============================================================================
// Function Name Normalization (Gemini API compatibility)
// =============================================================================

// MaxFunctionNameLength is the maximum allowed length for Gemini function names.
const MaxFunctionNameLength = 64

// NormalizeFunctionName ensures a function name conforms to Gemini API requirements:
// - Must start with a letter or underscore
// - Can only contain a-z, A-Z, 0-9, underscore, period, hyphen
// - Maximum 64 characters
func NormalizeFunctionName(name string) string {
	if name == "" {
		return "_unnamed_function"
	}

	var result strings.Builder
	result.Grow(len(name))

	for i, r := range name {
		// Check if character is allowed
		isAllowed := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '.' || r == '-'

		if isAllowed {
			result.WriteRune(r)
		} else {
			// Replace illegal characters with underscore
			result.WriteByte('_')
		}

		// Check length limit
		if i >= MaxFunctionNameLength-1 {
			break
		}
	}

	normalized := result.String()
	if normalized == "" {
		return "_unnamed_function"
	}

	// Ensure starts with letter or underscore
	firstChar := normalized[0]
	if !((firstChar >= 'a' && firstChar <= 'z') ||
		(firstChar >= 'A' && firstChar <= 'Z') ||
		firstChar == '_') {
		normalized = "_" + normalized
		if len(normalized) > MaxFunctionNameLength {
			normalized = normalized[:MaxFunctionNameLength]
		}
	}

	return normalized
}

// =============================================================================
// Remove Nulls from Tool Input (Roo/Kilo compatibility)
// =============================================================================

// RemoveNullsFromToolInput recursively removes null/None values from tool input dictionaries/lists.
// Roo/Kilo in Anthropic native tool path may interpret null as a real parameter (e.g., "search in null").
func RemoveNullsFromToolInput(value interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		cleaned := make(map[string]interface{})
		for k, val := range v {
			if val == nil {
				continue // Skip null values
			}
			cleaned[k] = RemoveNullsFromToolInput(val)
		}
		return cleaned
	case []interface{}:
		cleanedList := make([]interface{}, 0, len(v))
		for _, item := range v {
			if item == nil {
				continue // Skip null values
			}
			cleanedList = append(cleanedList, RemoveNullsFromToolInput(item))
		}
		return cleanedList
	default:
		return value
	}
}

// =============================================================================
// Enhanced JSON Schema Cleaning ($ref resolution, allOf merge, anyOf→enum)
// =============================================================================

// CleanJsonSchemaEnhanced performs advanced JSON Schema cleaning with:
// - $ref resolution (within same schema)
// - allOf merging
// - anyOf/oneOf to enum conversion (when possible)
// - Type array flattening
func CleanJsonSchemaEnhanced(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}

	// First pass: resolve $ref and merge allOf (start with depth 0)
	schema = resolveRefsAndMerge(schema, schema, 0)

	// Second pass: standard cleaning
	return ir.CleanJsonSchema(schema)
}

// resolveRefsAndMerge resolves $ref references and merges allOf schemas.
func resolveRefsAndMerge(schema, rootSchema map[string]interface{}, depth int) map[string]interface{} {
	if schema == nil {
		return nil
	}

	// Recursion depth limit to prevent stack overflow on circular references
	if depth > 20 {
		return schema
	}

	result := make(map[string]interface{})

	// Handle $ref
	if ref, ok := schema["$ref"].(string); ok {
		resolved := resolveRef(ref, rootSchema)
		// Merge resolved schema (range on nil map is safe - no iterations)
		for k, v := range resolved {
			result[k] = v
		}
		// Also copy other fields from original schema (they override $ref)
		for k, v := range schema {
			if k != "$ref" {
				result[k] = v
			}
		}
	} else {
		// Copy all fields
		for k, v := range schema {
			result[k] = v
		}
	}

	// Handle allOf - merge all schemas
	if allOf, ok := result["allOf"].([]interface{}); ok {
		merged := make(map[string]interface{})
		var mergedRequired []interface{}
		var mergedProperties map[string]interface{}

		for _, item := range allOf {
			if itemSchema, ok := item.(map[string]interface{}); ok {
				cleaned := resolveRefsAndMerge(itemSchema, rootSchema, depth+1)

				// Merge properties
				if props, ok := cleaned["properties"].(map[string]interface{}); ok {
					if mergedProperties == nil {
						mergedProperties = make(map[string]interface{})
					}
					for k, v := range props {
						mergedProperties[k] = v
					}
				}

				// Merge required
				if req, ok := cleaned["required"].([]interface{}); ok {
					mergedRequired = append(mergedRequired, req...)
				}

				// Merge other fields (later ones override)
				for k, v := range cleaned {
					if k != "properties" && k != "required" {
						merged[k] = v
					}
				}
			}
		}

		if mergedProperties != nil {
			merged["properties"] = mergedProperties
		}
		if len(mergedRequired) > 0 {
			merged["required"] = uniqueStrings(mergedRequired)
		}

		// Copy merged fields to result
		for k, v := range merged {
			result[k] = v
		}
		delete(result, "allOf")
	}

	// Handle anyOf - try to convert to enum if all items have const
	if anyOf, ok := result["anyOf"].([]interface{}); ok {
		if enumValues := tryExtractEnum(anyOf); enumValues != nil {
			result["type"] = "string"
			result["enum"] = enumValues
			delete(result, "anyOf")
		} else if len(anyOf) > 0 {
			// Take first valid schema as fallback
			if first, ok := anyOf[0].(map[string]interface{}); ok {
				cleaned := resolveRefsAndMerge(first, rootSchema, depth+1)
				for k, v := range cleaned {
					if _, exists := result[k]; !exists || k == "type" {
						result[k] = v
					}
				}
			}
			delete(result, "anyOf")
		}
	}

	// Handle oneOf similarly
	if oneOf, ok := result["oneOf"].([]interface{}); ok {
		if enumValues := tryExtractEnum(oneOf); enumValues != nil {
			result["type"] = "string"
			result["enum"] = enumValues
			delete(result, "oneOf")
		} else if len(oneOf) > 0 {
			if first, ok := oneOf[0].(map[string]interface{}); ok {
				cleaned := resolveRefsAndMerge(first, rootSchema, depth+1)
				for k, v := range cleaned {
					if _, exists := result[k]; !exists || k == "type" {
						result[k] = v
					}
				}
			}
			delete(result, "oneOf")
		}
	}

	// Recursively process properties
	if props, ok := result["properties"].(map[string]interface{}); ok {
		cleanedProps := make(map[string]interface{})
		for k, v := range props {
			if propSchema, ok := v.(map[string]interface{}); ok {
				cleanedProps[k] = resolveRefsAndMerge(propSchema, rootSchema, depth+1)
			} else {
				cleanedProps[k] = v
			}
		}
		result["properties"] = cleanedProps
	}

	// Recursively process items
	if items, ok := result["items"].(map[string]interface{}); ok {
		result["items"] = resolveRefsAndMerge(items, rootSchema, depth+1)
	}

	return result
}

// resolveRef resolves a $ref path within the schema.
func resolveRef(ref string, rootSchema map[string]interface{}) map[string]interface{} {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}

	path := strings.Split(ref[2:], "/")
	current := rootSchema

	for _, segment := range path {
		if current == nil {
			return nil
		}
		if next, ok := current[segment].(map[string]interface{}); ok {
			current = next
		} else {
			return nil
		}
	}

	return current
}

// tryExtractEnum attempts to extract enum values from anyOf/oneOf with const values.
func tryExtractEnum(schemas []interface{}) []interface{} {
	var enumValues []interface{}

	for _, item := range schemas {
		itemSchema, ok := item.(map[string]interface{})
		if !ok {
			return nil
		}

		constVal, hasConst := itemSchema["const"]
		if !hasConst {
			return nil
		}

		// Skip null values
		if constVal == nil {
			continue
		}
		if str, ok := constVal.(string); ok && str == "" {
			continue
		}

		enumValues = append(enumValues, constVal)
	}

	if len(enumValues) == 0 {
		return nil
	}
	return enumValues
}

// uniqueStrings removes duplicates from a slice while preserving order.
func uniqueStrings(items []interface{}) []interface{} {
	seen := make(map[string]bool)
	result := make([]interface{}, 0, len(items))

	for _, item := range items {
		if str, ok := item.(string); ok {
			if !seen[str] {
				seen[str] = true
				result = append(result, str)
			}
		}
	}
	return result
}

// =============================================================================
// Thinking Block Validation and Auto-Fix
// =============================================================================

// CheckLastAssistantHasThinking checks if the last assistant message starts with a thinking block.
// Returns true if no assistant message exists or if it properly starts with thinking.
func CheckLastAssistantHasThinking(messages []ir.Message) bool {
	// Find last assistant message
	var lastAssistant *ir.Message
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ir.RoleAssistant {
			lastAssistant = &messages[i]
			break
		}
	}

	if lastAssistant == nil {
		return true // No assistant message, OK to enable thinking
	}

	if len(lastAssistant.Content) == 0 {
		return false // Has assistant message but no content
	}

	// Check if first content part is thinking/reasoning
	return lastAssistant.Content[0].Type == ir.ContentTypeReasoning
}

// EnsureThinkingConsistency ensures that if thinking is enabled, the last assistant message
// starts with a thinking block. If not, it inserts a placeholder thinking block.
// Returns true if modification was made.
func EnsureThinkingConsistency(messages []ir.Message) ([]ir.Message, bool) {
	if CheckLastAssistantHasThinking(messages) {
		return messages, false
	}

	// Find and fix last assistant message.
	// IMPORTANT: keep placeholder content semantically neutral.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ir.RoleAssistant {
			// Insert placeholder thinking block at the beginning.
			// We intentionally keep Reasoning empty and rely on the Gemini emitter
			// to treat SkipThoughtSignatureValidator as an allowed sentinel.
			placeholder := ir.ContentPart{
				Type:             ir.ContentTypeReasoning,
				Reasoning:        "", // Continuing from previous context...
				ThoughtSignature: SkipThoughtSignatureValidator,
			}
			messages[i].Content = append([]ir.ContentPart{placeholder}, messages[i].Content...)
			return messages, true
		}
	}

	return messages, false
}

// SkipThoughtSignatureValidator is a special signature value that bypasses validation.
const SkipThoughtSignatureValidator = "skip_thought_signature_validator"

// HasToolCallsInMessages checks if any message contains tool calls (for MCP scenario detection).
func HasToolCallsInMessages(messages []ir.Message) bool {
	for _, msg := range messages {
		if len(msg.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// =============================================================================
// Networking Tool Detection (Grounding/Web Search)
// =============================================================================

// networkingToolNames contains all known networking/web search tool names.
var networkingToolNames = map[string]bool{
	"web_search":              true,
	"google_search":           true,
	"web_search_20250305":     true,
	"google_search_retrieval": true,
	"googleSearch":            true,
	"googleSearchRetrieval":   true,
}

// IsNetworkingToolName checks if a tool name is a networking/web search tool.
func IsNetworkingToolName(name string) bool {
	return networkingToolNames[name]
}

// DetectsNetworkingTool checks if the tool list contains a networking/web search tool.
// This is used to determine if grounding should be enabled for the request.
func DetectsNetworkingTool(tools []ir.ToolDefinition) bool {
	for _, tool := range tools {
		if networkingToolNames[tool.Name] {
			return true
		}
	}
	return false
}

// DetectsNetworkingToolFromRaw checks for networking tools in raw JSON tool definitions.
// This handles cases where tools haven't been parsed into ir.ToolDefinition yet.
func DetectsNetworkingToolFromRaw(toolsJSON []byte) bool {
	if len(toolsJSON) == 0 {
		return false
	}

	parsed := gjson.ParseBytes(toolsJSON)
	if !parsed.IsArray() {
		return false
	}

	for _, tool := range parsed.Array() {
		// Check direct name field
		if name := tool.Get("name").String(); networkingToolNames[name] {
			return true
		}

		// Check type field (for built-in tools like "web_search_20250305")
		if toolType := tool.Get("type").String(); networkingToolNames[toolType] {
			return true
		}

		// Check OpenAI nested format: {"type": "function", "function": {"name": "..."}}
		if funcName := tool.Get("function.name").String(); networkingToolNames[funcName] {
			return true
		}

		// Check Gemini functionDeclarations format
		if decls := tool.Get("functionDeclarations"); decls.IsArray() {
			for _, decl := range decls.Array() {
				if name := decl.Get("name").String(); networkingToolNames[name] {
					return true
				}
			}
		}

		// Check Gemini googleSearch/googleSearchRetrieval
		if tool.Get("googleSearch").Exists() || tool.Get("googleSearchRetrieval").Exists() {
			return true
		}
	}

	return false
}

// =============================================================================
// Tool Loop Recovery for Thinking Models
// =============================================================================

// CloseToolLoopForThinking closes a broken tool loop by injecting synthetic messages.
// When a client strips thinking blocks from assistant messages with tool calls,
// the API will reject subsequent tool results because "Assistant message must start with thinking".
// This function detects such scenarios and injects synthetic messages to close the loop,
// allowing the model to start fresh with a new thinking block.
// Note: Returns a new slice to avoid modifying the original.
func CloseToolLoopForThinking(messages []ir.Message) ([]ir.Message, bool) {
	if len(messages) == 0 {
		return messages, false
	}

	// Check if we're in a tool loop (last message is tool result)
	lastMsg := messages[len(messages)-1]
	inToolLoop := false
	if lastMsg.Role == ir.RoleUser || lastMsg.Role == ir.RoleTool {
		for _, part := range lastMsg.Content {
			if part.Type == ir.ContentTypeToolResult {
				inToolLoop = true
				break
			}
		}
	}

	if !inToolLoop {
		return messages, false
	}

	// Find last assistant message
	lastAssistantIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ir.RoleAssistant {
			lastAssistantIdx = i
			break
		}
	}

	if lastAssistantIdx == -1 {
		return messages, false
	}

	// Check if last assistant message has thinking block
	lastAssistant := messages[lastAssistantIdx]
	hasThinking := false
	if len(lastAssistant.Content) > 0 {
		hasThinking = lastAssistant.Content[0].Type == ir.ContentTypeReasoning
	}

	// If we're in a tool loop but assistant has no thinking, close the loop
	if !hasThinking {
		// Create a copy to avoid modifying the original slice
		result := make([]ir.Message, len(messages), len(messages)+2)
		copy(result, messages)

		// Inject synthetic messages to close the loop
		syntheticAssistant := ir.Message{
			Role: ir.RoleAssistant,
			Content: []ir.ContentPart{
				{Type: ir.ContentTypeText, Text: "[System: Tool loop recovered. Previous tool execution accepted.]"},
			},
		}
		syntheticUser := ir.Message{
			Role: ir.RoleUser,
			Content: []ir.ContentPart{
				{Type: ir.ContentTypeText, Text: "Please continue with the next step."},
			},
		}

		result = append(result, syntheticAssistant, syntheticUser)
		return result, true
	}

	return messages, false
}

// =============================================================================
// Thinking Block Validation and Filtering
// =============================================================================

// FilterInvalidThinkingBlocks converts thinking blocks with invalid signatures to text blocks.
// This preserves content that would otherwise be lost when thinking blocks are skipped.
// Used by Antigravity to handle responses where signature validation is required.
func FilterInvalidThinkingBlocks(messages []ir.Message, model string) []ir.Message {
	result := make([]ir.Message, 0, len(messages))

	for _, msg := range messages {
		// Only process assistant messages
		if msg.Role != ir.RoleAssistant {
			result = append(result, msg)
			continue
		}

		newMsg := msg
		newMsg.Content = make([]ir.ContentPart, 0, len(msg.Content))

		for _, part := range msg.Content {
			if part.Type == ir.ContentTypeReasoning {
				// Check if signature is valid
				if cache.HasValidSignature(model, part.ThoughtSignature) {
					// Valid signature, keep as reasoning
					newMsg.Content = append(newMsg.Content, part)
				} else {
					// Invalid signature, convert to text if content exists
					if part.Reasoning != "" {
						newMsg.Content = append(newMsg.Content, ir.ContentPart{
							Type: ir.ContentTypeText,
							Text: part.Reasoning,
						})
					}
					// Empty thinking blocks with invalid signatures are dropped
				}
			} else {
				// Non-reasoning parts are kept as-is
				newMsg.Content = append(newMsg.Content, part)
			}
		}

		// Preserve tool calls
		newMsg.ToolCalls = msg.ToolCalls

		// If no content remains, add empty text to keep message valid
		if len(newMsg.Content) == 0 && len(newMsg.ToolCalls) == 0 {
			newMsg.Content = []ir.ContentPart{{Type: ir.ContentTypeText, Text: ""}}
		}

		result = append(result, newMsg)
	}

	return result
}

// RemoveTrailingUnsignedThinking removes trailing thinking blocks without valid signatures from messages.
// This prevents invalid thinking blocks at the end of messages from causing issues.
// Used by Antigravity to clean up responses before returning to clients.
func RemoveTrailingUnsignedThinking(messages []ir.Message, model string) []ir.Message {
	result := make([]ir.Message, 0, len(messages))

	for _, msg := range messages {
		// Only process assistant messages
		if msg.Role != ir.RoleAssistant {
			result = append(result, msg)
			continue
		}

		// Find the last index of non-thinking content or valid thinking
		endIndex := len(msg.Content)
		for i := len(msg.Content) - 1; i >= 0; i-- {
			part := msg.Content[i]
			if part.Type == ir.ContentTypeReasoning {
				// Check if signature is valid
				if cache.HasValidSignature(model, part.ThoughtSignature) {
					// Valid signature, stop here
					break
				} else {
					// Invalid signature, mark for removal
					endIndex = i
				}
			} else {
				// Non-thinking part, stop here
				break
			}
		}

		// Create new message with trimmed content
		newMsg := msg
		if endIndex < len(msg.Content) {
			newMsg.Content = make([]ir.ContentPart, endIndex)
			copy(newMsg.Content, msg.Content[:endIndex])
		}

		// Preserve tool calls
		newMsg.ToolCalls = msg.ToolCalls

		result = append(result, newMsg)
	}

	return result
}
