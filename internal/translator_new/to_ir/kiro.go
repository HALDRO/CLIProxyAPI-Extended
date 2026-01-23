/**
 * @file Kiro (Amazon Q) response parser
 * @description Converts Kiro API responses (JSON and EventStream) into unified format.
 */

package to_ir

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	"github.com/tidwall/gjson"
)

var (
	embeddedToolCallPattern = regexp.MustCompile(`\[Called\s+(\w+)\s+with\s+args:\s*`)
	trailingCommaPattern    = regexp.MustCompile(`,\s*([}\]])`)
	unquotedKeyPattern      = regexp.MustCompile(`([{,]\s*)([a-zA-Z_][a-zA-Z0-9_]*)\s*:`)
)

// ParseKiroResponse converts a non-streaming Kiro API response to unified format.
func ParseKiroResponse(rawJSON []byte) ([]ir.Message, *ir.Usage, error) {
	if !gjson.ValidBytes(rawJSON) {
		return nil, nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}
	parsed := gjson.ParseBytes(rawJSON)

	// Try finding assistant response in various paths
	var resp gjson.Result
	if r := parsed.Get("conversationState.currentMessage.assistantResponseMessage"); r.Exists() {
		resp = r
	} else if r := parsed.Get("assistantResponseMessage"); r.Exists() {
		resp = r
	} else {
		return nil, nil, nil
	}

	msg := &ir.Message{Role: ir.RoleAssistant}

	// Parse content with thinking tag extraction
	if content := resp.Get("content").String(); content != "" {
		cleanContent, thinkingContent := extractThinkingFromContent(content)

		// Add thinking content first (if any)
		if thinkingContent != "" {
			msg.Content = append(msg.Content, ir.ContentPart{
				Type:      ir.ContentTypeReasoning,
				Reasoning: thinkingContent,
			})
		}

		// Add regular text content
		if cleanContent != "" {
			msg.Content = append(msg.Content, ir.ContentPart{
				Type: ir.ContentTypeText,
				Text: cleanContent,
			})
		}
	}

	for _, tool := range resp.Get("toolUsages").Array() {
		msg.ToolCalls = append(msg.ToolCalls, ir.ToolCall{
			ID:   convertToolID(tool.Get("toolUseId").String()),
			Name: tool.Get("name").String(),
			Args: tool.Get("input").String(),
		})
	}

	if len(msg.Content) == 0 && len(msg.ToolCalls) == 0 {
		return nil, nil, nil
	}
	return []ir.Message{*msg}, nil, nil
}

// extractThinkingFromContent parses content to extract thinking blocks and text.
// Returns (cleanContent, thinkingContent).
func extractThinkingFromContent(content string) (string, string) {
	if !strings.Contains(content, kiroThinkingStartTag) {
		return content, ""
	}

	var cleanContent strings.Builder
	var thinkingContent strings.Builder
	remaining := content

	for len(remaining) > 0 {
		startIdx := strings.Index(remaining, kiroThinkingStartTag)
		if startIdx < 0 {
			// No more thinking tags, add remaining as text
			cleanContent.WriteString(remaining)
			break
		}

		// Add text before thinking tag
		if startIdx > 0 {
			cleanContent.WriteString(remaining[:startIdx])
		}

		// Move past the opening tag
		remaining = remaining[startIdx+len(kiroThinkingStartTag):]

		// Find closing tag
		endIdx := strings.Index(remaining, kiroThinkingEndTag)
		if endIdx < 0 {
			// No closing tag found, treat rest as thinking content
			thinkingContent.WriteString(remaining)
			break
		}

		// Extract thinking content between tags
		thinkingContent.WriteString(remaining[:endIdx])
		remaining = remaining[endIdx+len(kiroThinkingEndTag):]
	}

	return strings.TrimSpace(cleanContent.String()), strings.TrimSpace(thinkingContent.String())
}

// KiroStreamState tracks state for Kiro streaming response parsing.
type KiroStreamState struct {
	Usage               *ir.Usage
	CurrentTool         *ir.ToolCall
	AccumulatedContent  string
	CurrentToolInput    string
	ToolCalls           []ir.ToolCall
	InThinkingBlock     bool   // Whether we're currently inside a <thinking> block
	AccumulatedThinking string // Accumulated thinking content
}

// Kiro thinking tag constants
const (
	kiroThinkingStartTag = "<thinking>"
	kiroThinkingEndTag   = "</thinking>"
)

