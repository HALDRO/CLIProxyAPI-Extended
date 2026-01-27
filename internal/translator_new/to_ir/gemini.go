// Package to_ir converts provider-specific API formats into unified format.
// This file handles Gemini AI Studio API responses (streaming and non-streaming).
package to_ir

import (
	"encoding/json"
	"time"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
)

// ParseGeminiResponse parses a non-streaming Gemini API response into unified format.
func ParseGeminiResponse(rawJSON []byte) (*ir.UnifiedChatRequest, []ir.Message, *ir.Usage, error) {
	messages, usage, _, err := ParseGeminiResponseMeta(rawJSON)
	return nil, messages, usage, err
}

// ParseGeminiResponseMeta parses a non-streaming Gemini API response into unified format with metadata.
// Returns messages, usage, and response metadata (responseId, createTime, nativeFinishReason).
func ParseGeminiResponseMeta(rawJSON []byte) ([]ir.Message, *ir.Usage, *ir.ResponseMeta, error) {
	if !gjson.ValidBytes(rawJSON) {
		return nil, nil, nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}

	parsed := gjson.ParseBytes(rawJSON)
	meta := parseGeminiMeta(parsed)
	usage := parseGeminiUsage(parsed)

	// Parse candidates
	candidates := parsed.Get("candidates").Array()
	if len(candidates) == 0 {
		return nil, usage, meta, nil
	}

	parts := candidates[0].Get("content.parts").Array()
	if len(parts) == 0 {
		return nil, usage, meta, nil
	}

	msg := ir.Message{Role: ir.RoleAssistant}
	for _, part := range parts {
		// Extract thought signature if present
		ts := part.Get("thoughtSignature").String()
		if ts == "" {
			ts = part.Get("thought_signature").String()
		}

		if text := part.Get("text"); text.Exists() && text.String() != "" {
			if part.Get("thought").Bool() {
				msg.Content = append(msg.Content, ir.ContentPart{Type: ir.ContentTypeReasoning, Reasoning: text.String(), ThoughtSignature: ts})
			} else {
				msg.Content = append(msg.Content, ir.ContentPart{Type: ir.ContentTypeText, Text: text.String(), ThoughtSignature: ts})
			}
		} else if fc := part.Get("functionCall"); fc.Exists() {
			if name := fc.Get("name").String(); name != "" {
				args := fc.Get("args").Raw
				if args == "" {
					args = "{}"
				}
				args = ir.ValidateAndNormalizeJSON(args)
				msg.ToolCalls = append(msg.ToolCalls, ir.ToolCall{ID: ir.GenToolCallIDWithName(name), Name: name, Args: args, ThoughtSignature: ts})
			}
		} else if img := parseGeminiInlineImage(part); img != nil {
			msg.Content = append(msg.Content, ir.ContentPart{Type: ir.ContentTypeImage, Image: img, ThoughtSignature: ts})
		} else if ts != "" {
			// Part with only thought signature (and maybe empty text)
			// Preserve it as a reasoning part with empty text
			msg.Content = append(msg.Content, ir.ContentPart{Type: ir.ContentTypeReasoning, Reasoning: "", ThoughtSignature: ts})
		}
	}

	if len(msg.Content) == 0 && len(msg.ToolCalls) == 0 {
		return nil, usage, meta, nil
	}

	// Filter invalid thinking blocks and remove trailing unsigned thinking
	// Note: model parameter not available here, pass empty string
	messages := []ir.Message{msg}
	messages = ir.FilterInvalidThinkingBlocks(messages, "")
	messages = ir.RemoveTrailingUnsignedThinking(messages, "")

	return messages, usage, meta, nil
}

