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

const (
	kiroThinkingStartTag           = "<thinking>"
	kiroThinkingEndTag             = "</thinking>"
	defaultKiroContextWindowTokens = 200000
)

// ParseKiroResponse converts a non-streaming Kiro API response to unified format.
func ParseKiroResponse(rawJSON []byte) ([]ir.Message, *ir.Usage, error) {
	if !gjson.ValidBytes(rawJSON) {
		return nil, nil, &json.UnmarshalTypeError{Value: "invalid json"}
	}
	parsed := gjson.ParseBytes(rawJSON)

	var resp gjson.Result
	if r := parsed.Get("conversationState.currentMessage.assistantResponseMessage"); r.Exists() {
		resp = r
	} else if r := parsed.Get("assistantResponseMessage"); r.Exists() {
		resp = r
	} else {
		return nil, nil, nil
	}

	msg := &ir.Message{Role: ir.RoleAssistant}
	metadata := make(map[string]any)
	if cid := strings.TrimSpace(parsed.Get("conversationState.conversationId").String()); cid != "" {
		metadata["conversationId"] = cid
	}
	if contID := strings.TrimSpace(parsed.Get("conversationState.agentContinuationId").String()); contID != "" {
		metadata["continuationId"] = contID
	}
	if content := resp.Get("content").String(); content != "" {
		cleanContent, thinkingContent := extractThinkingFromContent(content)
		if thinkingContent != "" {
			msg.Content = append(msg.Content, ir.ContentPart{Type: ir.ContentTypeReasoning, Reasoning: thinkingContent})
		}
		if cleanContent != "" {
			msg.Content = append(msg.Content, ir.ContentPart{Type: ir.ContentTypeText, Text: cleanContent})
		}
	}

	for _, tool := range resp.Get("toolUsages").Array() {
		toolID := convertToolID(tool.Get("toolUseId").String())
		if hasToolCallWithID(msg.ToolCalls, toolID) {
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, ir.ToolCall{ID: toolID, Name: tool.Get("name").String(), Args: tool.Get("input").String()})
	}
	for _, tool := range resp.Get("toolUses").Array() {
		toolID := convertToolID(tool.Get("toolUseId").String())
		if hasToolCallWithID(msg.ToolCalls, toolID) {
			continue
		}
		msg.ToolCalls = append(msg.ToolCalls, ir.ToolCall{ID: toolID, Name: tool.Get("name").String(), Args: tool.Get("input").String()})
	}
	if len(msg.ToolCalls) > 0 {
		metadata["continuation_mode"] = "requires_tool_result"
	}
	if len(metadata) > 0 {
		msg.Metadata = metadata
	}

	if len(msg.Content) == 0 && len(msg.ToolCalls) == 0 {
		return nil, nil, nil
	}
	return []ir.Message{*msg}, nil, nil
}

func extractThinkingFromContent(content string) (string, string) {
	if !strings.Contains(content, kiroThinkingStartTag) {
		return content, ""
	}
	var cleanContent strings.Builder
	var thinkingContent strings.Builder
	remaining := content
	for {
		before, after, found := strings.Cut(remaining, kiroThinkingStartTag)
		cleanContent.WriteString(before)
		if !found {
			break
		}
		thinkingPart, rest, endFound := strings.Cut(after, kiroThinkingEndTag)
		thinkingContent.WriteString(thinkingPart)
		if !endFound {
			break
		}
		remaining = rest
	}
	return strings.TrimSpace(cleanContent.String()), strings.TrimSpace(thinkingContent.String())
}

func convertToolID(id string) string {
	if strings.HasPrefix(id, "tooluse_") {
		return strings.Replace(id, "tooluse_", "call_", 1)
	}
	return id
}

func hasToolCallWithID(toolCalls []ir.ToolCall, id string) bool {
	for _, tc := range toolCalls {
		if tc.ID == id {
			return true
		}
	}
	return false
}

