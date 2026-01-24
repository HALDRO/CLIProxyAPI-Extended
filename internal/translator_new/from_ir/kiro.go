/**
 * @file Kiro (Amazon Q) request converter
 * @description Converts unified format into Kiro API request format using strict structs.
 */

package from_ir

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
)

// KiroProvider handles conversion from unified format to Kiro API format.
type KiroProvider struct{}

// -- Kiro API Structs --

type KiroRequest struct {
	ConversationState ConversationState `json:"conversationState"`
	ProfileArn        string            `json:"profileArn,omitempty"`
	InferenceConfig   *InferenceConfig  `json:"inferenceConfig,omitempty"`
}

type ConversationState struct {
	ChatTriggerType string           `json:"chatTriggerType"`
	ConversationId  string           `json:"conversationId"`
	CurrentMessage  CurrentMessage   `json:"currentMessage"`
	History         []HistoryMessage `json:"history"` // Can be empty list, but usually not null
}

type InferenceConfig struct {
	MaxTokens   *int     `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

type CurrentMessage struct {
	UserInputMessage UserInputMessage `json:"userInputMessage"`
}

type HistoryMessage struct {
	UserInputMessage         *UserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *AssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type UserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelId                 string                   `json:"modelId"`
	Origin                  string                   `json:"origin"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
	Images                  []ImageItem              `json:"images,omitempty"`
}