func NewKiroStreamState() *KiroStreamState {
	return &KiroStreamState{
		ToolCalls:       make([]ir.ToolCall, 0),
		InThinkingBlock: false,
	}
}

// ProcessChunk processes a Kiro stream chunk and returns events.
func (s *KiroStreamState) ProcessChunk(rawJSON []byte) ([]ir.UnifiedEvent, error) {
	if len(rawJSON) == 0 {
		return nil, nil
	}
	if !gjson.ValidBytes(rawJSON) {
		return nil, nil
	}
	parsed := gjson.ParseBytes(rawJSON)

	s.parseUsage(parsed)

	// Handle reasoningContentEvent (official Kiro thinking mode)
	if reasoningEvents := s.processReasoningEvent(parsed); len(reasoningEvents) > 0 {
		return reasoningEvents, nil
	}

	if parsed.Get("toolUseId").Exists() && parsed.Get("name").Exists() {
		return s.processToolEvent(parsed), nil
	}

	return s.processRegularEvents(parsed), nil
}

func (s *KiroStreamState) parseUsage(parsed gjson.Result) {
	usageNode := parsed.Get("supplementaryWebLinksEvent")
	if !usageNode.Exists() {
		if parsed.Get("inputTokens").Exists() || parsed.Get("outputTokens").Exists() {
			usageNode = parsed
		}
	}

	if !usageNode.Exists() {
		return
	}

	inTokens := usageNode.Get("inputTokens").Int()
	outTokens := usageNode.Get("outputTokens").Int()

	if inTokens > 0 || outTokens > 0 {
		s.Usage = &ir.Usage{
			PromptTokens:     int(inTokens),
			CompletionTokens: int(outTokens),
			TotalTokens:      int(inTokens + outTokens),
		}
	}
}

func (s *KiroStreamState) processToolEvent(parsed gjson.Result) []ir.UnifiedEvent {
	id := convertToolID(parsed.Get("toolUseId").String())
	name := parsed.Get("name").String()

	var events []ir.UnifiedEvent
	isNewTool := s.CurrentTool == nil || s.CurrentTool.ID != id
	toolIndex := len(s.ToolCalls)

	if isNewTool {
		s.CurrentTool = &ir.ToolCall{ID: id, Name: name}
		s.CurrentToolInput = ""
	}

	inputNode := parsed.Get("input")
	var inputDelta string
	if inputNode.IsObject() {
		inputDelta = inputNode.Raw
	} else {
		inputDelta = inputNode.String()
	}
	s.CurrentToolInput += inputDelta

	if isNewTool {
		// First event for this tool - emit full ToolCall with ID and Name
		events = append(events, ir.UnifiedEvent{
			Type:          ir.EventTypeToolCall,
			ToolCall:      &ir.ToolCall{ID: id, Name: name, Args: inputDelta},
			ToolCallIndex: toolIndex,
		})
	} else if inputDelta != "" {
		// Subsequent events - emit delta only (no ID/Name needed)
		events = append(events, ir.UnifiedEvent{
			Type:          ir.EventTypeToolCallDelta,
			ToolCall:      &ir.ToolCall{Args: inputDelta},
			ToolCallIndex: toolIndex,
		})
	}

	if parsed.Get("stop").Bool() {
		s.CurrentTool.Args = s.CurrentToolInput
		if s.CurrentTool.Args == "" {
			s.CurrentTool.Args = "{}"
		}
		s.ToolCalls = append(s.ToolCalls, *s.CurrentTool)
		// Emit completion event to close the content_block
		events = append(events, ir.UnifiedEvent{
			Type:          ir.EventTypeToolCallDelta,
			ToolCall:      &ir.ToolCall{IsComplete: true},
			ToolCallIndex: toolIndex,
		})
		s.CurrentTool = nil
		s.CurrentToolInput = ""
	}

	return events
}

func (s *KiroStreamState) processRegularEvents(parsed gjson.Result) []ir.UnifiedEvent {
	var events []ir.UnifiedEvent
	data := parsed
	if r := parsed.Get("assistantResponseEvent"); r.Exists() {
		data = r
	} else if r := parsed.Get("completionEvent"); r.Exists() {
		data = r
	} else if r := parsed.Get("chatResponseEvent"); r.Exists() {
		data = r
	} else if r := parsed.Get("message"); r.Exists() {
		data = r
	}

	if content := data.Get("content").String(); content != "" {
		// Process content with thinking tag parsing
		textEvents, thinkingEvents := s.processContentWithThinking(content)
		events = append(events, thinkingEvents...)
		events = append(events, textEvents...)
	}

	for _, tool := range data.Get("toolUsages").Array() {
		tc := ir.ToolCall{
			ID:   convertToolID(tool.Get("toolUseId").String()),
			Name: tool.Get("name").String(),
			Args: tool.Get("input").String(),
		}
		if !s.hasToolCall(tc.ID) {
			s.ToolCalls = append(s.ToolCalls, tc)
			events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToolCall, ToolCall: &tc})
		}
	}
	return events
}