// KiroStreamState tracks state for Kiro streaming response parsing.
type KiroStreamState struct {
	Usage                     *ir.Usage
	CurrentTool               *ir.ToolCall
	CurrentToolIndex          int
	AccumulatedContent        string
	CurrentToolInput          string
	ToolCalls                 []ir.ToolCall
	SeenToolCallIDs           map[string]struct{}
	SawToolLifecycleEvent     bool
	CurrentToolObservedStop   bool
	InThinkingBlock           bool
	PendingContent            string
	AccumulatedThinking       string
	ContentPhaseStarted       bool
	HasSubstantiveOutput      bool
	UpstreamContextPercentage float64
	ContextWindowTokens       int
	CompletionObserved        bool
	CompletionInferred        bool
	Interrupted               bool
	TransportError            bool
	ObservedStopSignal        string
	InferredFinishReason      ir.FinishReason
}

func NewKiroStreamState() *KiroStreamState {
	return &KiroStreamState{
		ToolCalls:           make([]ir.ToolCall, 0),
		SeenToolCallIDs:     make(map[string]struct{}),
		InThinkingBlock:     false,
		ContextWindowTokens: defaultKiroContextWindowTokens,
		CurrentToolIndex:    -1,
	}
}

// SetContextWindowTokens sets the model context window for context usage conversion.
func (s *KiroStreamState) SetContextWindowTokens(tokens int) {
	if tokens > 0 {
		s.ContextWindowTokens = tokens
	}
}

// ProcessChunk processes a Kiro stream chunk and returns events.
func (s *KiroStreamState) ProcessChunk(rawJSON []byte) ([]ir.UnifiedEvent, error) {
	if len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
		return nil, nil
	}
	parsed := gjson.ParseBytes(rawJSON)
	s.parseUsage(parsed)
	s.parseContextUsage(parsed)

	var events []ir.UnifiedEvent
	if reasoningEvents := s.processReasoningEvent(parsed); len(reasoningEvents) > 0 {
		events = append(events, reasoningEvents...)
	}
	s.observeCompletionSignals(parsed)
	handledToolEvent := false
	if toolEvent := parsed.Get("toolUseEvent"); toolEvent.Exists() {
		events = append(events, s.processToolEvent(toolEvent)...)
		handledToolEvent = true
	}
	if !handledToolEvent && parsed.Get("toolUseId").Exists() && parsed.Get("name").Exists() {
		events = append(events, s.processToolEvent(parsed)...)
		handledToolEvent = true
	}
	if !handledToolEvent && s.isToolLifecycleContinuation(parsed) {
		events = append(events, s.processToolEvent(parsed)...)
	}
	events = append(events, s.processRegularEvents(parsed)...)
	return events, nil
}

func (s *KiroStreamState) isToolLifecycleContinuation(parsed gjson.Result) bool {
	if s.CurrentTool == nil {
		return false
	}
	if parsed.Get("input").Exists() {
		return true
	}
	if parsed.Get("stop").Exists() {
		return true
	}
	if event := parsed.Get("toolUseEvent"); event.Exists() {
		if event.Get("input").Exists() || event.Get("stop").Exists() {
			return true
		}
	}
	return false
}

func (s *KiroStreamState) observeCompletionSignals(parsed gjson.Result) {
	completionNodes := []gjson.Result{
		parsed.Get("completionEvent"),
		parsed.Get("assistantResponseEvent.completionEvent"),
		parsed.Get("chatResponseEvent.completionEvent"),
	}
	for _, node := range completionNodes {
		if !node.Exists() {
			continue
		}
		s.CompletionObserved = true
		s.CompletionInferred = false
		s.Interrupted = false
		if stop := strings.TrimSpace(node.Get("stopReason").String()); stop != "" {
			s.ObservedStopSignal = stop
			return
		}
		if stop := strings.TrimSpace(node.Get("stop_reason").String()); stop != "" {
			s.ObservedStopSignal = stop
			return
		}
		if stop := strings.TrimSpace(node.Get("type").String()); stop != "" {
			s.ObservedStopSignal = stop
			return
		}
		s.ObservedStopSignal = "completion_event"
		return
	}

	if stop := strings.TrimSpace(parsed.Get("stopReason").String()); stop != "" {
		s.CompletionObserved = true
		s.CompletionInferred = false
		s.Interrupted = false
		s.ObservedStopSignal = stop
		return
	}
	if stop := strings.TrimSpace(parsed.Get("stop_reason").String()); stop != "" {
		s.CompletionObserved = true
		s.CompletionInferred = false
		s.Interrupted = false
		s.ObservedStopSignal = stop
	}
}

