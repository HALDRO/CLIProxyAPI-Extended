/**
 * @file Kiro (Amazon Q) request converter
 * @description Converts unified format into Kiro API request format using strict structs.
 */

package from_ir

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
)

// KiroProvider handles conversion from unified format to Kiro API format.
type KiroProvider struct{}

type KiroRequest struct {
	ConversationState ConversationState `json:"conversationState"`
	ProfileArn        string            `json:"profileArn,omitempty"`
	InferenceConfig   *InferenceConfig  `json:"inferenceConfig,omitempty"`
}

type ConversationState struct {
	AgentContinuationId string           `json:"agentContinuationId,omitempty"`
	AgentTaskType       string           `json:"agentTaskType,omitempty"`
	ChatTriggerType     string           `json:"chatTriggerType"`
	ConversationId      string           `json:"conversationId"`
	CurrentMessage      CurrentMessage   `json:"currentMessage"`
	History             []HistoryMessage `json:"history"`
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
	Json interface{} `json:"json"`
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
	Input     interface{} `json:"input"`
}

type ImageItem struct {
	Format string      `json:"format"`
	Source ImageSource `json:"source"`
}

type ImageSource struct {
	Bytes interface{} `json:"bytes"`
}

type kiroConversation struct {
	History []kiroSemanticTurn
	Current kiroSemanticUserTurn
}

type kiroSemanticTurn struct {
	User      *kiroSemanticUserTurn
	Assistant *kiroSemanticAssistantTurn
}

type kiroSemanticUserTurn struct {
	Text        string
	ToolResults []ToolResult
	Images      []ImageItem
}

type kiroSemanticAssistantTurn struct {
	Text     string
	ToolUses []ToolUse
}

const remoteWebSearchDescription = "WebSearch looks up information outside the model's training data. Supports multiple queries to gather comprehensive information."

const (
	kiroTurnPlaceholder     = "Continue."
	kiroWriteToolSuffix     = "\n- IMPORTANT: If the content to write exceeds 150 lines, you MUST only write the first 50 lines using this tool, then use `Edit` tool to append the remaining content in chunks of no more than 50 lines each. If needed, leave a unique placeholder to help append content. Do NOT attempt to write all content at once."
	kiroEditToolSuffix      = "\n- IMPORTANT: If the `new_string` content exceeds 50 lines, you MUST split it into multiple Edit calls, each replacing no more than 50 lines at a time. If used to append content, leave a unique placeholder to help append content. On the final chunk, do NOT include the placeholder."
	kiroMinThinkingBudget   = 1024
	kiroMaxThinkingBudget   = 24576
	kiroDefaultBudget       = 20000
	kiroMaxHistoryMessages  = 999
	kiroAgenticSystemPrompt = `When the Write or Edit tool has content size limits, always comply silently. Never suggest bypassing these limits via alternative tools. Never ask the user whether to switch approaches. Complete all chunked operations without commentary.`
)

var kiroChunkedWriteToolNames = map[string]string{
	"write":         kiroWriteToolSuffix,
	"write_to_file": kiroWriteToolSuffix,
	"fswrite":       kiroWriteToolSuffix,
	"edit":          kiroEditToolSuffix,
	"apply_diff":    kiroEditToolSuffix,
}

