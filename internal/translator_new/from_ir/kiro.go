/**
 * @file Kiro (Amazon Q) request converter
 * @description Converts unified format into Kiro API request format using strict structs.
 */

package from_ir

import (
	"crypto/sha256"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"

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
	AgentContinuationId string           `json:"agentContinuationId,omitempty"`
	AgentTaskType       string           `json:"agentTaskType,omitempty"`
	ChatTriggerType     string           `json:"chatTriggerType"`
	ConversationId      string           `json:"conversationId"`
	CurrentMessage      CurrentMessage   `json:"currentMessage"`
	History             []HistoryMessage `json:"history"` // Can be empty list, but usually not null
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

// reverseConvertToolID converts OpenAI-style "call_" IDs back to Kiro-native
// "tooluse_" format. This is the inverse of to_ir.convertToolID and is required
// so that tool_result IDs match the tool_use IDs that Kiro API originally issued.
// Without this, Kiro cannot pair tool results with tool calls → infinite loop.
func reverseConvertToolID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return strings.Replace(id, "call_", "tooluse_", 1)
	}
	return id
}

// -- Conversion Logic --

// remoteWebSearchDescription is a minimal fallback for when dynamic fetch from MCP tools/list hasn't completed yet.
const remoteWebSearchDescription = "WebSearch looks up information outside the model's training data. Supports multiple queries to gather comprehensive information."

const (
	kiroMinThinkingBudget = 1024
	kiroMaxThinkingBudget = 24576
	kiroDefaultBudget     = 20000

	// kiroUserPlaceholderToolResults is used when a user message contains only
	// tool_result blocks with no text. Kiro API requires non-empty content.
	// Matches kiro.rs behavior — descriptive but non-instructive.
	kiroUserPlaceholderToolResults = "Tool results provided."

	// kiroUserPlaceholderEmpty is used when a synthetic user message is needed
	// (e.g., last message was assistant, Kiro expects user as CurrentMessage).
	// Match working Kiro clients, which use "Continue" to resume the current turn.
	kiroUserPlaceholderEmpty = "Continue"

	// kiroAssistantPlaceholder is used when inserting synthetic assistant messages
	// so history ends with assistantResponseMessage before a current user turn.
	kiroAssistantPlaceholder = "Continue"

	// Chunked write/edit tool description suffixes (from kiro.rs converter.rs).
	// Appended to Write/Edit tool descriptions to prevent output truncation on large files.
	kiroWriteToolSuffix = "\n- IMPORTANT: If the content to write exceeds 150 lines, you MUST only write the first 50 lines using this tool, then use `Edit` tool to append the remaining content in chunks of no more than 50 lines each. If needed, leave a unique placeholder to help append content. Do NOT attempt to write all content at once."
	kiroEditToolSuffix  = "\n- IMPORTANT: If the `new_string` content exceeds 50 lines, you MUST split it into multiple Edit calls, each replacing no more than 50 lines at a time. If used to append content, leave a unique placeholder to help append content. On the final chunk, do NOT include the placeholder."
)