func (s *KiroStreamState) parseUsage(parsed gjson.Result) {
	usageNode := parsed.Get("supplementaryWebLinksEvent")
	if !usageNode.Exists() && (parsed.Get("inputTokens").Exists() || parsed.Get("outputTokens").Exists()) {
		usageNode = parsed
	}
	if !usageNode.Exists() {
		if mm := parsed.Get("messageMetadataEvent.tokenUsage"); mm.Exists() {
			inTokens := mm.Get("uncachedInputTokens").Int() + mm.Get("cacheReadInputTokens").Int()
			outTokens := mm.Get("outputTokens").Int()
			if inTokens > 0 || outTokens > 0 {
				s.Usage = &ir.Usage{PromptTokens: int(inTokens), CompletionTokens: int(outTokens), TotalTokens: int(inTokens + outTokens)}
			}
		}
		return
	}
	inTokens := usageNode.Get("inputTokens").Int()
	outTokens := usageNode.Get("outputTokens").Int()
	if inTokens > 0 || outTokens > 0 {
		s.Usage = &ir.Usage{PromptTokens: int(inTokens), CompletionTokens: int(outTokens), TotalTokens: int(inTokens + outTokens)}
	}
}

func (s *KiroStreamState) parseContextUsage(parsed gjson.Result) {
	found := false
	if ctxUsage := parsed.Get("contextUsageEvent"); ctxUsage.Exists() {
		if pct := ctxUsage.Get("contextUsagePercentage"); pct.Exists() {
			s.UpstreamContextPercentage = pct.Float()
			found = true
		}
	}
	if !found {
		if pct := parsed.Get("contextUsagePercentage"); pct.Exists() {
			s.UpstreamContextPercentage = pct.Float()
			found = true
		}
	}
	if !found {
		if pct := parsed.Get("messageMetadataEvent.tokenUsage.contextUsagePercentage"); pct.Exists() {
			s.UpstreamContextPercentage = pct.Float()
			found = true
		}
	}
	if !found || s.UpstreamContextPercentage <= 0 {
		return
	}
	pct := s.UpstreamContextPercentage
	if pct <= 1.0 {
		pct *= 100.0
	}
	if pct > 100.0 {
		pct = 100.0
	}
	if s.Usage == nil {
		s.Usage = &ir.Usage{}
	}
	base := s.ContextWindowTokens
	if base <= 0 {
		base = defaultKiroContextWindowTokens
	}
	promptTokens := int(pct * float64(base) / 100.0)
	if promptTokens > s.Usage.PromptTokens {
		s.Usage.PromptTokens = promptTokens
		s.Usage.TotalTokens = s.Usage.PromptTokens + s.Usage.CompletionTokens
	}
}

