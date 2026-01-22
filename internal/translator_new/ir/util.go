/**
 * @file IR utility functions for translator pipeline
 * @description Provides core utilities for the Canonical IR translator architecture:
 *              - UUID/Tool Call ID generation
 *              - Text sanitization (UTF-8 cleanup)
 *              - JSON Schema cleaning (Gemini/Claude compatibility)
 *              - ThoughtSignature encoding in tool IDs (round-trip preservation)
 *              - Function name normalization (Gemini API compliance)
 *              - Reverse transform for tool arguments (string→native types)
 *              - Thinking block validation and auto-fix
 *              - Code execution formatting (executableCode/codeExecutionResult)
 *              - Anti-truncation support for long responses
 *              - Finish reason mapping between providers
 */

package ir

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tailscale/hujson"
)

// =============================================================================
// UUID Generation
// =============================================================================

// GenerateUUID generates a UUID v4 string.
func GenerateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // Variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
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

// SanitizeUTF8 is an alias for SanitizeText.
// Deprecated: Use SanitizeText instead.
func SanitizeUTF8(s string) string { return SanitizeText(s) }

func hasProblematicChars(s string) bool {
	for _, r := range s {
		if r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') {
			return true
		}
	}
	return false
}

// =============================================================================
// JSON Schema Cleaning
// =============================================================================

// CleanJsonSchema removes fields not supported by Gemini from JSON Schema.
func CleanJsonSchema(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}

	// Remove unsupported top-level keywords
	unsupportedKeywords := []string{
		"strict", "input_examples", "$schema", "$id", "$defs", "definitions",
		"additionalProperties", "patternProperties", "unevaluatedProperties",
		"minProperties", "maxProperties", "dependentRequired", "dependentSchemas",
		"if", "then", "else", "not", "contentEncoding", "contentMediaType",
		"deprecated", "readOnly", "writeOnly", "examples", "$comment",
		"$vocabulary", "$anchor", "$dynamicRef", "$dynamicAnchor",
		"propertyNames",
	}
	for _, kw := range unsupportedKeywords {
		delete(schema, kw)
	}

	cleanNestedSchemas(schema)
	return schema
}

func cleanNestedSchemas(schema map[string]interface{}) {
	// Clean properties
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if propSchema, ok := v.(map[string]interface{}); ok {
				CleanJsonSchema(propSchema)
			}
		}
	}

	// Clean items (for arrays)
	if items, ok := schema["items"].(map[string]interface{}); ok {
		CleanJsonSchema(items)
	}

	// Clean allOf, anyOf, oneOf
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for _, item := range arr {
				if itemSchema, ok := item.(map[string]interface{}); ok {
					CleanJsonSchema(itemSchema)
				}
			}
		}
	}

	// Flatten type arrays like ["string", "null"] to just "string"
	if typeVal, ok := schema["type"].([]interface{}); ok && len(typeVal) > 0 {
		for _, t := range typeVal {
			if tStr, ok := t.(string); ok && tStr != "null" {
				schema["type"] = tStr
				break
			}
		}
	}
}

// CleanJsonSchemaForClaude prepares JSON Schema for Claude API compatibility.
func CleanJsonSchemaForClaude(schema map[string]interface{}) map[string]interface{} {
	if schema == nil {
		return nil
	}
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
// Malformed Function Call Parsing (Gemini Workaround)
// =============================================================================

// ParseMalformedFunctionCall extracts function name and arguments from Gemini's MALFORMED_FUNCTION_CALL.
func ParseMalformedFunctionCall(finishMessage string) (string, string, bool) {
	// Find "call:" marker
	idx := strings.LastIndex(finishMessage, "call:")
	if idx == -1 {
		// Fallback: try finding at start or after ": "
		idx = strings.Index(finishMessage, ": call:")
		if idx != -1 {
			idx += 2
		} else if strings.HasPrefix(finishMessage, "call:") {
			idx = 0
		} else {
			return "", "", false
		}
	}

	// Extract content after "call:"
	rest := finishMessage[idx+5:]

	// Skip namespace (e.g., "default_api:")
	if colonIdx := strings.Index(rest, ":"); colonIdx != -1 {
		rest = rest[colonIdx+1:]
	} else {
		return "", "", false
	}

	// Find opening brace
	braceIdx := strings.Index(rest, "{")
	if braceIdx == -1 {
		return "", "", false
	}

	funcName := rest[:braceIdx]
	argsRaw := rest[braceIdx:]

	// Find matching closing brace
	depth := 0
	endIdx := -1
	for i, c := range argsRaw {
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				endIdx = i + 1
				break
			}
		}
	}

	if endIdx == -1 {
		return "", "", false
	}
	argsRaw = argsRaw[:endIdx]

	return funcName, convertMalformedArgsToJSON(argsRaw), true
}