// processReasoningEvent handles official reasoningContentEvent from Kiro API.
// When thinking_mode is enabled, Kiro returns reasoning as dedicated events
// rather than inline <thinking> tags.
func (s *KiroStreamState) processReasoningEvent(parsed gjson.Result) []ir.UnifiedEvent {
	var events []ir.UnifiedEvent

	// Check for reasoningContentEvent (official Kiro thinking mode)
	if reasoning := parsed.Get("reasoningContentEvent"); reasoning.Exists() {
		content := reasoning.Get("content").String()
		if content != "" {
			s.AccumulatedThinking += content
			events = append(events, ir.UnifiedEvent{
				Type:      ir.EventTypeReasoning,
				Reasoning: content,
			})
		}
		return events
	}

	// Also check direct reasoningContent field
	if reasoning := parsed.Get("reasoningContent"); reasoning.Exists() {
		content := reasoning.String()
		if content != "" {
			s.AccumulatedThinking += content
			events = append(events, ir.UnifiedEvent{
				Type:      ir.EventTypeReasoning,
				Reasoning: content,
			})
		}
		return events
	}

	return nil
}

// processContentWithThinking parses content for <thinking> tags and separates
// thinking content from regular text content.
// Returns (textEvents, thinkingEvents).
func (s *KiroStreamState) processContentWithThinking(content string) ([]ir.UnifiedEvent, []ir.UnifiedEvent) {
	var textEvents, thinkingEvents []ir.UnifiedEvent

	remaining := content

	for len(remaining) > 0 {
		if s.InThinkingBlock {
			// We're inside a thinking block, look for </thinking>
			endIdx := strings.Index(remaining, kiroThinkingEndTag)
			if endIdx >= 0 {
				// Found end tag - emit thinking content before the tag
				thinkingText := remaining[:endIdx]
				if thinkingText != "" {
					s.AccumulatedThinking += thinkingText
					thinkingEvents = append(thinkingEvents, ir.UnifiedEvent{
						Type:      ir.EventTypeReasoning,
						Reasoning: thinkingText,
					})
				}
				s.InThinkingBlock = false
				remaining = remaining[endIdx+len(kiroThinkingEndTag):]
			} else {
				// No end tag found - all remaining content is thinking
				if remaining != "" {
					s.AccumulatedThinking += remaining
					thinkingEvents = append(thinkingEvents, ir.UnifiedEvent{
						Type:      ir.EventTypeReasoning,
						Reasoning: remaining,
					})
				}
				break
			}
		} else {
			// We're outside a thinking block, look for <thinking>
			startIdx := strings.Index(remaining, kiroThinkingStartTag)
			if startIdx >= 0 {
				// Found start tag - emit text content before the tag
				textBefore := remaining[:startIdx]
				if textBefore != "" {
					cleanContent, embeddedTools := ParseEmbeddedToolCalls(textBefore)
					if cleanContent != "" {
						s.AccumulatedContent += cleanContent
						textEvents = append(textEvents, ir.UnifiedEvent{
							Type:    ir.EventTypeToken,
							Content: cleanContent,
						})
					}
					for _, tc := range embeddedTools {
						if !s.hasToolCall(tc.ID) {
							s.ToolCalls = append(s.ToolCalls, tc)
							tcCopy := tc
							textEvents = append(textEvents, ir.UnifiedEvent{
								Type:     ir.EventTypeToolCall,
								ToolCall: &tcCopy,
							})
						}
					}
				}
				s.InThinkingBlock = true
				remaining = remaining[startIdx+len(kiroThinkingStartTag):]
			} else {
				// No start tag found - all remaining content is regular text
				if remaining != "" {
					cleanContent, embeddedTools := ParseEmbeddedToolCalls(remaining)
					if cleanContent != "" {
						s.AccumulatedContent += cleanContent
						textEvents = append(textEvents, ir.UnifiedEvent{
							Type:    ir.EventTypeToken,
							Content: cleanContent,
						})
					}
					for _, tc := range embeddedTools {
						if !s.hasToolCall(tc.ID) {
							s.ToolCalls = append(s.ToolCalls, tc)
							tcCopy := tc
							textEvents = append(textEvents, ir.UnifiedEvent{
								Type:     ir.EventTypeToolCall,
								ToolCall: &tcCopy,
							})
						}
					}
				}
				break
			}
		}
	}

	return textEvents, thinkingEvents
}

