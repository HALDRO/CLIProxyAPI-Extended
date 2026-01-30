package ir

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// SkipThoughtSignatureValidator is a special signature value that bypasses validation.
const SkipThoughtSignatureValidator = "skip_thought_signature_validator"

// minThoughtSignatureLength is the minimum length for a valid thought signature.
const minThoughtSignatureLength = 50

// =============================================================================
// JSON Schema Cleaning (Gemini specific)
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

// CleanJsonSchemaEnhanced applies extra compatibility cleanup on top of CleanJsonSchema.
// It matches the robust logic from src_NiceAG/proxy/common/json_schema.rs:
// 1. $ref flattening (recursive resolution)
// 2. allOf merging
// 3. anyOf/oneOf scoring and merging
// 4. const -> enum
// 5. Type lowercase & nullable handling
// 6. Constraint migration (validation fields to description)
// 7. Strict whitelist filtering
func CleanJsonSchemaEnhanced(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}

	// 0. Pre-processing: Collect all definitions
	defs := make(map[string]any)
	collectAllDefs(schema, defs)

	// Remove root defs
	delete(schema, "$defs")
	delete(schema, "definitions")

	// Flatten refs
	flattenRefs(schema, defs)

	// Recursive cleaning
	cleanSchemaEnhancedRecursive(schema)

	return schema
}

func collectAllDefs(value any, defs map[string]any) {
	switch v := value.(type) {
	case map[string]any:
		if d, ok := v["$defs"].(map[string]any); ok {
			for k, val := range d {
				if _, exists := defs[k]; !exists {
					defs[k] = val
				}
			}
		}
		if d, ok := v["definitions"].(map[string]any); ok {
			for k, val := range d {
				if _, exists := defs[k]; !exists {
					defs[k] = val
				}
			}
		}
		for k, val := range v {
			if k != "$defs" && k != "definitions" {
				collectAllDefs(val, defs)
			}
		}
	case []any:
		for _, item := range v {
			collectAllDefs(item, defs)
		}
	}
}

func flattenRefs(mapVal map[string]any, defs map[string]any) {
	// Check and replace $ref
	if refPath, ok := mapVal["$ref"].(string); ok {
		delete(mapVal, "$ref")

		// Parse ref name (e.g. #/$defs/MyType -> MyType)
		parts := strings.Split(refPath, "/")
		refName := parts[len(parts)-1]

		if defSchema, ok := defs[refName]; ok {
			if defMap, ok := defSchema.(map[string]any); ok {
				// Merge definition content
				for k, v := range defMap {
					if _, exists := mapVal[k]; !exists {
						// Deep copy needed? For now shallow copy of definition structure
						mapVal[k] = deepCopyValue(v)
					}
				}
				// Recursively process merged content
				flattenRefs(mapVal, defs)
			}
		} else {
			// Unresolved ref fallback
			mapVal["type"] = "string"
			hint := fmt.Sprintf("(Unresolved $ref: %s)", refPath)
			if desc, ok := mapVal["description"].(string); ok {
				if !strings.Contains(desc, hint) {
					if desc != "" {
						desc += " "
					}
					mapVal["description"] = desc + hint
				}
			} else {
				mapVal["description"] = hint
			}
		}
	}

	// Traverse children
	for _, v := range mapVal {
		if childMap, ok := v.(map[string]any); ok {
			flattenRefs(childMap, defs)
		} else if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if itemMap, ok := item.(map[string]any); ok {
					flattenRefs(itemMap, defs)
				}
			}
		}
	}
}