func convertMalformedArgsToJSON(argsRaw string) string {
	if argsRaw == "{}" || argsRaw == "" {
		return "{}"
	}
	// Try hujson standardizer
	if standardized, err := hujson.Standardize([]byte(argsRaw)); err == nil {
		return string(standardized)
	}
	// Fallback to manual repair
	return convertMalformedArgsToJSONFallback(argsRaw)
}

func convertMalformedArgsToJSONFallback(argsRaw string) string {
	var result strings.Builder
	result.Grow(len(argsRaw) + 20)
	inString, escaped := false, false

	for i := 0; i < len(argsRaw); i++ {
		c := argsRaw[i]

		if escaped {
			result.WriteByte(c)
			escaped = false
			continue
		}
		if c == '\\' && inString {
			result.WriteByte(c)
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			result.WriteByte(c)
			continue
		}
		if inString {
			result.WriteByte(c)
			continue
		}

		// Handle keys
		if c == '{' || c == ',' {
			result.WriteByte(c)
			// Skip whitespace
			for i+1 < len(argsRaw) && (argsRaw[i+1] == ' ' || argsRaw[i+1] == '\t' || argsRaw[i+1] == '\n') {
				i++
			}
			// Check if next token is an unquoted key
			if i+1 < len(argsRaw) && argsRaw[i+1] != '"' && argsRaw[i+1] != '}' {
				keyStart := i + 1
				keyEnd := keyStart
				for keyEnd < len(argsRaw) && argsRaw[keyEnd] != ':' && argsRaw[keyEnd] != ' ' {
					keyEnd++
				}
				if keyEnd < len(argsRaw) && keyStart < keyEnd {
					result.WriteByte('"')
					result.WriteString(argsRaw[keyStart:keyEnd])
					result.WriteByte('"')
					i = keyEnd - 1
				}
			}
			continue
		}
		result.WriteByte(c)
	}
	return result.String()
}

// =============================================================================
// Mapping Helpers
// =============================================================================

