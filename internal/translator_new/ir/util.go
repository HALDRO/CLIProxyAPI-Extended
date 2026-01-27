/**
 * @file IR utility functions for translator pipeline
 * @description Provides core utilities for the Canonical IR translator architecture:
 *              - UUID/Tool Call ID generation
 *              - Text sanitization (UTF-8 cleanup)
 *              - Deep copying of maps and slices
 *              - JSON Schema cleaning (Claude compatibility)
 *              - ThoughtSignature encoding in tool IDs (round-trip preservation)
 *              - Provider mapping helpers (FinishReason, Role, Effort)
 */

package ir

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// =============================================================================
// UUID Generation
// =============================================================================

// GenerateUUID generates a UUID v4 string.
func GenerateUUID() string {
	return uuid.NewString()
}

// =============================================================================
// Tool Call ID Generation
// =============================================================================

// GenToolCallID generates a unique tool call ID with default prefix "call".
func GenToolCallID() string {
	return GenToolCallIDWithName("call")
}

// GenToolCallIDWithName generates a unique tool call ID with the given function name.
func GenToolCallIDWithName(name string) string {
	return fmt.Sprintf("%s-%s", name, GenerateUUID()[:8])
}

// GenClaudeToolCallID generates a Claude-compatible tool call ID with default prefix "toolu".
func GenClaudeToolCallID() string {
	return GenClaudeToolCallIDWithName("toolu")
}

// GenClaudeToolCallIDWithName generates a Claude-compatible tool call ID with function name.
func GenClaudeToolCallIDWithName(name string) string {
	return fmt.Sprintf("%s-%s", name, GenerateUUID()[:8])
}

// EncodeToolIDWithSignature packs thoughtSignature into a tool call ID.
// This is a best-effort round-trip helper: older clients may strip custom fields.
//
// Format: <id>|sig:<signature>
// If signature is empty, the original id is returned.
func EncodeToolIDWithSignature(id, signature string) string {
	id = strings.TrimSpace(id)
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return id
	}
	if id == "" {
		id = "tool"
	}
	return id + "|sig:" + signature
}

// DecodeToolIDAndSignature unpacks a tool call ID produced by EncodeToolIDWithSignature.
// If the id does not contain a signature marker, returns (id, "").
func DecodeToolIDAndSignature(encoded string) (id, signature string) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return "", ""
	}
	const marker = "|sig:"
	idx := strings.Index(encoded, marker)
	if idx < 0 {
		return encoded, ""
	}
	id = strings.TrimSpace(encoded[:idx])
	signature = strings.TrimSpace(encoded[idx+len(marker):])
	return id, signature
}

// =============================================================================
// Deep Copy Utilities
// =============================================================================

// CopyMap creates a deep copy of a map.
func CopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = DeepCopy(v)
	}
	return result
}

// CopySlice creates a deep copy of a slice.
func CopySlice(s []interface{}) []interface{} {
	if s == nil {
		return nil
	}
	result := make([]interface{}, len(s))
	for i, v := range s {
		result[i] = DeepCopy(v)
	}
	return result
}

// DeepCopy creates a deep copy of any value (map, slice, or primitive).
func DeepCopy(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return CopyMap(val)
	case []interface{}:
		return CopySlice(val)
	default:
		return val
	}
}

// =============================================================================
// Text Sanitization
// =============================================================================

// SanitizeText cleans text for safe use in API payloads.
// It removes invalid UTF-8 sequences and control characters (except tab, newline, carriage return).
func SanitizeText(s string) string {
	if s == "" || (utf8.ValidString(s) && !hasProblematicChars(s)) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			i++
			continue
		}
		if r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			i += size
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

func hasProblematicChars(s string) bool {
	for _, r := range s {
		if r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			return true
		}
	}
	return false
}

// =============================================================================
// JSON Schema Cleaning (Claude)
// =============================================================================

// CleanJsonSchemaForClaude prepares JSON Schema for Claude API compatibility.
func CleanJsonSchemaForClaude(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
	// Note: CleanJsonSchema is now in util_gemini.go but available in package ir.
	// It performs general cleaning desirable for Claude too (removing $defs etc).
	schema = CleanJsonSchema(schema)
	cleanSchemaForClaudeRecursive(schema)
	schema["additionalProperties"] = false
	schema["$schema"] = "http://json-schema.org/draft-07/schema#"
	return schema
}