func cleanSchemaEnhancedRecursive(schema map[string]any) bool {
	isEffectivelyNullable := false

	// 0. Merge allOf
	mergeAllOf(schema)

	// 1. Recursive cleaning of children
	if props, ok := schema["properties"].(map[string]any); ok {
		nullableKeys := make(map[string]bool)
		for k, v := range props {
			if vMap, ok := v.(map[string]any); ok {
				if cleanSchemaEnhancedRecursive(vMap) {
					nullableKeys[k] = true
				}
			}
		}

		if len(nullableKeys) > 0 {
			if req, ok := schema["required"].([]any); ok {
				newReq := []any{}
				for _, r := range req {
					if s, ok := r.(string); ok {
						if !nullableKeys[s] {
							newReq = append(newReq, s)
						}
					}
				}
				if len(newReq) == 0 {
					delete(schema, "required")
				} else {
					schema["required"] = newReq
				}
			}
		}
	} else if items, ok := schema["items"].(map[string]any); ok {
		cleanSchemaEnhancedRecursive(items)
	} else {
		for _, v := range schema {
			if vMap, ok := v.(map[string]any); ok {
				cleanSchemaEnhancedRecursive(vMap)
			} else if vArr, ok := v.([]any); ok {
				for _, item := range vArr {
					if itemMap, ok := item.(map[string]any); ok {
						cleanSchemaEnhancedRecursive(itemMap)
					}
				}
			}
		}
	}

	// 1.5 Clean anyOf/oneOf branches before merging
	for _, key := range []string{"anyOf", "oneOf"} {
		if arr, ok := schema[key].([]any); ok {
			for _, branch := range arr {
				if branchMap, ok := branch.(map[string]any); ok {
					cleanSchemaEnhancedRecursive(branchMap)
				}
			}
		}
	}

	// 2. Handle anyOf/oneOf merging (extract best schema)
	var unionToMerge []any
	typeVal, _ := schema["type"].(string)
	if typeVal == "" || typeVal == "object" {
		if anyOf, ok := schema["anyOf"].([]any); ok {
			unionToMerge = anyOf
		} else if oneOf, ok := schema["oneOf"].([]any); ok {
			unionToMerge = oneOf
		}
	}

	if len(unionToMerge) > 0 {
		if bestBranch := extractBestSchemaFromUnion(unionToMerge); bestBranch != nil {
			if branchObj, ok := bestBranch.(map[string]any); ok {
				for k, v := range branchObj {
					if k == "properties" {
						targetProps, _ := schema["properties"].(map[string]any)
						if targetProps == nil {
							targetProps = make(map[string]any)
							schema["properties"] = targetProps
						}
						if sourceProps, ok := v.(map[string]any); ok {
							for pk, pv := range sourceProps {
								if _, exists := targetProps[pk]; !exists {
									targetProps[pk] = deepCopyValue(pv)
								}
							}
						}
					} else if k == "required" {
						targetReq, _ := schema["required"].([]any)
						if targetReq == nil {
							targetReq = []any{}
						}
						sourceReq, _ := v.([]any)
						// Union required fields
						seen := make(map[string]bool)
						for _, r := range targetReq {
							if s, ok := r.(string); ok {
								seen[s] = true
							}
						}
						for _, r := range sourceReq {
							if s, ok := r.(string); ok {
								if !seen[s] {
									targetReq = append(targetReq, s)
								}
							}
						}
						schema["required"] = targetReq
					} else if _, exists := schema[k]; !exists {
						schema[k] = deepCopyValue(v)
					}
				}
			}
		}
	}

	// 3. Safety check: looks like schema?
	looksLikeSchema := false
	for _, k := range []string{"type", "properties", "items", "enum", "anyOf", "oneOf", "allOf"} {
		if _, ok := schema[k]; ok {
			looksLikeSchema = true
			break
		}
	}

	if looksLikeSchema {
		// 4. Robust Constraint Migration
		hints := []string{}
		constraints := map[string]string{
			"minLength":        "minLen",
			"maxLength":        "maxLen",
			"pattern":          "pattern",
			"minimum":          "min",
			"maximum":          "max",
			"multipleOf":       "multipleOf",
			"exclusiveMinimum": "exclMin",
			"exclusiveMaximum": "exclMax",
			"minItems":         "minItems",
			"maxItems":         "maxItems",
			"propertyNames":    "propertyNames",
			"format":           "format",
		}
		for field, label := range constraints {
			if val, ok := schema[field]; ok && val != nil {
				valStr := fmt.Sprintf("%v", val)
				hints = append(hints, fmt.Sprintf("%s: %s", label, valStr))
			}
		}

		if len(hints) > 0 {
			suffix := fmt.Sprintf(" [Constraint: %s]", strings.Join(hints, ", "))
			desc, _ := schema["description"].(string)
			if !strings.Contains(desc, suffix) {
				schema["description"] = desc + suffix
			}
		}

		// 5. Whitelist filtering
		allowedFields := map[string]bool{
			"type":        true,
			"description": true,
			"properties":  true,
			"required":    true,
			"items":       true,
			"enum":        true,
			"title":       true,
		}
		for k := range schema {
			if !allowedFields[k] {
				delete(schema, k)
			}
		}

		// 6. Handle empty Object
		if t, ok := schema["type"].(string); ok && t == "object" {
			props, _ := schema["properties"].(map[string]any)
			if len(props) == 0 {
				schema["properties"] = map[string]any{
					"reason": map[string]any{"type": "string", "description": "Reason for calling this tool"},
				}
				schema["required"] = []any{"reason"}
			}
		}

		// 7. Align required fields
		if req, ok := schema["required"].([]any); ok {
			props, _ := schema["properties"].(map[string]any)
			newReq := []any{}
			for _, r := range req {
				if s, ok := r.(string); ok {
					if _, exists := props[s]; exists {
						newReq = append(newReq, s)
					}
				}
			}
			if len(newReq) == 0 {
				delete(schema, "required")
			} else {
				schema["required"] = newReq
			}
		}

		// 8. Handle type field
		if typeVal, ok := schema["type"]; ok {
			var selectedType string
			switch t := typeVal.(type) {
			case string:
				lower := strings.ToLower(t)
				if lower == "null" {
					isEffectivelyNullable = true
				} else {
					selectedType = lower
				}
			case []any:
				for _, item := range t {
					if s, ok := item.(string); ok {
						lower := strings.ToLower(s)
						if lower == "null" {
							isEffectivelyNullable = true
						} else if selectedType == "" {
							selectedType = lower
						}
					}
				}
			}
			if selectedType == "" {
				selectedType = "string"
			}
			schema["type"] = selectedType
		}

		if isEffectivelyNullable {
			desc, _ := schema["description"].(string)
			if !strings.Contains(desc, "nullable") {
				if desc != "" {
					desc += " "
				}
				schema["description"] = desc + "(nullable)"
			}
		}

		// 9. Enum values to string
		if enumVal, ok := schema["enum"].([]any); ok {
			newEnum := make([]any, len(enumVal))
			for i, v := range enumVal {
				if _, ok := v.(string); ok {
					newEnum[i] = v
				} else {
					if v == nil {
						newEnum[i] = "null"
					} else {
						newEnum[i] = fmt.Sprintf("%v", v)
					}
				}
			}
			schema["enum"] = newEnum
		}
	}

	return isEffectivelyNullable
}

