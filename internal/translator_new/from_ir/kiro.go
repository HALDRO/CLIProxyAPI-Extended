/**
 * @file Kiro (Amazon Q) request converter
 * @description Converts unified format into Kiro API request format.
 */

package from_ir

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
)

// KiroProvider handles conversion from unified format to Kiro API format.
type KiroProvider struct{}

// ConvertRequest converts UnifiedChatRequest to Kiro API JSON format.
func (p *KiroProvider) ConvertRequest(req *ir.UnifiedChatRequest) ([]byte, error) {
	origin := extractOrigin(req)
	tools := extractTools(req.Tools)
	systemPrompt := extractSystemPrompt(req.Messages)

	// Inject thinking mode configuration if enabled
	// Kiro API supports official thinking/reasoning mode via <thinking_mode> tag.
	// When set to "enabled", Kiro returns reasoning content as official reasoningContentEvent
	// rather than inline <thinking> tags in assistantResponseEvent.
	systemPrompt = injectThinkingMode(req.Thinking, systemPrompt)

	history, currentMessage := processMessages(req.Messages, tools, req.Model, origin)

	injectSystemPrompt(systemPrompt, &history, currentMessage, req.Model, origin)

	request := map[string]interface{}{
		"conversationState": map[string]interface{}{
			"chatTriggerType": "MANUAL",
			"conversationId":  ir.GenerateUUID(),
			"currentMessage":  currentMessage,
			"history":         history,
		},
	}

	if req.Metadata != nil {
		if arn, ok := req.Metadata["profileArn"].(string); ok && arn != "" {
			request["profileArn"] = arn
		}
	}

	result, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return []byte(ir.SanitizeText(string(result))), nil
}

func extractOrigin(req *ir.UnifiedChatRequest) string {
	if req.Metadata != nil {
		if o, ok := req.Metadata["origin"].(string); ok && o != "" {
			return o
		}
	}
	return "AI_EDITOR"
}

func extractTools(irTools []ir.ToolDefinition) []interface{} {
	if len(irTools) == 0 {
		return nil
	}
	tools := make([]interface{}, len(irTools))
	for i, t := range irTools {
		tools[i] = map[string]interface{}{
			"toolSpecification": map[string]interface{}{
				"name": t.Name, "description": t.Description,
				"inputSchema": map[string]interface{}{"json": t.Parameters},
			},
		}
	}
	return tools
}

func extractSystemPrompt(messages []ir.Message) string {
	var parts []string
	for _, msg := range messages {
		if msg.Role == ir.RoleSystem {
			parts = append(parts, ir.CombineTextParts(msg))
		}
	}
	return strings.Join(parts, "\n")
}

func processMessages(messages []ir.Message, tools []interface{}, modelID, origin string) ([]interface{}, map[string]interface{}) {
	nonSystem := filterSystemMessages(messages)
	nonSystem = mergeConsecutiveMessages(nonSystem)

	// Handle prefill: if last message is from assistant without tool_calls, it's a prefill
	// Kiro doesn't support prefill, so we remove it
	nonSystem = removePrefill(nonSystem)

	nonSystem = alternateRoles(nonSystem)

	if len(nonSystem) == 0 {
		return nil, nil
	}

	lastMsg := nonSystem[len(nonSystem)-1]
	if lastMsg.Role == ir.RoleUser {
		history := buildHistory(nonSystem[:len(nonSystem)-1], tools, modelID, origin)
		return history, convertMessage(lastMsg, tools, modelID, origin, true)
	}

	// Handle trailing tool messages
	trailingStart := findTrailingStart(nonSystem)
	history := buildHistory(nonSystem[:trailingStart], tools, modelID, origin)

	var currentMessage map[string]interface{}
	if trailingStart < len(nonSystem) {
		currentMessage = buildMergedToolResultMessage(nonSystem[trailingStart:], tools, modelID, origin)
	} else {
		currentMessage = convertMessage(nonSystem[len(nonSystem)-1], tools, modelID, origin, true)
	}
	return history, currentMessage
}

// removePrefill removes trailing assistant messages that are prefills (no tool_calls).
// Prefill is a technique where the assistant message contains the start of the expected response
// to guide the model's output format. Kiro doesn't support this.
func removePrefill(messages []ir.Message) []ir.Message {
	if len(messages) == 0 {
		return messages
	}

	// Check if last message is an assistant message without tool_calls (prefill)
	lastIdx := len(messages) - 1
	lastMsg := messages[lastIdx]

	if lastMsg.Role == ir.RoleAssistant && len(lastMsg.ToolCalls) == 0 {
		// This is a prefill - remove it
		// Note: We could alternatively append the prefill content to the previous user message
		// as a hint, but for now we just remove it as Kiro doesn't support prefill
		return messages[:lastIdx]
	}

	return messages
}