func (s *KiroStreamState) processToolEvent(parsed gjson.Result) []ir.UnifiedEvent {
	id := convertToolID(parsed.Get("toolUseId").String())
	name := parsed.Get("name").String()
	if id == "" && s.CurrentTool != nil {
		id = s.CurrentTool.ID
	}
	if name == "" && s.CurrentTool != nil {
		name = s.CurrentTool.Name
	}
	if id == "" || name == "" {
		return nil
	}
	var events []ir.UnifiedEvent
	s.SawToolLifecycleEvent = true
	isNewTool := s.CurrentTool == nil || s.CurrentTool.ID != id
	if isNewTool {
		previousToolIndex := s.CurrentToolIndex
		if finalized := s.finalizeCurrentTool(false); finalized != nil {
			events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToolCallDelta, ToolCall: &ir.ToolCall{ID: finalized.ID, Name: finalized.Name, IsComplete: true}, ToolCallIndex: previousToolIndex})
		}
		s.CurrentTool = &ir.ToolCall{ID: id, Name: name}
		s.CurrentToolInput = ""
		s.CurrentToolIndex = len(s.ToolCalls)
		s.CurrentToolObservedStop = false
	}
	toolIndex := s.CurrentToolIndex
	inputNode := parsed.Get("input")
	inputDelta := inputNode.String()
	if inputNode.IsObject() {
		inputDelta = inputNode.Raw
	}
	s.CurrentToolInput += inputDelta
	if isNewTool {
		events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToolCall, ToolCall: &ir.ToolCall{ID: id, Name: name, Args: inputDelta}, ToolCallIndex: toolIndex})
	} else if inputDelta != "" {
		events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToolCallDelta, ToolCall: &ir.ToolCall{Args: inputDelta}, ToolCallIndex: toolIndex})
	}
	if parsed.Get("stop").Bool() {
		s.CurrentToolObservedStop = true
		if finalized := s.finalizeCurrentTool(true); finalized != nil {
			events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToolCallDelta, ToolCall: &ir.ToolCall{ID: finalized.ID, Name: finalized.Name, IsComplete: true}, ToolCallIndex: toolIndex})
		}
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
	if data.Raw != parsed.Raw {
		if reasoning := data.Get("reasoningContentEvent"); reasoning.Exists() {
			content := reasoning.Get("content").String()
			if content == "" {
				content = reasoning.Get("text").String()
			}
			signature := reasoning.Get("signature").String()
			if content != "" {
				s.AccumulatedThinking += content
				events = append(events, ir.UnifiedEvent{Type: ir.EventTypeReasoning, Reasoning: content, ThoughtSignature: signature})
			}
		}
	}
	if content := data.Get("content").String(); content != "" {
		if !(s.AccumulatedThinking != "" && !s.ContentPhaseStarted && strings.TrimSpace(content) == "") {
			if strings.TrimSpace(content) != "" {
				s.ContentPhaseStarted = true
			}
			s.HasSubstantiveOutput = true
			textEvents, thinkingEvents := s.processContentWithThinking(content)
			events = append(events, thinkingEvents...)
			events = append(events, textEvents...)
		}
	}
	for _, tool := range data.Get("toolUsages").Array() {
		tc := ir.ToolCall{ID: convertToolID(tool.Get("toolUseId").String()), Name: tool.Get("name").String(), Args: tool.Get("input").String()}
		if !s.hasToolCall(tc.ID) {
			s.ToolCalls = append(s.ToolCalls, tc)
			s.SeenToolCallIDs[tc.ID] = struct{}{}
			s.HasSubstantiveOutput = true
			events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToolCall, ToolCall: &tc})
		}
	}
	for _, tool := range data.Get("toolUses").Array() {
		tc := ir.ToolCall{ID: convertToolID(tool.Get("toolUseId").String()), Name: tool.Get("name").String(), Args: tool.Get("input").String()}
		if !s.hasToolCall(tc.ID) {
			s.ToolCalls = append(s.ToolCalls, tc)
			s.SeenToolCallIDs[tc.ID] = struct{}{}
			s.HasSubstantiveOutput = true
			events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToolCall, ToolCall: &tc})
		}
	}
	return events
}

func (s *KiroStreamState) processReasoningEvent(parsed gjson.Result) []ir.UnifiedEvent {
	var events []ir.UnifiedEvent
	if reasoning := parsed.Get("reasoningContentEvent"); reasoning.Exists() {
		content := reasoning.Get("content").String()
		if content == "" {
			content = reasoning.Get("text").String()
		}
		signature := reasoning.Get("signature").String()
		if content != "" {
			s.AccumulatedThinking += content
			events = append(events, ir.UnifiedEvent{Type: ir.EventTypeReasoning, Reasoning: content, ThoughtSignature: signature})
		}
		return events
	}
	if reasoning := parsed.Get("reasoningContent"); reasoning.Exists() {
		content := reasoning.String()
		if content != "" {
			s.AccumulatedThinking += content
			events = append(events, ir.UnifiedEvent{Type: ir.EventTypeReasoning, Reasoning: content})
		}
		return events
	}
	return nil
}