func cleanSchemaForClaudeRecursive(schema map[string]interface{}) {
	if schema == nil {
		return
	}

	// Convert "const" to "enum"
	if constVal, ok := schema["const"]; ok {
		schema["enum"] = []interface{}{constVal}
		delete(schema, "const")
	}

	// Handle "anyOf" / "oneOf" by taking the first element
	for _, key := range []string{"anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok && len(arr) > 0 {
			if firstItem, ok := arr[0].(map[string]interface{}); ok {
				for k, v := range firstItem {
					schema[k] = v
				}
			}
			delete(schema, key)
		}
	}

	// Lowercase type fields
	if typeVal, ok := schema["type"].(string); ok {
		schema["type"] = strings.ToLower(typeVal)
	}

	// Remove unsupported fields
	unsupportedFields := []string{
		"allOf", "not",
		"any_of", "one_of", "all_of",
		"$ref", "$defs", "definitions", "$id", "$anchor", "$dynamicRef", "$dynamicAnchor",
		"$schema", "$vocabulary", "$comment",
		"if", "then", "else", "dependentSchemas", "dependentRequired",
		"unevaluatedItems", "unevaluatedProperties",
		"contentEncoding", "contentMediaType", "contentSchema",
		"dependencies",
		"minItems", "maxItems", "uniqueItems", "minContains", "maxContains",
		"minLength", "maxLength", "pattern", "format",
		"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
		"minProperties", "maxProperties",
		"default",
	}
	for _, field := range unsupportedFields {
		delete(schema, field)
	}

	// Recursively clean properties
	if properties, ok := schema["properties"].(map[string]interface{}); ok {
		for key, prop := range properties {
			if propMap, ok := prop.(map[string]interface{}); ok {
				cleanSchemaForClaudeRecursive(propMap)
				properties[key] = propMap
			}
		}
	}

	// Clean items
	if items := schema["items"]; items != nil {
		switch v := items.(type) {
		case map[string]interface{}:
			cleanSchemaForClaudeRecursive(v)
		case []interface{}:
			for i, item := range v {
				if itemMap, ok := item.(map[string]interface{}); ok {
					cleanSchemaForClaudeRecursive(itemMap)
					v[i] = itemMap
				}
			}
		}
	}

	// Handle prefixItems, additionalProperties, patternProperties, propertyNames, contains
	if prefixItems, ok := schema["prefixItems"].([]interface{}); ok {
		for i, item := range prefixItems {
			if itemMap, ok := item.(map[string]interface{}); ok {
				cleanSchemaForClaudeRecursive(itemMap)
				prefixItems[i] = itemMap
			}
		}
	}
	if addProps, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		cleanSchemaForClaudeRecursive(addProps)
	}
	if patternProps, ok := schema["patternProperties"].(map[string]interface{}); ok {
		for key, prop := range patternProps {
			if propMap, ok := prop.(map[string]interface{}); ok {
				cleanSchemaForClaudeRecursive(propMap)
				patternProps[key] = propMap
			}
		}
	}
	if propNames, ok := schema["propertyNames"].(map[string]interface{}); ok {
		cleanSchemaForClaudeRecursive(propNames)
	}
	if contains, ok := schema["contains"].(map[string]interface{}); ok {
		cleanSchemaForClaudeRecursive(contains)
	}
}

// =============================================================================
// Mapping Helpers (General / Claude / OpenAI)
// =============================================================================

// MapClaudeFinishReason converts Claude stop_reason to FinishReason.
func MapClaudeFinishReason(claudeReason string) FinishReason {
	switch claudeReason {
	case "end_turn", "stop_sequence":
		return FinishReasonStop
	case "max_tokens":
		return FinishReasonLength
	case "tool_use":
		return FinishReasonToolCalls
	default:
		return FinishReasonUnknown
	}
}

// MapOpenAIFinishReason converts OpenAI finish_reason to FinishReason.
func MapOpenAIFinishReason(openaiReason string) FinishReason {
	switch openaiReason {
	case "stop":
		return FinishReasonStop
	case "length":
		return FinishReasonLength
	case "tool_calls", "function_call":
		return FinishReasonToolCalls
	case "content_filter":
		return FinishReasonContentFilter
	default:
		return FinishReasonUnknown
	}
}

// MapFinishReasonToOpenAI converts FinishReason to OpenAI format.
func MapFinishReasonToOpenAI(reason FinishReason) string {
	switch reason {
	case FinishReasonLength:
		return "length"
	case FinishReasonToolCalls:
		return "tool_calls"
	case FinishReasonContentFilter:
		return "content_filter"
	default:
		return "stop"
	}
}

// MapStandardRole maps standard role strings to IR Role.
func MapStandardRole(role string) Role {
	switch role {
	case "system", "developer":
		return RoleSystem
	case "assistant":
		return RoleAssistant
	case "tool":
		return RoleTool
	default:
		return RoleUser
	}
}

// MapFinishReasonToClaude converts FinishReason to Claude format.
func MapFinishReasonToClaude(reason FinishReason) string {
	switch reason {
	case FinishReasonLength:
		return "max_tokens"
	case FinishReasonToolCalls:
		return "tool_use"
	default:
		return "end_turn"
	}
}

// MapEffortToBudget converts reasoning effort string to token budget.
// Returns (budget, includeThoughts).
func MapEffortToBudget(effort string) (int, bool) {
	switch effort {
	case "none":
		return 0, false
	case "auto":
		return -1, true
	case "minimal":
		return 512, true
	case "low":
		return 1024, true
	case "medium":
		return 8192, true
	case "high":
		return 24576, true
	case "xhigh":
		return 32768, true
	default:
		return -1, true
	}
}

// MapBudgetToEffort converts token budget to reasoning effort string.
func MapBudgetToEffort(budget int, defaultForZero string) string {
	if budget <= 0 {
		return defaultForZero
	}
	if budget <= 1024 {
		return "low"
	}
	if budget <= 8192 {
		return "medium"
	}
	return "high"
}