// ParseGeminiChunk parses a streaming Gemini API chunk into events.
func ParseGeminiChunk(rawJSON []byte) ([]ir.UnifiedEvent, error) {
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

	var events []ir.UnifiedEvent
	var finishReason ir.FinishReason
	var usage *ir.Usage

	// Parse usage metadata if present
	if u := parseGeminiUsage(parsed); u != nil {
		usage = u
	}

	// Parse candidates content
	if candidates := parsed.Get("candidates").Array(); len(candidates) > 0 {
		candidate := candidates[0]

		// Parse parts
		for _, part := range candidate.Get("content.parts").Array() {
			// Extract thought signature if present
			ts := part.Get("thoughtSignature").String()
			if ts == "" {
				ts = part.Get("thought_signature").String()
			}

			if text := part.Get("text"); text.Exists() && text.String() != "" {
				if part.Get("thought").Bool() {
					events = append(events, ir.UnifiedEvent{Type: ir.EventTypeReasoning, Reasoning: text.String(), ThoughtSignature: ts})
				} else {
					events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToken, Content: text.String(), ThoughtSignature: ts})
				}
			} else if fc := part.Get("functionCall"); fc.Exists() {
				if name := fc.Get("name").String(); name != "" {
					// NOTE: We no longer emit a separate reasoning event for thoughtSignature here.
					// With include_thoughts=true, Gemini sends readable thoughts in separate parts
					// with "thought": true. The signature is preserved in ToolCall.ThoughtSignature
					// for history/context purposes.

					id := fc.Get("id").String()
					if id == "" {
						id = ir.GenToolCallIDWithName(name)
					}
					args := fc.Get("args").Raw
					if args == "" {
						args = "{}"
					}
					args = ir.ValidateAndNormalizeJSON(args)

					var partialArgs string
					if pa := fc.Get("partialArgs"); pa.Exists() {
						partialArgs = pa.Raw
						// NOTE: Do NOT normalize partialArgs - they are incomplete JSON fragments
						// that cannot be safely parsed or modified. Only normalize complete args.
					}

					events = append(events, ir.UnifiedEvent{
						Type:             ir.EventTypeToolCall,
						ToolCall:         &ir.ToolCall{ID: id, Name: name, Args: args, PartialArgs: partialArgs, ThoughtSignature: ts},
						ThoughtSignature: ts,
					})
				}
			} else if img := parseGeminiInlineImage(part); img != nil {
				// Handle inline image in streaming response
				events = append(events, ir.UnifiedEvent{Type: ir.EventTypeImage, Image: img, ThoughtSignature: ts})
			} else if ts != "" {
				// Part with only thought signature
				events = append(events, ir.UnifiedEvent{Type: ir.EventTypeReasoning, Reasoning: "", ThoughtSignature: ts})
			}
		}

		// Check for finish reason
		if fr := candidate.Get("finishReason"); fr.Exists() {
			frStr := fr.String()
			finishReason = ir.MapGeminiFinishReason(frStr)

			// Handle MALFORMED_FUNCTION_CALL - Gemini sometimes returns malformed tool calls
			// Skip these for now as they require special parsing
			if frStr == "MALFORMED_FUNCTION_CALL" {
				// Skip malformed function calls - no event emitted
			}
		}
	}

	// Emit Finish event ONLY if we have an explicit finish reason from Gemini.
	// Do NOT use usage.TotalTokens as a fallback - Gemini sends usageMetadata
	// with totalTokenCount > 0 in EVERY chunk, not just the final one.
	if finishReason != "" {
		events = append(events, ir.UnifiedEvent{
			Type:         ir.EventTypeFinish,
			Usage:        usage,
			FinishReason: finishReason,
		})
	}

	return events, nil
}

// --- Helper Functions ---

func parseGeminiMeta(parsed gjson.Result) *ir.ResponseMeta {
	meta := &ir.ResponseMeta{}
	if rid := parsed.Get("responseId"); rid.Exists() {
		meta.ResponseID = rid.String()
	}
	if ct := parsed.Get("createTime"); ct.Exists() {
		if t, err := time.Parse(time.RFC3339Nano, ct.String()); err == nil {
			meta.CreateTime = t.Unix()
		}
	}
	if fr := parsed.Get("candidates.0.finishReason"); fr.Exists() {
		meta.NativeFinishReason = fr.String()
	}
	return meta
}

func parseGeminiUsage(parsed gjson.Result) *ir.Usage {
	u := parsed.Get("usageMetadata")
	if !u.Exists() {
		return nil
	}

	promptTokens := int(u.Get("promptTokenCount").Int())
	thoughtsTokens := int(u.Get("thoughtsTokenCount").Int())
	cachedTokens := int(u.Get("cachedContentTokenCount").Int())

	// Adjust prompt tokens to exclude cached tokens.
	promptTokens = promptTokens - cachedTokens
	if promptTokens < 0 {
		promptTokens = 0
	}

	return &ir.Usage{
		PromptTokens:       promptTokens + thoughtsTokens,
		CompletionTokens:   int(u.Get("candidatesTokenCount").Int()),
		TotalTokens:        int(u.Get("totalTokenCount").Int()),
		ThoughtsTokenCount: thoughtsTokens,
		CachedTokens:       cachedTokens,
	}
}

func parseGeminiInlineImage(part gjson.Result) *ir.ImagePart {
	inlineData := part.Get("inlineData")
	if !inlineData.Exists() {
		inlineData = part.Get("inline_data")
	}
	if !inlineData.Exists() {
		return nil
	}
	data := inlineData.Get("data").String()
	if data == "" {
		return nil
	}
	mimeType := inlineData.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inlineData.Get("mime_type").String()
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return &ir.ImagePart{MimeType: mimeType, Data: data}
}