func (s *KiroStreamState) processContentWithThinking(content string) ([]ir.UnifiedEvent, []ir.UnifiedEvent) {
	var textEvents, thinkingEvents []ir.UnifiedEvent
	remaining := s.PendingContent + content
	s.PendingContent = ""
	for len(remaining) > 0 {
		if s.InThinkingBlock {
			endIdx := strings.Index(remaining, kiroThinkingEndTag)
			if endIdx >= 0 {
				thinkingText := remaining[:endIdx]
				if thinkingText != "" {
					s.AccumulatedThinking += thinkingText
					thinkingEvents = append(thinkingEvents, ir.UnifiedEvent{Type: ir.EventTypeReasoning, Reasoning: thinkingText})
				}
				s.InThinkingBlock = false
				remaining = remaining[endIdx+len(kiroThinkingEndTag):]
				continue
			}
			emitPart, pending := splitPotentialTagSuffix(remaining, kiroThinkingEndTag)
			if emitPart != "" {
				s.AccumulatedThinking += emitPart
				thinkingEvents = append(thinkingEvents, ir.UnifiedEvent{Type: ir.EventTypeReasoning, Reasoning: emitPart})
			}
			s.PendingContent = pending
			break
		}
		startIdx := strings.Index(remaining, kiroThinkingStartTag)
		if startIdx >= 0 {
			if startIdx > 0 {
				textEvents = append(textEvents, s.buildTextEvents(remaining[:startIdx])...)
			}
			s.InThinkingBlock = true
			remaining = remaining[startIdx+len(kiroThinkingStartTag):]
			continue
		}
		emitPart, pending := splitPotentialTagSuffix(remaining, kiroThinkingStartTag)
		if emitPart != "" {
			textEvents = append(textEvents, s.buildTextEvents(emitPart)...)
		}
		s.PendingContent = pending
		break
	}
	return textEvents, thinkingEvents
}

func (s *KiroStreamState) buildTextEvents(text string) []ir.UnifiedEvent {
	var events []ir.UnifiedEvent
	if text == "" {
		return events
	}
	// Keep bracket-style markers as plain text in the canonical Kiro path.
	// Reference clients sometimes speculate on [Called ...] text, but for this
	// transport we only promote tool calls from explicit lifecycle events that are
	// backed by raw Kiro traces.
	s.AccumulatedContent += text
	events = append(events, ir.UnifiedEvent{Type: ir.EventTypeToken, Content: text})
	return events
}

func splitPotentialTagSuffix(content, fullTag string) (emit, pending string) {
	maxSuffix := len(fullTag) - 1
	if maxSuffix <= 0 || len(content) == 0 {
		return content, ""
	}
	if len(content) < maxSuffix {
		maxSuffix = len(content)
	}
	for suffixLen := maxSuffix; suffixLen >= 1; suffixLen-- {
		prefix := fullTag[:suffixLen]
		if strings.HasSuffix(content, prefix) {
			return content[:len(content)-suffixLen], content[len(content)-suffixLen:]
		}
	}
	return content, ""
}

// FlushPendingContent flushes buffered partial-tag suffixes at stream end.
func (s *KiroStreamState) FlushPendingContent() []ir.UnifiedEvent {
	if s.PendingContent == "" {
		return nil
	}
	pending := s.PendingContent
	s.PendingContent = ""
	if s.InThinkingBlock {
		s.AccumulatedThinking += pending
		return []ir.UnifiedEvent{{Type: ir.EventTypeReasoning, Reasoning: pending}}
	}
	return s.buildTextEvents(pending)
}

func (s *KiroStreamState) FinalizeCurrentTool() *ir.ToolCall {
	if s.CurrentTool == nil {
		return nil
	}
	if !s.CurrentToolObservedStop {
		return nil
	}
	return s.finalizeCurrentTool(true)
}