func mergeAllOf(schema map[string]any) {
	allOf, ok := schema["allOf"].([]any)
	if !ok || len(allOf) == 0 {
		return
	}
	delete(schema, "allOf")

	mergedProperties := make(map[string]any)
	mergedRequired := make(map[string]bool)
	var otherFields []map[string]any

	for _, sub := range allOf {
		if subMap, ok := sub.(map[string]any); ok {
			if props, ok := subMap["properties"].(map[string]any); ok {
				for k, v := range props {
					mergedProperties[k] = deepCopyValue(v)
				}
			}
			if req, ok := subMap["required"].([]any); ok {
				for _, r := range req {
					if s, ok := r.(string); ok {
						mergedRequired[s] = true
					}
				}
			}
			// Collect other fields
			otherFields = append(otherFields, subMap)
		}
	}

	// Apply other fields (first wins logic simplified)
	for _, sub := range otherFields {
		for k, v := range sub {
			if k != "properties" && k != "required" && k != "allOf" {
				if _, exists := schema[k]; !exists {
					schema[k] = deepCopyValue(v)
				}
			}
		}
	}

	if len(mergedProperties) > 0 {
		existingProps, _ := schema["properties"].(map[string]any)
		if existingProps == nil {
			existingProps = make(map[string]any)
			schema["properties"] = existingProps
		}
		for k, v := range mergedProperties {
			if _, exists := existingProps[k]; !exists {
				existingProps[k] = v
			}
		}
	}

	if len(mergedRequired) > 0 {
		existingReq, _ := schema["required"].([]any)
		seen := make(map[string]bool)
		newReq := []any{}
		for _, r := range existingReq {
			if s, ok := r.(string); ok {
				seen[s] = true
				newReq = append(newReq, s)
			}
		}
		for req := range mergedRequired {
			if !seen[req] {
				newReq = append(newReq, req)
			}
		}
		schema["required"] = newReq
	}
}

func scoreSchemaOption(val any) int {
	if obj, ok := val.(map[string]any); ok {
		typeVal, _ := obj["type"].(string)
		if _, hasProps := obj["properties"]; hasProps || typeVal == "object" {
			return 3
		}
		if _, hasItems := obj["items"]; hasItems || typeVal == "array" {
			return 2
		}
		if typeVal != "" && typeVal != "null" {
			return 1
		}
	}
	return 0
}