// ConvertRequest converts UnifiedChatRequest to Kiro API JSON format.
func (p *KiroProvider) ConvertRequest(req *ir.UnifiedChatRequest) ([]byte, error) {
	origin := extractOrigin(req)
	conversationID := extractConversationID(req)
	continuationID := deriveNextContinuationID(req, conversationID)

	systemPrompt := extractSystemPrompt(req.Messages)
	tools, err := extractToolsStruct(req.Tools)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(systemPrompt, kiroAgenticSystemPrompt) {
		if systemPrompt != "" {
			systemPrompt += "\n" + kiroAgenticSystemPrompt
		} else {
			systemPrompt = kiroAgenticSystemPrompt
		}
	}
	thinkingHint := buildKiroThinkingHint(req)

	if thinkingHint != "" && !hasThinkingConfigTags(systemPrompt) {
		if systemPrompt != "" {
			systemPrompt = thinkingHint + "\n\n" + systemPrompt
		} else {
			systemPrompt = thinkingHint
		}
	}

	history, currentMsg := processMessagesStruct(req.Messages, tools, req.Model, origin)

	if systemPrompt != "" {
		injectSystemPromptStruct(systemPrompt, history, &currentMsg)
	}

	request := KiroRequest{
		ConversationState: ConversationState{
			AgentTaskType:   "vibe",
			ChatTriggerType: "MANUAL",
			ConversationId:  conversationID,
			CurrentMessage:  currentMsg,
			History:         history,
		},
	}

	if continuationID != "" {
		request.ConversationState.AgentContinuationId = continuationID
	}
	if request.ConversationState.History == nil {
		request.ConversationState.History = []HistoryMessage{}
	}
	if req.Metadata != nil {
		if arn, ok := req.Metadata["profileArn"].(string); ok && arn != "" {
			request.ProfileArn = arn
		}
	}

	infConfig := &InferenceConfig{}
	hasConfig := false
	if req.MaxTokens != nil {
		val := *req.MaxTokens
		if val == -1 {
			val = 32000
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

func extractConversationID(req *ir.UnifiedChatRequest) string {
	if req != nil && req.Metadata != nil {
		if id, ok := req.Metadata["conversationId"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
		if sid, ok := req.Metadata["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
			return deriveStableKiroConversationID(strings.TrimSpace(sid))
		}
	}
	if req != nil {
		// Transcript imports may preserve provider conversation identity only on the
		// assistant message metadata. Prefer an explicit assistant conversationId
		// before falling back to a random UUID when session_id is unavailable.
		for i := len(req.Messages) - 1; i >= 0; i-- {
			msg := req.Messages[i]
			if msg.Role != ir.RoleAssistant || len(msg.Metadata) == 0 {
				continue
			}
			if id, ok := msg.Metadata["conversationId"].(string); ok && strings.TrimSpace(id) != "" {
				return strings.TrimSpace(id)
			}
		}
	}
	return uuid.New().String()
}

func deriveStableKiroConversationID(sessionID string) string {
	hash := sha256.Sum256([]byte("kiro-conv:" + sessionID))
	return fmt.Sprintf("%x-%x-%x-%x-%x", hash[0:4], hash[4:6], hash[6:8], hash[8:10], hash[10:16])
}

func extractContinuationID(req *ir.UnifiedChatRequest) string {
	if req != nil && req.Metadata != nil {
		if id, ok := req.Metadata["continuationId"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	if req != nil {
		for i := len(req.Messages) - 1; i >= 0; i-- {
			msg := req.Messages[i]
			if msg.Role != ir.RoleAssistant || len(msg.Metadata) == 0 {
				continue
			}
			mode, _ := msg.Metadata["continuation_mode"].(string)
			if mode != "requires_tool_result" && mode != "resume_assistant" {
				continue
			}
			contID, _ := msg.Metadata["continuationId"].(string)
			if strings.TrimSpace(contID) == "" {
				continue
			}
			return strings.TrimSpace(contID)
		}
	}
	return ""
}

func deriveNextContinuationID(req *ir.UnifiedChatRequest, conversationID string) string {
	_ = conversationID
	if req == nil {
		return ""
	}
	return extractContinuationID(req)
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

func injectSystemPromptStruct(prompt string, history []HistoryMessage, currentMsg *CurrentMessage) {
	if prompt == "" {
		return
	}
	for i := range history {
		if history[i].UserInputMessage == nil {
			continue
		}
		history[i].UserInputMessage.Content = prependPrompt(prompt, history[i].UserInputMessage.Content)
		return
	}
	if currentMsg != nil {
		currentMsg.UserInputMessage.Content = prependPrompt(prompt, currentMsg.UserInputMessage.Content)
	}
}

func processMessagesStruct(messages []ir.Message, tools []ToolSpecification, modelID, origin string) ([]HistoryMessage, CurrentMessage) {
	nonSystem := filterSystemMessages(messages)
	conversation := buildKiroConversation(nonSystem)
	conversation.History = truncateSemanticHistoryIfNeeded(conversation.History)
	conversation = degradeOrphanedToolResults(conversation)
	return serializeKiroConversation(conversation, tools, modelID, origin)
}

func buildKiroConversation(messages []ir.Message) kiroConversation {
	conversation := kiroConversation{History: make([]kiroSemanticTurn, 0, len(messages))}
	turns := normalizeKiroTurns(messages)
	if len(turns) == 0 {
		return conversation
	}
	lastIdx := len(turns) - 1
	for i, turn := range turns {
		if i == lastIdx && turn.User != nil {
			conversation.Current = *turn.User
			continue
		}
		conversation.History = append(conversation.History, turn)
	}
	conversation.Current.ToolResults = dedupeToolResults(conversation.Current.ToolResults)
	return conversation
}

func dedupeToolResults(results []ToolResult) []ToolResult {
	if len(results) <= 1 {
		return results
	}
	deduped := make([]ToolResult, 0, len(results))
	seenByID := make(map[string]struct{})
	for _, result := range results {
		result.ToolUseId = strings.TrimSpace(result.ToolUseId)
		if result.ToolUseId != "" {
			if _, ok := seenByID[result.ToolUseId]; ok {
				continue
			}
			seenByID[result.ToolUseId] = struct{}{}
		}
		deduped = append(deduped, result)
	}
	return deduped
}

func degradeOrphanedToolResults(conversation kiroConversation) kiroConversation {
	validToolUseIDs := collectValidToolUseIDs(conversation.History)
	for i := range conversation.History {
		if conversation.History[i].User == nil {
			continue
		}
		conversation.History[i].User = degradeToolResultsForTurn(conversation.History[i].User, validToolUseIDs)
	}
	conversation.Current = *degradeToolResultsForTurn(&conversation.Current, validToolUseIDs)
	return conversation
}

func truncateSemanticHistoryIfNeeded(history []kiroSemanticTurn) []kiroSemanticTurn {
	if len(history) <= kiroMaxHistoryMessages {
		return history
	}
	units := buildKiroHistoryUnits(history)
	if len(units) == 0 {
		return history[len(history)-kiroMaxHistoryMessages:]
	}
	kept := make([]kiroSemanticTurn, 0, min(len(history), kiroMaxHistoryMessages))
	remaining := kiroMaxHistoryMessages
	for i := len(units) - 1; i >= 0; i-- {
		unit := units[i]
		if len(kept) > 0 && len(unit.messages) > remaining {
			break
		}
		if len(kept) == 0 && len(unit.messages) > remaining {
			continue
		}
		remaining -= len(unit.messages)
		kept = append(unit.messages, kept...)
		if remaining == 0 {
			break
		}
	}
	if len(kept) == 0 {
		return history[len(history)-kiroMaxHistoryMessages:]
	}
	return kept
}

type kiroHistoryUnit struct {
	messages []kiroSemanticTurn
}

func buildKiroHistoryUnits(history []kiroSemanticTurn) []kiroHistoryUnit {
	units := make([]kiroHistoryUnit, 0, len(history))
	for i := 0; i < len(history); i++ {
		current := history[i]
		if current.Assistant != nil && i+1 < len(history) && isLinkedToolResultTurn(current.Assistant, history[i+1].User) {
			units = append(units, kiroHistoryUnit{messages: []kiroSemanticTurn{current, history[i+1]}})
			i++
			continue
		}
		units = append(units, kiroHistoryUnit{messages: []kiroSemanticTurn{current}})
	}
	return units
}

func isLinkedToolResultTurn(assistant *kiroSemanticAssistantTurn, user *kiroSemanticUserTurn) bool {
	if assistant == nil || user == nil {
		return false
	}
	if len(assistant.ToolUses) == 0 || len(user.ToolResults) == 0 {
		return false
	}
	toolUseIDs := make(map[string]struct{}, len(assistant.ToolUses))
	for _, toolUse := range assistant.ToolUses {
		id := strings.TrimSpace(toolUse.ToolUseId)
		if id != "" {
			toolUseIDs[id] = struct{}{}
		}
	}
	for _, toolResult := range user.ToolResults {
		id := strings.TrimSpace(toolResult.ToolUseId)
		if id == "" {
			continue
		}
		if _, ok := toolUseIDs[id]; ok {
			return true
		}
	}
	return false
}

func serializeKiroConversation(conversation kiroConversation, tools []ToolSpecification, modelID, origin string) ([]HistoryMessage, CurrentMessage) {
	history := make([]HistoryMessage, 0, len(conversation.History)+1)
	if len(conversation.History) > 0 && conversation.History[0].Assistant != nil {
		history = append(history, HistoryMessage{UserInputMessage: serializeSemanticUserTurn(kiroSemanticUserTurn{}, modelID, origin, nil)})
	}
	for _, turn := range conversation.History {
		switch {
		case turn.User != nil:
			history = append(history, HistoryMessage{UserInputMessage: serializeSemanticUserTurn(*turn.User, modelID, origin, nil)})
		case turn.Assistant != nil:
			history = append(history, HistoryMessage{AssistantResponseMessage: serializeSemanticAssistantTurn(*turn.Assistant)})
		}
	}
	return history, CurrentMessage{UserInputMessage: *serializeSemanticUserTurn(conversation.Current, modelID, origin, tools)}
}

func buildSemanticUserTurn(msg ir.Message) kiroSemanticUserTurn {
	turn := kiroSemanticUserTurn{Text: strings.TrimSpace(ir.CombineTextParts(msg))}
	for _, part := range msg.Content {
		if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
			turn.ToolResults = append(turn.ToolResults, buildToolResultStruct(part.ToolResult))
		}
		if part.Type == ir.ContentTypeImage && part.Image != nil {
			turn.Images = append(turn.Images, buildImageItemStruct(part.Image))
		}
	}
	return turn
}

func buildSemanticAssistantTurn(msg ir.Message) kiroSemanticAssistantTurn {
	turn := kiroSemanticAssistantTurn{Text: strings.TrimSpace(ir.CombineTextParts(msg))}
	for _, tc := range msg.ToolCalls {
		name := tc.Name
		if ir.IsNetworkingToolName(name) {
			name = "remote_web_search"
		}
		turn.ToolUses = append(turn.ToolUses, ToolUse{ToolUseId: reverseConvertToolID(tc.ID), Name: name, Input: ir.ParseToolCallArgs(tc.Args)})
	}
	return turn
}

func serializeSemanticUserTurn(turn kiroSemanticUserTurn, modelID, origin string, tools []ToolSpecification) *UserInputMessage {
	content := strings.TrimSpace(turn.Text)
	if content == "" {
		content = kiroTurnPlaceholder
	}
	msg := &UserInputMessage{Content: content, ModelId: modelID, Origin: origin}
	if len(turn.Images) > 0 {
		msg.Images = append([]ImageItem(nil), turn.Images...)
	}
	if len(tools) > 0 || len(turn.ToolResults) > 0 {
		ctx := &UserInputMessageContext{}
		if len(tools) > 0 {
			ctx.Tools = append([]ToolSpecification(nil), tools...)
		}
		if len(turn.ToolResults) > 0 {
			ctx.ToolResults = append([]ToolResult(nil), turn.ToolResults...)
		}
		msg.UserInputMessageContext = ctx
	}
	return msg
}

func serializeSemanticAssistantTurn(turn kiroSemanticAssistantTurn) *AssistantResponseMessage {
	content := strings.TrimSpace(turn.Text)
	if content == "" {
		content = kiroTurnPlaceholder
	}
	return &AssistantResponseMessage{Content: content, ToolUses: append([]ToolUse(nil), turn.ToolUses...)}
}

func buildImageItemStruct(img *ir.ImagePart) ImageItem {
	format := "png"
	if parts := strings.Split(img.MimeType, "/"); len(parts) == 2 {
		format = parts[1]
	}
	return ImageItem{Format: format, Source: ImageSource{Bytes: img.Data}}
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

func normalizeKiroTurns(messages []ir.Message) []kiroSemanticTurn {
	turns := make([]kiroSemanticTurn, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case ir.RoleUser:
			userTurn := buildSemanticUserTurn(msg)
			turns = append(turns, kiroSemanticTurn{User: &userTurn})
		case ir.RoleAssistant:
			assistantTurn := buildSemanticAssistantTurn(msg)
			turns = append(turns, kiroSemanticTurn{Assistant: &assistantTurn})
		case ir.RoleTool:
			userTurn := buildSemanticUserTurn(msg)
			if len(userTurn.ToolResults) == 0 && userTurn.Text == "" {
				continue
			}
			turns = append(turns, kiroSemanticTurn{User: &userTurn})
		}
	}
	return turns
}

func collectValidToolUseIDs(history []kiroSemanticTurn) map[string]struct{} {
	validToolUseIDs := make(map[string]struct{})
	for _, turn := range history {
		if turn.Assistant == nil {
			continue
		}
		for _, toolUse := range turn.Assistant.ToolUses {
			if id := strings.TrimSpace(toolUse.ToolUseId); id != "" {
				validToolUseIDs[id] = struct{}{}
			}
		}
	}
	return validToolUseIDs
}

func degradeToolResultsForTurn(turn *kiroSemanticUserTurn, validToolUseIDs map[string]struct{}) *kiroSemanticUserTurn {
	if turn == nil || len(turn.ToolResults) == 0 {
		return turn
	}
	matched := make([]ToolResult, 0, len(turn.ToolResults))
	orphaned := make([]ToolResult, 0, len(turn.ToolResults))
	for _, result := range dedupeToolResults(turn.ToolResults) {
		id := strings.TrimSpace(result.ToolUseId)
		if id != "" {
			if _, ok := validToolUseIDs[id]; ok {
				matched = append(matched, result)
				continue
			}
		}
		orphaned = append(orphaned, result)
	}
	turn.ToolResults = matched
	if len(orphaned) > 0 {
		orphanedText := summarizeToolResults(orphaned)
		if orphanedText != "" {
			if strings.TrimSpace(turn.Text) == "" {
				turn.Text = orphanedText
			} else {
				turn.Text = turn.Text + "\n\n" + orphanedText
			}
		}
	}
	return turn
}

func summarizeToolResults(results []ToolResult) string {
	if len(results) == 0 {
		return ""
	}
	parts := make([]string, 0, len(results))
	for _, result := range results {
		id := strings.TrimSpace(result.ToolUseId)
		text := strings.TrimSpace(toolResultContentText(result.Content))
		if id == "" && text == "" {
			continue
		}
		label := "Tool result"
		if id != "" {
			label += " " + id
		}
		if text != "" {
			label += ": " + text
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, "\n")
}

func toolResultContentText(content []ToolResultContent) string {
	parts := make([]string, 0, len(content))
	for _, item := range content {
		if text := strings.TrimSpace(item.Text); text != "" {
			parts = append(parts, text)
			continue
		}
		if item.Json != nil {
			if raw, err := json.Marshal(item.Json); err == nil && len(raw) > 0 {
				parts = append(parts, string(raw))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func prependPrompt(prompt, content string) string {
	trimmedContent := strings.TrimSpace(content)
	if trimmedContent == "" || trimmedContent == kiroTurnPlaceholder {
		return prompt
	}
	return prompt + "\n\n" + content
}

func reverseConvertToolID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return strings.Replace(id, "call_", "tooluse_", 1)
	}
	return id
}

func shortenToolNameIfNeeded(name string) string {
	if len(name) > 64 {
		return name[:64]
	}
	return name
}

func ensureKiroInputSchema(parameters map[string]interface{}) map[string]interface{} {
	if parameters != nil {
		return parameters
	}
	return map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
}

func extractToolsStruct(irTools []ir.ToolDefinition) ([]ToolSpecification, error) {
	if len(irTools) == 0 {
		return nil, nil
	}
	tools := make([]ToolSpecification, 0, len(irTools))
	for _, t := range irTools {
		cleanedSchema := ir.CleanJsonSchemaEnhanced(ir.CopyMap(t.Parameters))
		finalSchema := ensureKiroInputSchema(cleanedSchema)
		name := shortenToolNameIfNeeded(t.Name)
		description := t.Description
		if ir.IsNetworkingToolName(name) {
			name = "remote_web_search"
			if description == "" {
				description = remoteWebSearchDescription
			}
		}
		if suffix, ok := kiroChunkedWriteToolNames[strings.ToLower(name)]; ok {
			description += suffix
			if len([]rune(description)) > 10000 {
				runes := []rune(description)
				description = string(runes[:10000])
			}
		}
		tools = append(tools, ToolSpecification{ToolSpecification: ToolSpecDetails{Name: name, Description: description, InputSchema: ToolInputSchema{Json: finalSchema}}})
	}
	return tools, nil
}

func buildToolResultStruct(tr *ir.ToolResultPart) ToolResult {
	status := "success"
	if tr.IsError {
		status = "error"
	}
	content := []ToolResultContent{{Text: ir.SanitizeText(tr.Result)}}
	if len(content) == 1 && strings.TrimSpace(content[0].Text) == "" {
		content[0].Text = "."
	}
	return ToolResult{ToolUseId: reverseConvertToolID(tr.ToolCallID), Status: status, Content: content}
}

func hasThinkingConfigTags(prompt string) bool {
	return strings.Contains(prompt, "<thinking_mode>") || strings.Contains(prompt, "<max_thinking_length>") || strings.Contains(prompt, "<thinking_effort>")
}

func buildKiroThinkingHint(req *ir.UnifiedChatRequest) string {
	if req == nil || req.Thinking == nil {
		return ""
	}
	thinking := req.Thinking
	if hasThinkingConfigTags(extractSystemPrompt(req.Messages)) {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(thinking.Effort), "none") {
		return ""
	}
	if !thinking.IncludeThoughts && thinking.Budget == 0 {
		return ""
	}
	return `<thinking_mode>enabled</thinking_mode>
<max_thinking_length>` + normalizeKiroThinkingBudget(thinking.Budget) + `</max_thinking_length>`
}

func normalizeKiroThinkingBudget(budget int) string {
	value := budget
	if value <= 0 {
		value = kiroDefaultBudget
	}
	if value < kiroMinThinkingBudget {
		value = kiroMinThinkingBudget
	}
	if value > kiroMaxThinkingBudget {
		value = kiroMaxThinkingBudget
	}
	return strconv.Itoa(value)
}

// ApplyDefaultKiroThinking preserves historical Kiro defaulting behavior for clients
// that signal reasoning mode indirectly rather than via canonical thinking fields.
func ApplyDefaultKiroThinking(req *ir.UnifiedChatRequest) {
	if req == nil || req.Thinking != nil {
		return
	}
	modelLower := strings.ToLower(req.Model)
	if strings.Contains(modelLower, "thinking") || strings.Contains(modelLower, "-reason") {
		req.Thinking = &ir.ThinkingConfig{IncludeThoughts: true, Budget: 16000}
		return
	}
	for _, msg := range req.Messages {
		if msg.Role != ir.RoleSystem {
			continue
		}
		text := ir.CombineTextParts(msg)
		if strings.Contains(text, "<thinking_mode>enabled</thinking_mode>") || strings.Contains(text, "<thinking_mode>interleaved</thinking_mode>") {
			req.Thinking = &ir.ThinkingConfig{IncludeThoughts: true, Budget: 16000}
			return
		}
	}
}