func (s *KiroStreamState) finalizeCurrentTool(markSubstantive bool) *ir.ToolCall {
	if s.CurrentTool == nil {
		return nil
	}
	tool := s.CurrentTool
	tool.Args = s.CurrentToolInput
	if tool.Args == "" {
		tool.Args = "{}"
	}
	if !s.hasToolCall(tool.ID) {
		s.ToolCalls = append(s.ToolCalls, *tool)
		s.SeenToolCallIDs[tool.ID] = struct{}{}
	}
	if markSubstantive {
		s.HasSubstantiveOutput = true
	}
	s.CurrentTool = nil
	s.CurrentToolInput = ""
	s.CurrentToolIndex = -1
	s.CurrentToolObservedStop = false
	return tool
}

func (s *KiroStreamState) hasToolCall(id string) bool {
	if _, ok := s.SeenToolCallIDs[id]; ok {
		return true
	}
	for _, tc := range s.ToolCalls {
		if tc.ID == id {
			s.SeenToolCallIDs[id] = struct{}{}
			return true
		}
	}
	return false
}

func (s *KiroStreamState) hasEquivalentToolCall(candidate ir.ToolCall) bool {
	if candidate.ID != "" && s.hasToolCall(candidate.ID) {
		return true
	}
	candidateKey := toolCallSemanticKey(candidate)
	if candidateKey == "" {
		return false
	}
	for _, existing := range s.ToolCalls {
		if toolCallSemanticKey(existing) == candidateKey {
			return true
		}
	}
	if s.CurrentTool != nil && toolCallSemanticKey(*s.CurrentTool) == candidateKey {
		return true
	}
	return false
}

func toolCallSemanticKey(tc ir.ToolCall) string {
	name := strings.TrimSpace(tc.Name)
	args := normalizeToolCallArgs(tc.Args)
	if name == "" || args == "" {
		return ""
	}
	return name + "\x00" + args
}

func normalizeToolCallArgs(args string) string {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return ""
	}
	var parsed interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return trimmed
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return trimmed
	}
	return string(normalized)
}

func (s *KiroStreamState) DetermineFinishReason() ir.FinishReason {
	if s.CompletionObserved {
		s.CompletionInferred = false
		s.Interrupted = false
		return s.mapObservedFinishReason()
	}
	if s.InferredFinishReason != "" {
		return s.InferredFinishReason
	}
	if s.SawToolLifecycleEvent || len(s.ToolCalls) > 0 || s.CurrentTool != nil {
		s.CompletionInferred = true
		s.Interrupted = s.TransportError
		s.InferredFinishReason = ir.FinishReasonToolCalls
		return s.InferredFinishReason
	}
	s.CompletionInferred = true
	s.Interrupted = s.TransportError
	if s.AccumulatedThinking != "" && !s.HasSubstantiveOutput {
		s.InferredFinishReason = ir.FinishReasonLength
		return s.InferredFinishReason
	}
	s.InferredFinishReason = ir.FinishReasonStop
	return s.InferredFinishReason
}

func (s *KiroStreamState) mapObservedFinishReason() ir.FinishReason {
	signal := strings.ToLower(strings.TrimSpace(s.ObservedStopSignal))
	switch signal {
	case "", "completion_event", "stop", "end_turn", "end_turn_event", "completed", "finished":
		return ir.FinishReasonStop
	case "tool_use", "tool_uses", "tool_call", "tool_calls", "requires_action":
		return ir.FinishReasonToolCalls
	case "max_tokens", "max_token", "length":
		return ir.FinishReasonLength
	case "content_filter", "safety", "guardrail":
		return ir.FinishReasonContentFilter
	case "error":
		return ir.FinishReasonError
	default:
		if strings.Contains(signal, "tool") {
			return ir.FinishReasonToolCalls
		}
		if strings.Contains(signal, "length") || strings.Contains(signal, "max") {
			return ir.FinishReasonLength
		}
		return ir.FinishReasonStop
	}
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
		toolCalls = append(toolCalls, ir.ToolCall{ID: toolUseID, Name: toolName, Args: repairedJSON})
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
			switch char {
			case openChar:
				depth++
			case closeChar:
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

// MarkTransportError records that stream termination was caused by a local transport/parser error.
func (s *KiroStreamState) MarkTransportError() {
	if s == nil {
		return
	}
	s.TransportError = true
}