func filterSystemMessages(messages []ir.Message) []ir.Message {
	var result []ir.Message
	for _, msg := range messages {
		if msg.Role != ir.RoleSystem {
			result = append(result, msg)
		}
	}
	return result
}

func mergeConsecutiveMessages(messages []ir.Message) []ir.Message {
	if len(messages) <= 1 {
		return messages
	}
	merged := make([]ir.Message, 0, len(messages))
	for _, msg := range messages {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.Role == msg.Role && msg.Role != ir.RoleUser {
				last.Content = append(last.Content, msg.Content...)
				continue
			}
		}
		merged = append(merged, msg)
	}
	return merged
}

func alternateRoles(messages []ir.Message) []ir.Message {
	var alternated []ir.Message
	for i, msg := range messages {
		if i > 0 {
			prev, curr := messages[i-1].Role, msg.Role
			isUserLike := func(r ir.Role) bool { return r == ir.RoleUser || r == ir.RoleTool }
			if isUserLike(prev) && isUserLike(curr) {
				alternated = append(alternated, ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Type: ir.ContentTypeText, Text: "[Continued]"}}})
			} else if prev == ir.RoleAssistant && curr == ir.RoleAssistant {
				alternated = append(alternated, ir.Message{Role: ir.RoleUser, Content: []ir.ContentPart{{Type: ir.ContentTypeText, Text: "Continue"}}})
			}
		}
		alternated = append(alternated, msg)
	}
	return alternated
}

func findTrailingStart(messages []ir.Message) int {
	trailingStart := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == ir.RoleTool {
			trailingStart = i
		} else {
			break
		}
	}
	return trailingStart
}

func buildHistory(messages []ir.Message, tools []interface{}, modelID, origin string) []interface{} {
	history := make([]interface{}, 0, len(messages))
	for _, msg := range messages {
		if m := convertMessage(msg, tools, modelID, origin, false); m != nil {
			history = append(history, m)
		}
	}
	return history
}

func convertMessage(msg ir.Message, tools []interface{}, modelID, origin string, isCurrent bool) map[string]interface{} {
	switch msg.Role {
	case ir.RoleUser:
		return buildUserMessage(msg, tools, modelID, origin, isCurrent)
	case ir.RoleAssistant:
		return buildAssistantMessage(msg)
	case ir.RoleTool:
		return buildToolResultMessage(msg, modelID, origin)
	}
	return nil
}

func buildUserMessage(msg ir.Message, tools []interface{}, modelID, origin string, isCurrent bool) map[string]interface{} {
	content := ir.CombineTextParts(msg)
	var toolResults, images []interface{}
	for _, part := range msg.Content {
		if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
			toolResults = append(toolResults, buildToolResultItem(part.ToolResult))
		} else if part.Type == ir.ContentTypeImage && part.Image != nil {
			images = append(images, buildImageItem(part.Image))
		}
	}

	if isCurrent && content == "" && len(toolResults) == 0 {
		content = "Continue"
	}

	ctx := map[string]interface{}{}
	if len(toolResults) > 0 {
		ctx["toolResults"] = toolResults
	}
	if isCurrent && len(tools) > 0 {
		ctx["tools"] = tools
	}

	userInput := map[string]interface{}{
		"content": content, "modelId": modelID, "origin": origin, "userInputMessageContext": ctx,
	}
	if len(images) > 0 {
		userInput["images"] = images
	} else if isCurrent {
		userInput["images"] = nil
	}

	return map[string]interface{}{"userInputMessage": userInput}
}

func buildAssistantMessage(msg ir.Message) map[string]interface{} {
	toolUses := make([]interface{}, len(msg.ToolCalls))
	for i, tc := range msg.ToolCalls {
		toolUses[i] = map[string]interface{}{
			"input": ir.ParseToolCallArgs(tc.Args), "name": tc.Name, "toolUseId": tc.ID,
		}
	}
	assistantMsg := map[string]interface{}{"content": ir.CombineTextParts(msg), "toolUses": toolUses}
	return map[string]interface{}{"assistantResponseMessage": assistantMsg}
}