func (s *KiroStreamState) hasToolCall(id string) bool {
	for _, tc := range s.ToolCalls {
		if tc.ID == id {
			return true
		}
	}
	return false
}

func (s *KiroStreamState) DetermineFinishReason() ir.FinishReason {
	if len(s.ToolCalls) > 0 {
		return ir.FinishReasonToolCalls
	}
	return ir.FinishReasonStop
}

func convertToolID(id string) string {
	if strings.HasPrefix(id, "tooluse_") {
		return strings.Replace(id, "tooluse_", "call_", 1)
	}
	return id
}

// ParseEmbeddedToolCalls extracts [Called tool_name with args: {...}] format from text.
func ParseEmbeddedToolCalls(text string) (string, []ir.ToolCall) {
	if !strings.Contains(text, "[Called") {
		return text, nil
	}

	var toolCalls []ir.ToolCall
	cleanText := text
	processedIDs := make(map[string]bool)

	matches := embeddedToolCallPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	// Process matches in reverse order
	for i := len(matches) - 1; i >= 0; i-- {
		matchStart := matches[i][0]
		toolNameStart := matches[i][2]
		toolNameEnd := matches[i][3]

		if toolNameStart < 0 || toolNameEnd < 0 {
			continue
		}

		toolName := text[toolNameStart:toolNameEnd]
		jsonStart := matches[i][1]

		if jsonStart >= len(text) {
			continue
		}

		// Skip whitespace
		for jsonStart < len(text) && (text[jsonStart] == ' ' || text[jsonStart] == '\t') {
			jsonStart++
		}

		if jsonStart >= len(text) || text[jsonStart] != '{' {
			continue
		}

		jsonEnd := findMatchingBracket(text, jsonStart)
		if jsonEnd < 0 {
			continue
		}

		jsonStr := text[jsonStart : jsonEnd+1]
		closingBracket := jsonEnd + 1
		for closingBracket < len(text) && text[closingBracket] != ']' {
			closingBracket++
		}
		if closingBracket >= len(text) {
			continue
		}

		fullMatch := text[matchStart : closingBracket+1]
		repairedJSON := repairJSON(jsonStr)
		var argsMap map[string]interface{}
		if err := json.Unmarshal([]byte(repairedJSON), &argsMap); err != nil {
			continue
		}

		toolUseID := "call_" + uuid.New().String()[:12]
		dedupeKey := toolName + ":" + repairedJSON
		if processedIDs[dedupeKey] {
			cleanText = strings.Replace(cleanText, fullMatch, "", 1)
			continue
		}
		processedIDs[dedupeKey] = true

		toolCalls = append(toolCalls, ir.ToolCall{
			ID:   toolUseID,
			Name: toolName,
			Args: repairedJSON,
		})

		cleanText = strings.Replace(cleanText, fullMatch, "", 1)
	}

	return strings.TrimSpace(cleanText), toolCalls
}

func findMatchingBracket(text string, startPos int) int {
	if startPos >= len(text) {
		return -1
	}

	openChar := text[startPos]
	var closeChar byte
	switch openChar {
	case '{':
		closeChar = '}'
	case '[':
		closeChar = ']'
	default:
		return -1
	}

	depth := 1
	inString := false
	escapeNext := false

	for i := startPos + 1; i < len(text); i++ {
		char := text[i]

		if escapeNext {
			escapeNext = false
			continue
		}
		if char == '\\' && inString {
			escapeNext = true
			continue
		}
		if char == '"' {
			inString = !inString
			continue
		}

		if !inString {
			if char == openChar {
				depth++
			} else if char == closeChar {
				depth--
				if depth == 0 {
					return i
				}
			}
		}
	}
	return -1
}

func repairJSON(raw string) string {
	repaired := trailingCommaPattern.ReplaceAllString(raw, "$1")
	repaired = unquotedKeyPattern.ReplaceAllString(repaired, `$1"$2":`)
	return repaired
}