// ConvertRequest converts UnifiedChatRequest to Kiro API JSON format.
func (p *KiroProvider) ConvertRequest(req *ir.UnifiedChatRequest) ([]byte, error) {
	origin := extractOrigin(req)
	tools := extractToolsStruct(req.Tools)
	conversationID := extractConversationID(req)
	continuationID := extractContinuationID(req)

	systemPrompt := extractSystemPrompt(req.Messages)
	thinkingHint := buildKiroThinkingHint(req)

	// Inject thinking mode configuration if present.
	// Kiro/Amazon Q uses prompt tags rather than structured API fields.
	// Avoid duplicate injection when client already provided thinking tags.
	if thinkingHint != "" && !hasThinkingConfigTags(systemPrompt) {
		if systemPrompt != "" {
			systemPrompt = thinkingHint + "\n\n" + systemPrompt
		} else {
			systemPrompt = thinkingHint
		}
	}

	history, currentMsg := processMessagesStruct(req.Messages, tools, req.Model, origin)

	// Validate tool_use/tool_result pairing: remove orphaned tool_uses from history
	// that have no matching tool_result anywhere (history + current message).
	// This prevents Kiro API errors when compaction or truncation drops tool_result messages.
	removeOrphanedToolUses(&history, &currentMsg)

	// Ensure tools list includes placeholder definitions for any tool names
	// referenced in history but missing from the current tools list.
	// This prevents Kiro API errors when clients change tool sets between requests.
	tools = ensurePlaceholderTools(tools, history)

	// Re-attach tools to current message context if needed
	if currentMsg.UserInputMessage.UserInputMessageContext != nil && len(tools) > 0 {
		currentMsg.UserInputMessage.UserInputMessageContext.Tools = tools
	}

	// Inject system prompt
	if systemPrompt != "" {
		injectSystemPromptStruct(systemPrompt, &history, &currentMsg)
	}

	// Prepare request struct
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

func extractConversationID(req *ir.UnifiedChatRequest) string {
	if req != nil && req.Metadata != nil {
		if id, ok := req.Metadata["conversationId"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
		if sid, ok := req.Metadata["session_id"].(string); ok && strings.TrimSpace(sid) != "" {
			return deriveStableKiroConversationID(strings.TrimSpace(sid))
		}
	}
	return uuid.New().String()
}

func deriveStableKiroConversationID(sessionID string) string {
	hash := sha256.Sum256([]byte("kiro-conv:" + sessionID))
	hex := strconv.FormatUint(uint64(hash[0])<<56|uint64(hash[1])<<48|uint64(hash[2])<<40|uint64(hash[3])<<32|uint64(hash[4])<<24|uint64(hash[5])<<16|uint64(hash[6])<<8|uint64(hash[7]), 16)
	for len(hex) < 16 {
		hex = "0" + hex
	}
	hex2 := strconv.FormatUint(uint64(hash[8])<<56|uint64(hash[9])<<48|uint64(hash[10])<<40|uint64(hash[11])<<32|uint64(hash[12])<<24|uint64(hash[13])<<16|uint64(hash[14])<<8|uint64(hash[15]), 16)
	for len(hex2) < 16 {
		hex2 = "0" + hex2
	}
	combined := hex + hex2
	return combined[0:8] + "-" + combined[8:12] + "-" + combined[12:16] + "-" + combined[16:20] + "-" + combined[20:32]
}

func extractContinuationID(req *ir.UnifiedChatRequest) string {
	if req != nil && req.Metadata != nil {
		if id, ok := req.Metadata["continuationId"].(string); ok && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
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
	return map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	}
}

// kiroChunkedWriteToolNames maps tool names (lowercase) to their description suffixes.
var kiroChunkedWriteToolNames = map[string]string{
	"write":         kiroWriteToolSuffix,
	"write_to_file": kiroWriteToolSuffix,
	"fswrite":       kiroWriteToolSuffix,
	"edit":          kiroEditToolSuffix,
	"apply_diff":    kiroEditToolSuffix,
}

func extractToolsStruct(irTools []ir.ToolDefinition) []ToolSpecification {
	if len(irTools) == 0 {
		return nil
	}
	tools := make([]ToolSpecification, 0, len(irTools))
	for _, t := range irTools {
		// Use enhanced schema cleaning for better compatibility
		// This handles $ref resolution, allOf merging, and removes unsupported keywords
		cleanedSchema := ir.CleanJsonSchemaEnhanced(ir.CopyMap(t.Parameters))
		finalSchema := ensureKiroInputSchema(cleanedSchema)

		name := shortenToolNameIfNeeded(t.Name)
		description := t.Description

		// Rename web_search → remote_web_search for Kiro API compatibility
		if ir.IsNetworkingToolName(name) {
			name = "remote_web_search"
			if description == "" {
				description = remoteWebSearchDescription
			}
		}

		// Append chunked write/edit instructions to tool descriptions (kiro.rs approach).
		// This prevents output truncation on large file operations.
		if suffix, ok := kiroChunkedWriteToolNames[strings.ToLower(name)]; ok {
			description += suffix
			// Kiro API limits tool descriptions to 10000 chars
			if len([]rune(description)) > 10000 {
				runes := []rune(description)
				description = string(runes[:10000])
			}
		}

		tools = append(tools, ToolSpecification{
			ToolSpecification: ToolSpecDetails{
				Name:        name,
				Description: description,
				InputSchema: ToolInputSchema{Json: finalSchema},
			},
		})
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

func hasThinkingConfigTags(prompt string) bool {
	return strings.Contains(prompt, "<thinking_mode>") ||
		strings.Contains(prompt, "<max_thinking_length>") ||
		strings.Contains(prompt, "<thinking_effort>")
}

func buildKiroThinkingHint(req *ir.UnifiedChatRequest) string {
	if req == nil || req.Thinking == nil {
		return ""
	}
	thinking := req.Thinking

	if strings.EqualFold(strings.TrimSpace(thinking.Effort), "none") {
		return ""
	}

	if !thinking.IncludeThoughts && thinking.Budget == 0 {
		return ""
	}

	budget := normalizeKiroThinkingBudget(thinking.Budget)
	return `<thinking_mode>enabled</thinking_mode>
<max_thinking_length>` + budget + `</max_thinking_length>`
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

const kiroMaxHistoryMessages = 999

func truncateHistoryIfNeeded(history []HistoryMessage) []HistoryMessage {
	if len(history) <= kiroMaxHistoryMessages {
		return sanitizeHistoryToolReferences(history)
	}
	truncated := history[len(history)-kiroMaxHistoryMessages:]
	return sanitizeHistoryToolReferences(truncated)
}

func sanitizeHistoryToolReferences(history []HistoryMessage) []HistoryMessage {
	if len(history) == 0 {
		return history
	}

	validToolUseIDs := collectHistoryToolUseIDs(history)
	for i := range history {
		if history[i].UserInputMessage == nil || history[i].UserInputMessage.UserInputMessageContext == nil {
			continue
		}

		ctx := history[i].UserInputMessage.UserInputMessageContext
		if len(ctx.ToolResults) == 0 {
			continue
		}

		filtered := filterToolResultsByKnownToolUseIDs(ctx.ToolResults, validToolUseIDs)
		if len(filtered) == 0 {
			ctx.ToolResults = nil
			if len(ctx.Tools) == 0 {
				history[i].UserInputMessage.UserInputMessageContext = nil
			}
			continue
		}
		ctx.ToolResults = filtered
	}

	return history
}

func sanitizeCurrentToolResults(currentMsg *CurrentMessage, history []HistoryMessage) {
	if currentMsg == nil || currentMsg.UserInputMessage.UserInputMessageContext == nil {
		return
	}

	ctx := currentMsg.UserInputMessage.UserInputMessageContext
	if len(ctx.ToolResults) == 0 {
		return
	}

	validToolUseIDs := collectHistoryToolUseIDs(history)
	historyToolResultIDs := collectHistoryToolResultIDs(history)
	filtered := filterCurrentToolResults(ctx.ToolResults, validToolUseIDs, historyToolResultIDs)
	if len(filtered) == 0 {
		ctx.ToolResults = nil
		if len(ctx.Tools) == 0 {
			currentMsg.UserInputMessage.UserInputMessageContext = nil
		}
		return
	}
	ctx.ToolResults = filtered
}

func collectHistoryToolUseIDs(history []HistoryMessage) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, msg := range history {
		if msg.AssistantResponseMessage == nil || len(msg.AssistantResponseMessage.ToolUses) == 0 {
			continue
		}
		for _, tu := range msg.AssistantResponseMessage.ToolUses {
			id := strings.TrimSpace(tu.ToolUseId)
			if id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

func collectHistoryToolResultIDs(history []HistoryMessage) map[string]struct{} {
	ids := make(map[string]struct{})
	for _, msg := range history {
		if msg.UserInputMessage == nil || msg.UserInputMessage.UserInputMessageContext == nil {
			continue
		}
		for _, tr := range msg.UserInputMessage.UserInputMessageContext.ToolResults {
			id := strings.TrimSpace(tr.ToolUseId)
			if id != "" {
				ids[id] = struct{}{}
			}
		}
	}
	return ids
}

func filterToolResultsByKnownToolUseIDs(toolResults []ToolResult, validToolUseIDs map[string]struct{}) []ToolResult {
	if len(toolResults) == 0 {
		return toolResults
	}

	filtered := make([]ToolResult, 0, len(toolResults))
	seen := make(map[string]struct{}, len(toolResults))
	for _, tr := range toolResults {
		id := strings.TrimSpace(tr.ToolUseId)
		if id == "" {
			continue
		}
		if _, ok := validToolUseIDs[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		filtered = append(filtered, tr)
	}
	return filtered
}

func filterCurrentToolResults(toolResults []ToolResult, validToolUseIDs, historyToolResultIDs map[string]struct{}) []ToolResult {
	if len(toolResults) == 0 {
		return toolResults
	}

	unpairedToolUseIDs := make(map[string]struct{}, len(validToolUseIDs))
	for id := range validToolUseIDs {
		if _, paired := historyToolResultIDs[id]; !paired {
			unpairedToolUseIDs[id] = struct{}{}
		}
	}

	filtered := make([]ToolResult, 0, len(toolResults))
	seenCurrent := make(map[string]struct{}, len(toolResults))
	for _, tr := range toolResults {
		id := strings.TrimSpace(tr.ToolUseId)
		if id == "" {
			continue
		}
		if _, dup := seenCurrent[id]; dup {
			continue
		}
		if _, ok := unpairedToolUseIDs[id]; !ok {
			continue
		}
		seenCurrent[id] = struct{}{}
		delete(unpairedToolUseIDs, id)
		filtered = append(filtered, tr)
	}
	return filtered
}

// removeOrphanedToolUses performs bidirectional tool_use/tool_result pairing validation.
// It collects all tool_result IDs from both history and current message, then removes
// any tool_use entries from assistant messages in history that have no matching tool_result.
// This prevents Kiro API errors when compaction or truncation drops tool_result messages.
func removeOrphanedToolUses(history *[]HistoryMessage, currentMsg *CurrentMessage) {
	if history == nil || len(*history) == 0 {
		return
	}

	// Collect all tool_result IDs from history user messages
	allToolResultIDs := make(map[string]struct{})
	for _, msg := range *history {
		if msg.UserInputMessage == nil || msg.UserInputMessage.UserInputMessageContext == nil {
			continue
		}
		for _, tr := range msg.UserInputMessage.UserInputMessageContext.ToolResults {
			id := strings.TrimSpace(tr.ToolUseId)
			if id != "" {
				allToolResultIDs[id] = struct{}{}
			}
		}
	}

	// Collect tool_result IDs from current message
	if currentMsg != nil && currentMsg.UserInputMessage.UserInputMessageContext != nil {
		for _, tr := range currentMsg.UserInputMessage.UserInputMessageContext.ToolResults {
			id := strings.TrimSpace(tr.ToolUseId)
			if id != "" {
				allToolResultIDs[id] = struct{}{}
			}
		}
	}

	// Remove orphaned tool_uses from assistant messages
	for i := range *history {
		msg := &(*history)[i]
		if msg.AssistantResponseMessage == nil || len(msg.AssistantResponseMessage.ToolUses) == 0 {
			continue
		}
		filtered := make([]ToolUse, 0, len(msg.AssistantResponseMessage.ToolUses))
		for _, tu := range msg.AssistantResponseMessage.ToolUses {
			id := strings.TrimSpace(tu.ToolUseId)
			if _, paired := allToolResultIDs[id]; paired {
				filtered = append(filtered, tu)
			}
		}
		// Only update if we actually removed something.
		// Keep nil (not empty slice) when all tool_uses were orphaned,
		// so Kiro API doesn't receive an empty toolUses array.
		if len(filtered) < len(msg.AssistantResponseMessage.ToolUses) {
			if len(filtered) == 0 {
				msg.AssistantResponseMessage.ToolUses = nil
			} else {
				msg.AssistantResponseMessage.ToolUses = filtered
			}
		}
	}
}

// ensurePlaceholderTools creates placeholder tool definitions for any tool names
// referenced in history assistant messages (via tool_use) but missing from the
// current tools list. This prevents Kiro API errors when clients change their
// tool set between requests (e.g., after compaction or plugin changes).
func ensurePlaceholderTools(tools []ToolSpecification, history []HistoryMessage) []ToolSpecification {
	if len(history) == 0 {
		return tools
	}

	// Collect existing tool names (case-insensitive)
	existingNames := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		existingNames[strings.ToLower(t.ToolSpecification.Name)] = struct{}{}
	}

	// Collect tool names from history assistant messages
	needed := make(map[string]struct{})
	for _, msg := range history {
		if msg.AssistantResponseMessage == nil {
			continue
		}
		for _, tu := range msg.AssistantResponseMessage.ToolUses {
			name := strings.TrimSpace(tu.Name)
			if name == "" {
				continue
			}
			if _, exists := existingNames[strings.ToLower(name)]; !exists {
				needed[name] = struct{}{}
			}
		}
	}

	if len(needed) == 0 {
		return tools
	}

	// Create placeholder definitions for missing tools
	for name := range needed {
		tools = append(tools, ToolSpecification{
			ToolSpecification: ToolSpecDetails{
				Name:        name,
				Description: "Tool used in conversation history.",
				InputSchema: ToolInputSchema{Json: map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				}},
			},
		})
	}

	return tools
}

func processMessagesStruct(messages []ir.Message, tools []ToolSpecification, modelID, origin string) ([]HistoryMessage, CurrentMessage) {
	nonSystem := filterSystemMessages(messages)
	nonSystem = mergeConsecutiveMessages(nonSystem)
	nonSystem = removePrefill(nonSystem)

	// FIX: Kiro API requires history to start with a user message.
	// Some clients (e.g., OpenClaw) send conversations starting with an assistant message,
	// which causes "Improperly formed request" on Kiro.
	// Prepend a placeholder user message so the history alternation is correct.
	if len(nonSystem) > 0 && nonSystem[0].Role == ir.RoleAssistant {
		placeholder := ir.Message{
			Role:    ir.RoleUser,
			Content: []ir.ContentPart{{Type: ir.ContentTypeText, Text: kiroUserPlaceholderEmpty}},
		}
		nonSystem = append([]ir.Message{placeholder}, nonSystem...)
	}

	if len(nonSystem) == 0 {
		// Fallback for empty conversation
		return []HistoryMessage{}, CurrentMessage{
			UserInputMessage: UserInputMessage{
				Content: kiroUserPlaceholderEmpty,
				ModelId: modelID,
				Origin:  origin,
			},
		}
	}

	history := make([]HistoryMessage, 0, len(nonSystem))
	var pendingToolResults []ToolResult
	var currentMsg UserInputMessage
	hasCurrent := false

	for i, msg := range nonSystem {
		isLast := i == len(nonSystem)-1

		switch msg.Role {
		case ir.RoleUser:
			userMsg := buildUserMessageStruct(msg, tools, modelID, origin, isLast)
			if len(pendingToolResults) > 0 {
				mergePendingToolResultsIntoUser(userMsg, pendingToolResults)
				pendingToolResults = nil
			}
			if isLast {
				currentMsg = *userMsg
				hasCurrent = true
			} else {
				history = append(history, HistoryMessage{UserInputMessage: userMsg})
			}

		case ir.RoleAssistant:
			if len(pendingToolResults) > 0 {
				history = append(history, HistoryMessage{UserInputMessage: syntheticToolResultHistoryMessage(pendingToolResults, modelID, origin)})
				pendingToolResults = nil
			}

			assistantMsg := buildAssistantMessageStruct(msg)
			history = append(history, HistoryMessage{AssistantResponseMessage: assistantMsg})
			if isLast {
				currentMsg = UserInputMessage{
					Content: kiroUserPlaceholderEmpty,
					ModelId: modelID,
					Origin:  origin,
				}
				hasCurrent = true
			}

		case ir.RoleTool:
			pendingToolResults = append(pendingToolResults, collectToolResults(msg)...)
			if isLast {
				currentMsg = UserInputMessage{
					Content: kiroUserPlaceholderToolResults,
					ModelId: modelID,
					Origin:  origin,
					UserInputMessageContext: &UserInputMessageContext{
						ToolResults: append([]ToolResult(nil), pendingToolResults...),
						Tools:       tools,
					},
				}
				hasCurrent = true
			}
		}
	}

	if !hasCurrent {
		if len(pendingToolResults) > 0 {
			currentMsg = UserInputMessage{
				Content:                 kiroUserPlaceholderToolResults,
				ModelId:                 modelID,
				Origin:                  origin,
				UserInputMessageContext: &UserInputMessageContext{ToolResults: pendingToolResults},
			}
		} else {
			currentMsg = UserInputMessage{
				Content: kiroUserPlaceholderEmpty,
				ModelId: modelID,
				Origin:  origin,
			}
		}
	}

	history = truncateHistoryIfNeeded(history)
	wrappedCurrent := CurrentMessage{UserInputMessage: currentMsg}
	ensureHistoryEndsWithAssistant(&history)
	sanitizeCurrentToolResults(&wrappedCurrent, history)
	return history, wrappedCurrent
}

func ensureHistoryEndsWithAssistant(history *[]HistoryMessage) {
	if history == nil || len(*history) == 0 {
		return
	}
	last := (*history)[len(*history)-1]
	if last.AssistantResponseMessage != nil {
		return
	}
	*history = append(*history, HistoryMessage{
		AssistantResponseMessage: &AssistantResponseMessage{Content: kiroAssistantPlaceholder},
	})
}

func collectToolResults(msg ir.Message) []ToolResult {
	var toolResults []ToolResult
	for _, part := range msg.Content {
		if part.Type == ir.ContentTypeToolResult && part.ToolResult != nil {
			toolResults = append(toolResults, buildToolResultStruct(part.ToolResult))
		}
	}
	return toolResults
}

func mergePendingToolResultsIntoUser(userMsg *UserInputMessage, pending []ToolResult) {
	if userMsg == nil || len(pending) == 0 {
		return
	}
	if userMsg.UserInputMessageContext == nil {
		userMsg.UserInputMessageContext = &UserInputMessageContext{}
	}
	userMsg.UserInputMessageContext.ToolResults = append(pending, userMsg.UserInputMessageContext.ToolResults...)
	if strings.TrimSpace(userMsg.Content) == "" {
		userMsg.Content = kiroUserPlaceholderToolResults
	}
}

func syntheticToolResultHistoryMessage(toolResults []ToolResult, modelID, origin string) *UserInputMessage {
	if len(toolResults) == 0 {
		return nil
	}
	return &UserInputMessage{
		Content: kiroUserPlaceholderToolResults,
		ModelId: modelID,
		Origin:  origin,
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: toolResults,
		},
	}
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

	// CRITICAL: Kiro API requires content to be non-empty for ALL user messages.
	// This includes both history messages and the current message.
	// When user message contains only tool_result (no text), content will be empty.
	// This commonly happens in compaction requests from OpenCode.
	if strings.TrimSpace(content) == "" {
		if len(toolResults) > 0 {
			content = kiroUserPlaceholderToolResults
		} else {
			content = kiroUserPlaceholderEmpty
		}
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
		name := tc.Name
		// Rename web_search → remote_web_search to match convertClaudeToolsToKiro
		if ir.IsNetworkingToolName(name) {
			name = "remote_web_search"
		}
		toolUses = append(toolUses, ToolUse{
			ToolUseId: reverseConvertToolID(tc.ID),
			Name:      name,
			Input:     ir.ParseToolCallArgs(tc.Args),
		})
	}

	content := strings.TrimSpace(ir.CombineTextParts(msg))
	if content == "" {
		// Kiro API requires non-empty assistant content.
		// This happens in compaction/tool-only assistant turns.
		// Use " " (space) for tool-use-only messages (matches kiro.rs:793),
		// "." for messages with no content at all.
		if len(toolUses) > 0 {
			content = " "
		} else {
			content = "."
		}
	}

	return &AssistantResponseMessage{
		Content:  content,
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
		Content: kiroUserPlaceholderToolResults,
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

	content := kiroUserPlaceholderToolResults
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
		ToolUseId: reverseConvertToolID(tr.ToolCallID),
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
// Kiro expects the trailing assistant turn to remain in history so a synthetic
// current user message can continue the conversation. Do not trim it here.
func removePrefill(messages []ir.Message) []ir.Message {
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
			// Keep tool messages isolated. Legacy Kiro translators do not merge them
			// because each tool turn carries distinct tool_call/tool_result pairing
			// semantics, and collapsing them can distort multi-step agent flows.
			if last.Role == msg.Role && msg.Role != ir.RoleUser && msg.Role != ir.RoleTool {
				last.Content = append(last.Content, msg.Content...)
				// Preserve tool_calls when merging adjacent assistant messages.
				// Without this, tool_calls from the second message are lost,
				// causing orphaned tool_result references downstream.
				if msg.Role == ir.RoleAssistant && len(msg.ToolCalls) > 0 {
					last.ToolCalls = append(last.ToolCalls, msg.ToolCalls...)
				}
				continue
			}
		}
		merged = append(merged, msg)
	}
	return merged
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