func buildToolResultMessage(msg ir.Message, modelID, origin string) map[string]interface{} {
	var toolResults []interface{}
	for _, part := range msg.Content {
		if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
			toolResults = append(toolResults, buildToolResultItem(part.ToolResult))
		}
	}
	if len(toolResults) == 0 {
		return nil
	}
	return map[string]interface{}{
		"userInputMessage": map[string]interface{}{
			"content": "Continue", "modelId": modelID, "origin": origin, "images": []interface{}{},
			"userInputMessageContext": map[string]interface{}{"toolResults": toolResults},
		},
	}
}

func buildMergedToolResultMessage(msgs []ir.Message, tools []interface{}, modelID, origin string) map[string]interface{} {
	var toolResults []interface{}
	var textParts []string
	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
				toolResults = append(toolResults, buildToolResultItem(part.ToolResult))
			} else if part.Type == ir.ContentTypeText && part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		}
	}
	content := "Continue"
	if len(textParts) > 0 {
		content = strings.Join(textParts, "\n")
	}
	ctx := map[string]interface{}{"toolResults": toolResults}
	if len(tools) > 0 {
		ctx["tools"] = tools
	}
	return map[string]interface{}{
		"userInputMessage": map[string]interface{}{
			"content": content, "modelId": modelID, "origin": origin, "images": nil, "userInputMessageContext": ctx,
		},
	}
}

func buildToolResultItem(tr *ir.ToolResultPart) map[string]interface{} {
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"text": ir.SanitizeText(tr.Result)}},
		"status":  "success", "toolUseId": tr.ToolCallID,
	}
}

func buildImageItem(img *ir.ImagePart) map[string]interface{} {
	format := "png"
	if parts := strings.Split(img.MimeType, "/"); len(parts) == 2 {
		format = parts[1]
	}
	return map[string]interface{}{"format": format, "source": map[string]interface{}{"bytes": img.Data}}
}

func injectSystemPrompt(prompt string, history *[]interface{}, currentMessage map[string]interface{}, modelID, origin string) {
	if prompt == "" {
		return
	}
	prepend := func(msg interface{}) bool {
		if m, ok := msg.(map[string]interface{}); ok {
			if userMsg, ok := m["userInputMessage"].(map[string]interface{}); ok {
				if existing, _ := userMsg["content"].(string); existing != "" {
					userMsg["content"] = prompt + "\n\n" + existing
				} else {
					userMsg["content"] = prompt
				}
				return true
			}
		}
		return false
	}

	if len(*history) > 0 && prepend((*history)[0]) {
		return
	}
	if currentMessage != nil && prepend(currentMessage) {
		return
	}

	*history = append([]interface{}{map[string]interface{}{
		"userInputMessage": map[string]interface{}{
			"content": prompt, "modelId": modelID, "origin": origin,
		},
	}}, *history...)
}

// injectThinkingMode adds thinking mode configuration to the system prompt if enabled.
// Kiro API supports official thinking/reasoning mode via <thinking_mode> tag.
// When set to "enabled", Kiro returns reasoning content as official reasoningContentEvent.
// Supports multiple detection methods:
// - ThinkingConfig.IncludeThoughts = true
// - ThinkingConfig.Budget > 0 or Budget == -1 (auto)
// - ThinkingConfig.Effort != "" and != "none"
func injectThinkingMode(thinking *ir.ThinkingConfig, systemPrompt string) string {
	if thinking == nil {
		return systemPrompt
	}

	// Determine if thinking should be enabled
	thinkingEnabled := thinking.IncludeThoughts ||
		thinking.Budget > 0 ||
		thinking.Budget == -1 ||
		(thinking.Effort != "" && thinking.Effort != "none")

	if !thinkingEnabled {
		return systemPrompt
	}

	// Determine max thinking length based on budget
	// Default to 200000 tokens for extensive reasoning
	maxThinkingLength := 200000
	if thinking.Budget > 0 {
		maxThinkingLength = thinking.Budget
	}

	// Build thinking mode hint
	thinkingHint := "<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>" +
		strconv.Itoa(maxThinkingLength) + "</max_thinking_length>"

	if systemPrompt != "" {
		return thinkingHint + "\n\n" + systemPrompt
	}
	return thinkingHint
}

// Kiro thinking tag constants for response parsing
const (
	KiroThinkingStartTag = "<thinking>"
	KiroThinkingEndTag   = "</thinking>"
)