type AssistantResponseMessage struct {
	Content  string    `json:"content"`
	ToolUses []ToolUse `json:"toolUses,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []ToolSpecification `json:"tools,omitempty"`
	ToolResults []ToolResult        `json:"toolResults,omitempty"`
}

type ToolSpecification struct {
	ToolSpecification ToolSpecDetails `json:"toolSpecification"`
}

type ToolSpecDetails struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema ToolInputSchema `json:"inputSchema"`
}

type ToolInputSchema struct {
	Json interface{} `json:"json"` // raw schema
}

type ToolResult struct {
	ToolUseId string              `json:"toolUseId"`
	Content   []ToolResultContent `json:"content"`
	Status    string              `json:"status"`
}

type ToolResultContent struct {
	Text string      `json:"text,omitempty"`
	Json interface{} `json:"json,omitempty"`
}

type ToolUse struct {
	ToolUseId string      `json:"toolUseId"`
	Name      string      `json:"name"`
	Input     interface{} `json:"input"` // JSON object
}

type ImageItem struct {
	Format string      `json:"format"`
	Source ImageSource `json:"source"`
}

// Updated ImageSource to use interface{} for Bytes to prevent double encoding
type ImageSource struct {
	Bytes interface{} `json:"bytes"`
}

// -- Conversion Logic --

// ConvertRequest converts UnifiedChatRequest to Kiro API JSON format.
func (p *KiroProvider) ConvertRequest(req *ir.UnifiedChatRequest) ([]byte, error) {
	origin := extractOrigin(req)
	tools := extractToolsStruct(req.Tools)
	systemPrompt := extractSystemPrompt(req.Messages)

	// Inject thinking mode - keeping simple for now to avoid validation issues
	// systemPrompt = injectThinkingMode(req.Thinking, systemPrompt)

	history, currentMsg := processMessagesStruct(req.Messages, tools, req.Model, origin)

	// Inject system prompt
	if systemPrompt != "" {
		injectSystemPromptStruct(systemPrompt, &history, &currentMsg)
	}

	// Prepare request struct
	request := KiroRequest{
		ConversationState: ConversationState{
			ChatTriggerType: "MANUAL",
			ConversationId:  ir.GenerateUUID(),
			CurrentMessage:  currentMsg,
			History:         history,
		},
	}

	if request.ConversationState.History == nil {
		request.ConversationState.History = []HistoryMessage{}
	}

	if req.Metadata != nil {
		if arn, ok := req.Metadata["profileArn"].(string); ok && arn != "" {
			request.ProfileArn = arn
		}
	}

	// Inference Config
	infConfig := &InferenceConfig{}
	hasConfig := false
	if req.MaxTokens != nil {
		val := *req.MaxTokens
		if val == -1 {
			val = 32000 // Kiro max
		}
		infConfig.MaxTokens = &val
		hasConfig = true
	}
	if req.Temperature != nil {
		infConfig.Temperature = req.Temperature
		hasConfig = true
	}
	if req.TopP != nil {
		infConfig.TopP = req.TopP
		hasConfig = true
	}
	if hasConfig {
		request.InferenceConfig = infConfig
	}

	// Marshal
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

func extractToolsStruct(irTools []ir.ToolDefinition) []ToolSpecification {
	if len(irTools) == 0 {
		return nil
	}
	tools := make([]ToolSpecification, len(irTools))
	for i, t := range irTools {
		tools[i] = ToolSpecification{
			ToolSpecification: ToolSpecDetails{
				Name:        t.Name,
				Description: t.Description,
				InputSchema: ToolInputSchema{Json: t.Parameters},
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

func processMessagesStruct(messages []ir.Message, tools []ToolSpecification, modelID, origin string) ([]HistoryMessage, CurrentMessage) {
	nonSystem := filterSystemMessages(messages)
	nonSystem = mergeConsecutiveMessages(nonSystem)
	nonSystem = removePrefill(nonSystem)
	nonSystem = alternateRoles(nonSystem)

	if len(nonSystem) == 0 {
		// Fallback for empty conversation
		return []HistoryMessage{}, CurrentMessage{
			UserInputMessage: UserInputMessage{
				Content: "Continue",
				ModelId: modelID,
				Origin:  origin,
			},
		}
	}

	// Check logic for last message
	lastMsg := nonSystem[len(nonSystem)-1]

	// If last is User, it's CurrentMessage. Rest is history.
	if lastMsg.Role == ir.RoleUser {
		history := buildHistoryStruct(nonSystem[:len(nonSystem)-1], tools, modelID, origin)
		current := buildUserMessageStruct(lastMsg, tools, modelID, origin, true)
		return history, CurrentMessage{UserInputMessage: *current}
	}

	// If last uses tools or is assistant, we might be in a flow.
	// But usually the request to Kiro implies *User* is sending something (or Tool Result).

	// Handle trailing tool messages (User role in Kiro IR, but Tool in Unified IR)
	trailingStart := findTrailingStart(nonSystem)

	history := buildHistoryStruct(nonSystem[:trailingStart], tools, modelID, origin)

	var currentMsg UserInputMessage

	if trailingStart < len(nonSystem) {
		// We have tool results at the end
		currentMsg = buildMergedToolResultMessageStruct(nonSystem[trailingStart:], tools, modelID, origin)
	} else {
		// Last was Assistant? Kiro expects UserInput as CurrentMessage.
		// If last was Assistant, we force a "Continue" user message.
		currentMsg = UserInputMessage{
			Content: "Continue",
			ModelId: modelID,
			Origin:  origin,
		}
	}

	return history, CurrentMessage{UserInputMessage: currentMsg}
}

func buildHistoryStruct(messages []ir.Message, tools []ToolSpecification, modelID, origin string) []HistoryMessage {
	history := make([]HistoryMessage, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case ir.RoleUser:
			uMsg := buildUserMessageStruct(msg, tools, modelID, origin, false)
			history = append(history, HistoryMessage{UserInputMessage: uMsg})
		case ir.RoleAssistant:
			aMsg := buildAssistantMessageStruct(msg)
			history = append(history, HistoryMessage{AssistantResponseMessage: aMsg})
		case ir.RoleTool:
			// Tool results in history are treated as UserInputMessage in Kiro
			uMsg := buildToolResultMessageStruct(msg, modelID, origin)
			if uMsg != nil {
				history = append(history, HistoryMessage{UserInputMessage: uMsg})
			}
		}
	}
	return history
}

func buildUserMessageStruct(msg ir.Message, tools []ToolSpecification, modelID, origin string, isCurrent bool) *UserInputMessage {
	content := ir.CombineTextParts(msg)
	var toolResults []ToolResult
	var images []ImageItem

	for _, part := range msg.Content {
		if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
			toolResults = append(toolResults, buildToolResultStruct(part.ToolResult))
		} else if part.Type == ir.ContentTypeImage && part.Image != nil {
			images = append(images, buildImageItemStruct(part.Image))
		}
	}

	if isCurrent && content == "" && len(toolResults) == 0 {
		content = "Continue"
	}

	uInput := &UserInputMessage{
		Content: content,
		ModelId: modelID,
		Origin:  origin,
	}

	if len(images) > 0 {
		uInput.Images = images
	}

	// Context (Tools + ToolResults)
	hasContext := false
	ctx := UserInputMessageContext{}

	if isCurrent && len(tools) > 0 {
		ctx.Tools = tools
		hasContext = true
	}
	if len(toolResults) > 0 {
		ctx.ToolResults = toolResults
		hasContext = true
	}

	if hasContext {
		uInput.UserInputMessageContext = &ctx
	}

	return uInput
}

func buildAssistantMessageStruct(msg ir.Message) *AssistantResponseMessage {
	var toolUses []ToolUse
	for _, tc := range msg.ToolCalls {
		toolUses = append(toolUses, ToolUse{
			ToolUseId: tc.ID,
			Name:      tc.Name,
			Input:     ir.ParseToolCallArgs(tc.Args),
		})
	}
	return &AssistantResponseMessage{
		Content:  ir.CombineTextParts(msg),
		ToolUses: toolUses,
	}
}

func buildToolResultMessageStruct(msg ir.Message, modelID, origin string) *UserInputMessage {
	var toolResults []ToolResult
	for _, part := range msg.Content {
		if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
			toolResults = append(toolResults, buildToolResultStruct(part.ToolResult))
		}
	}
	if len(toolResults) == 0 {
		return nil
	}

	return &UserInputMessage{
		Content: "Continue",
		ModelId: modelID,
		Origin:  origin,
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: toolResults,
		},
	}
}

func buildMergedToolResultMessageStruct(msgs []ir.Message, tools []ToolSpecification, modelID, origin string) UserInputMessage {
	var toolResults []ToolResult
	var textParts []string

	for _, msg := range msgs {
		for _, part := range msg.Content {
			if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
				toolResults = append(toolResults, buildToolResultStruct(part.ToolResult))
			} else if part.Type == ir.ContentTypeText && part.Text != "" {
				textParts = append(textParts, part.Text)
			}
		}
	}

	content := "Continue"
	if len(textParts) > 0 {
		content = strings.Join(textParts, "\n")
	}

	ctx := UserInputMessageContext{
		ToolResults: toolResults,
	}
	if len(tools) > 0 {
		ctx.Tools = tools
	}

	return UserInputMessage{
		Content:                 content,
		ModelId:                 modelID,
		Origin:                  origin,
		UserInputMessageContext: &ctx,
	}
}

func buildToolResultStruct(tr *ir.ToolResultPart) ToolResult {
	return ToolResult{
		ToolUseId: tr.ToolCallID,
		Status:    "success",
		Content: []ToolResultContent{
			{Text: ir.SanitizeText(tr.Result)},
		},
	}
}

func buildImageItemStruct(img *ir.ImagePart) ImageItem {
	format := "png"
	if parts := strings.Split(img.MimeType, "/"); len(parts) == 2 {
		format = parts[1]
	}
	return ImageItem{
		Format: format,
		Source: ImageSource{Bytes: img.Data},
	}
}

func injectSystemPromptStruct(prompt string, history *[]HistoryMessage, currentMsg *CurrentMessage) {
	if prompt == "" {
		return
	}

	// Attempt to prepend to current message if it's user input
	if currentMsg != nil {
		if currentMsg.UserInputMessage.Content != "" {
			currentMsg.UserInputMessage.Content = prompt + "\n\n" + currentMsg.UserInputMessage.Content
		} else {
			currentMsg.UserInputMessage.Content = prompt
		}
		return
	}

	// Else prepend new history message (unlikely fallback)
	*history = append([]HistoryMessage{{
		UserInputMessage: &UserInputMessage{
			Content: prompt,
			ModelId: "auto",
			Origin:  "CLI",
		},
	}}, *history...)
}

// Helpers from original code
// removePrefill removes trailing assistant messages that are prefills (no tool_calls).
func removePrefill(messages []ir.Message) []ir.Message {
	if len(messages) == 0 {
		return messages
	}
	lastIdx := len(messages) - 1
	lastMsg := messages[lastIdx]
	if lastMsg.Role == ir.RoleAssistant && len(lastMsg.ToolCalls) == 0 {
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