func extractBestSchemaFromUnion(unionArray []any) any {
	var bestOption any
	bestScore := -1

	for _, item := range unionArray {
		score := scoreSchemaOption(item)
		if score > bestScore {
			bestScore = score
			bestOption = item
		}
	}
	return bestOption
}

func deepCopyValue(v any) any {
	return DeepCopy(v)
}

// =============================================================================
// Mapping Helpers (Gemini)
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
		// Recoverable error - should be skipped in stream, not treated as finish
		return FinishReasonUnknown
	case "UNEXPECTED_TOOL_CALL":
		// This is an intermediate state, not a final finish reason
		// Should be filtered out before calling this function
		return FinishReasonUnknown
	default:
		return FinishReasonUnknown
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
// Thinking Block Helpers (Gemini/Antigravity)
// =============================================================================

// EnsureThinkingConsistency ensures that if thinking is enabled, the last assistant message
// starts with a thinking block. If not, it inserts a placeholder thinking block.
func EnsureThinkingConsistency(messages []Message) ([]Message, bool) {
	if checkLastAssistantHasThinking(messages) {
		return messages, false
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant {
			placeholder := ContentPart{
				Type:             ContentTypeReasoning,
				Reasoning:        "",
				ThoughtSignature: SkipThoughtSignatureValidator,
			}
			messages[i].Content = append([]ContentPart{placeholder}, messages[i].Content...)
			return messages, true
		}
	}
	return messages, false
}

func checkLastAssistantHasThinking(messages []Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != RoleAssistant {
			continue
		}
		if len(messages[i].Content) == 0 {
			return false
		}
		return messages[i].Content[0].Type == ContentTypeReasoning
	}
	return true
}

// CloseToolLoopForThinking closes a broken tool loop by injecting synthetic messages.
// This prevents upstream errors when a client strips thinking blocks.
func CloseToolLoopForThinking(messages []Message) ([]Message, bool) {
	if len(messages) == 0 {
		return messages, false
	}

	// Find a trailing tool result without a preceding assistant tool call.
	// If found, inject a minimal assistant message that starts with thinking.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleTool {
			// Find nearest preceding assistant
			for j := i - 1; j >= 0; j-- {
				if messages[j].Role == RoleAssistant {
					return messages, false
				}
			}
			// No assistant before tool result: inject assistant placeholder at start.
			placeholder := Message{
				Role: RoleAssistant,
				Content: []ContentPart{{
					Type:             ContentTypeReasoning,
					Reasoning:        "",
					ThoughtSignature: SkipThoughtSignatureValidator,
				}},
			}
			out := make([]Message, 0, len(messages)+1)
			out = append(out, placeholder)
			out = append(out, messages...)
			return out, true
		}
	}
	return messages, false
}

func FilterInvalidThinkingBlocks(messages []Message, _ string) []Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != RoleAssistant {
			out = append(out, msg)
			continue
		}
		filtered := msg
		filtered.Content = nil
		for _, part := range msg.Content {
			if part.Type != ContentTypeReasoning {
				filtered.Content = append(filtered.Content, part)
				continue
			}
			sig := strings.TrimSpace(part.ThoughtSignature)
			if sig != "" && sig != SkipThoughtSignatureValidator && len(sig) < minThoughtSignatureLength {
				continue
			}
			filtered.Content = append(filtered.Content, part)
		}
		out = append(out, filtered)
	}
	return out
}

func RemoveTrailingUnsignedThinking(messages []Message, _ string) []Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role != RoleAssistant {
			out = append(out, msg)
			continue
		}
		trimmed := msg
		for len(trimmed.Content) > 0 {
			last := trimmed.Content[len(trimmed.Content)-1]
			if last.Type != ContentTypeReasoning {
				break
			}
			sig := strings.TrimSpace(last.ThoughtSignature)
			if sig != "" {
				break
			}
			trimmed.Content = trimmed.Content[:len(trimmed.Content)-1]
		}
		out = append(out, trimmed)
	}
	return out
}

// =============================================================================
// Tool Helpers (Networking / Compatibility)
// =============================================================================

var networkingToolNames = map[string]bool{
	"web_search":              true,
	"google_search":           true,
	"web_search_20250305":     true,
	"google_search_retrieval": true,
	"googleSearch":            true,
	"googleSearchRetrieval":   true,
}

func IsNetworkingToolName(name string) bool {
	return networkingToolNames[name]
}