// DefaultGeminiSafetySettings returns the default safety settings for Gemini API.
func DefaultGeminiSafetySettings() []map[string]string {
	return []map[string]string{
		{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "OFF"},
		{"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "BLOCK_NONE"},
	}
}

// MapGeminiFinishReason converts Gemini finishReason to FinishReason.
func MapGeminiFinishReason(geminiReason string) FinishReason {
	switch strings.ToUpper(geminiReason) {
	case "STOP", "FINISH_REASON_UNSPECIFIED", "UNKNOWN":
		return FinishReasonStop
	case "MAX_TOKENS":
		return FinishReasonLength
	case "SAFETY", "RECITATION":
		return FinishReasonContentFilter
	case "MALFORMED_FUNCTION_CALL":
		return FinishReasonToolCalls
	default:
		return FinishReasonUnknown
	}
}

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

// MapFinishReasonToGemini converts FinishReason to Gemini format.
func MapFinishReasonToGemini(reason FinishReason) string {
	switch reason {
	case FinishReasonStop, FinishReasonToolCalls:
		return "STOP"
	case FinishReasonLength:
		return "MAX_TOKENS"
	case FinishReasonContentFilter:
		return "SAFETY"
	default:
		return "OTHER"
	}
}

// =============================================================================
// Token Estimation and Budget Mapping
// =============================================================================

// EstimateTokenCount estimates token count from text (~4 chars/token).
func EstimateTokenCount(text string) int {
	if text == "" {
		return 0
	}
	// Conservative estimate: ~3 chars per token
	return (utf8.RuneCountInString(text) + 2) / 3
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

// =============================================================================
// Code Execution Parts (executableCode, codeExecutionResult)
// =============================================================================

// CodeExecutionPart represents executable code from Gemini response.
type CodeExecutionPart struct {
	Language string
	Code     string
}

// CodeExecutionResultPart represents code execution result from Gemini response.
type CodeExecutionResultPart struct {
	Outcome string // "OUTCOME_OK" or error
	Output  string
}

// FormatCodeExecutionAsMarkdown formats code execution parts as Markdown.
func FormatCodeExecutionAsMarkdown(code *CodeExecutionPart) string {
	if code == nil || code.Code == "" {
		return ""
	}
	lang := strings.ToLower(code.Language)
	if lang == "" {
		lang = "python"
	}
	return fmt.Sprintf("\n```%s\n%s\n```\n", lang, code.Code)
}

// FormatCodeExecutionResultAsMarkdown formats code execution result as Markdown.
func FormatCodeExecutionResultAsMarkdown(result *CodeExecutionResultPart) string {
	if result == nil || result.Output == "" {
		return ""
	}
	label := "output"
	if result.Outcome != "OUTCOME_OK" {
		label = "error"
	}
	return fmt.Sprintf("\n```%s\n%s\n```\n", label, result.Output)
}

// =============================================================================
// Reverse Transform for Tool Call Arguments (Gemini string→native types)
// =============================================================================

// ReverseTransformValue converts string values back to their native types.
// Gemini sometimes returns all values as strings; this restores proper types.
// Also handles JSON arrays/objects encoded as strings (e.g., "[\"file1.ts\",\"file2.ts\"]").
func ReverseTransformValue(value interface{}) interface{} {
	str, ok := value.(string)
	if !ok {
		return value
	}

	// Boolean values
	if str == "true" {
		return true
	}
	if str == "false" {
		return false
	}

	// Null
	if str == "null" {
		return nil
	}

	// Try to parse JSON array (Gemini sometimes returns arrays as strings)
	// e.g., "[\"file1.ts\",\"file2.ts\"]" -> ["file1.ts", "file2.ts"]
	// Safety: only parse if it looks like a JSON array with quoted strings (contains \")
	// This avoids accidentally parsing user-intended strings like "[test]" or "[1,2,3]"
	if len(str) >= 4 && str[0] == '[' && str[len(str)-1] == ']' && strings.Contains(str, `"`) {
		var arr []interface{}
		if err := json.Unmarshal([]byte(str), &arr); err == nil {
			return arr
		}
	}

	// Try to parse JSON object (Gemini sometimes returns objects as strings)
	// e.g., "{\"key\":\"value\"}" -> {"key": "value"}
	// Safety: only parse if it contains quotes (valid JSON objects have quoted keys)
	if len(str) >= 4 && str[0] == '{' && str[len(str)-1] == '}' && strings.Contains(str, `"`) {
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(str), &obj); err == nil {
			return obj
		}
	}

	// Try to parse as number (only if it looks like a number)
	if len(str) > 0 && (str[0] == '-' || str[0] == '+' || (str[0] >= '0' && str[0] <= '9')) {
		// Avoid converting strings that start with 0 (like "007") unless it's just "0"
		if len(str) > 1 && str[0] == '0' && str[1] != '.' {
			return str
		}

		// Try integer first
		isFloat := strings.Contains(str, ".")
		if !isFloat {
			if intVal, err := strconv.ParseInt(str, 10, 64); err == nil {
				// Check if it fits in int
				if intVal >= -2147483648 && intVal <= 2147483647 {
					return int(intVal)
				}
				return intVal
			}
		}

		// Try float
		if floatVal, err := strconv.ParseFloat(str, 64); err == nil {
			return floatVal
		}
	}

	return str
}

// ReverseTransformArgs recursively converts string values in tool call arguments to native types.
func ReverseTransformArgs(args interface{}) interface{} {
	switch v := args.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(v))
		for key, val := range v {
			if nested, ok := val.(map[string]interface{}); ok {
				result[key] = ReverseTransformArgs(nested)
			} else if arr, ok := val.([]interface{}); ok {
				result[key] = ReverseTransformArgs(arr)
			} else {
				result[key] = ReverseTransformValue(val)
			}
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, item := range v {
			if nested, ok := item.(map[string]interface{}); ok {
				result[i] = ReverseTransformArgs(nested)
			} else if arr, ok := item.([]interface{}); ok {
				result[i] = ReverseTransformArgs(arr)
			} else {
				result[i] = ReverseTransformValue(item)
			}
		}
		return result
	default:
		return args
	}
}

// ReverseTransformArgsJSON parses JSON args, applies reverse transform to convert
// string values back to native types, and returns the transformed JSON string.
// Gemini sometimes returns all values as strings; this restores proper types.
func ReverseTransformArgsJSON(argsJSON string) string {
	if argsJSON == "" || argsJSON == "{}" {
		return argsJSON
	}

	var args interface{}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON
	}

	transformed := ReverseTransformArgs(args)
	result, err := json.Marshal(transformed)
	if err != nil {
		return argsJSON
	}
	return string(result)
}

// =============================================================================
// Deep Clean Undefined Values (Cherry Studio compatibility)
// =============================================================================

// DeepCleanUndefined recursively removes "[undefined]" string values from maps.
// Some clients like Cherry Studio inject "[undefined]" as placeholder values,
// which can cause Gemini API validation errors.
func DeepCleanUndefined(data map[string]interface{}) {
	if data == nil {
		return
	}
	for key, val := range data {
		switch v := val.(type) {
		case string:
			if v == "[undefined]" {
				delete(data, key)
			}
		case map[string]interface{}:
			DeepCleanUndefined(v)
		case []interface{}:
			deepCleanUndefinedArray(v)
		}
	}
}

// deepCleanUndefinedArray recursively cleans arrays of maps.
func deepCleanUndefinedArray(arr []interface{}) {
	for _, item := range arr {
		if m, ok := item.(map[string]interface{}); ok {
			DeepCleanUndefined(m)
		} else if nested, ok := item.([]interface{}); ok {
			deepCleanUndefinedArray(nested)
		}
	}
}