func DetectsNetworkingTool(tools []ToolDefinition) bool {
	for _, tool := range tools {
		if networkingToolNames[tool.Name] {
			return true
		}
	}
	return false
}

// DetectsNetworkingToolFromRaw checks for networking tools in raw JSON tool definitions.
func DetectsNetworkingToolFromRaw(toolsJSON []byte) bool {
	if len(toolsJSON) == 0 || !gjson.ValidBytes(toolsJSON) {
		return false
	}
	parsed := gjson.ParseBytes(toolsJSON)
	if !parsed.IsArray() {
		return false
	}
	for _, tool := range parsed.Array() {
		if name := tool.Get("name").String(); networkingToolNames[name] {
			return true
		}
		if toolType := tool.Get("type").String(); networkingToolNames[toolType] {
			return true
		}
		if fn := tool.Get("function.name").String(); networkingToolNames[fn] {
			return true
		}
		if decls := tool.Get("functionDeclarations"); decls.IsArray() {
			for _, decl := range decls.Array() {
				if name := decl.Get("name").String(); networkingToolNames[name] {
					return true
				}
			}
		}
	}
	return false
}

// FixToolCallArgs modifies the args map in-place to match the schema.
// It converts string values to the correct type (number, boolean) based on the schema definition.
func FixToolCallArgs(args map[string]any, schema map[string]any) {
	if args == nil || schema == nil {
		return
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return
	}

	for k, v := range args {
		if propSchema, exists := props[k]; exists {
			if ps, ok := propSchema.(map[string]any); ok {
				// Use type switch to handle potential nil or incorrect types safely
				// but since we need to modify args[k], we'll update it inside fixSingleArg
				// Wait, we need to pass address or update map directly.
				// Since interface{} is a copy, we update args[k] with the result.
				updatedVal := fixSingleArg(v, ps)
				if updatedVal != nil {
					args[k] = updatedVal
				}
			}
		}
	}
}

func fixSingleArg(val any, schema map[string]any) any {
	// 1. Handle nested objects
	if props, ok := schema["properties"].(map[string]any); ok {
		if valMap, ok := val.(map[string]any); ok {
			for k, v := range valMap {
				if ps, exists := props[k]; exists {
					if psMap, ok := ps.(map[string]any); ok {
						updated := fixSingleArg(v, psMap)
						if updated != nil {
							valMap[k] = updated
						}
					}
				}
			}
			return valMap
		}
		return val
	}

	// 2. Handle arrays
	typeVal, _ := schema["type"].(string)
	if strings.ToLower(typeVal) == "array" {
		if items, ok := schema["items"].(map[string]any); ok {
			if valArr, ok := val.([]any); ok {
				for i, item := range valArr {
					updated := fixSingleArg(item, items)
					if updated != nil {
						valArr[i] = updated
					}
				}
				return valArr
			}
		}
		return val
	}

	// 3. Handle primitive type correction
	switch strings.ToLower(typeVal) {
	case "number", "integer":
		if s, ok := val.(string); ok {
			// Protection: Don't convert strings starting with '0' unless "0." or just "0"
			if len(s) > 1 && strings.HasPrefix(s, "0") && !strings.HasPrefix(s, "0.") {
				return val
			}
			if i, err := strconv.ParseInt(s, 10, 64); err == nil {
				return float64(i) // JSON numbers are float64 in Go map[string]interface{}
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		}
	case "boolean":
		if s, ok := val.(string); ok {
			switch strings.ToLower(s) {
			case "true", "1", "yes", "on":
				return true
			case "false", "0", "no", "off":
				return false
			}
		} else if n, ok := val.(float64); ok {
			switch n {
			case 1:
				return true
			case 0:
				return false
			}
		}
	case "string":
		if _, ok := val.(string); !ok && val != nil {
			return fmt.Sprintf("%v", val)
		}
	}

	return val
}

// RemoveNullsFromToolInput recursively removes nil values from tool input maps/arrays.
// This is often required for clients (like Roo/Kilo) that send explicit nulls which some providers (Gemini) reject.
func RemoveNullsFromToolInput(input any) any {
	switch v := input.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			if val == nil {
				continue
			}
			cleaned := RemoveNullsFromToolInput(val)
			if cleaned == nil {
				continue
			}
			out[k] = cleaned
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			if item == nil {
				continue
			}
			cleaned := RemoveNullsFromToolInput(item)
			if cleaned == nil {
				continue
			}
			out = append(out, cleaned)
		}
		return out
	default:
		return input
	}
}

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
