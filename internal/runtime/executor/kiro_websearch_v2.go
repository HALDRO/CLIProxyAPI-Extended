package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type kiroMcpRequestV2 struct {
	ID      string          `json:"id"`
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  kiroMcpParamsV2 `json:"params"`
}

type kiroMcpParamsV2 struct {
	Name      string             `json:"name"`
	Arguments kiroMcpArgumentsV2 `json:"arguments"`
}

type kiroMcpArgumentsV2 struct {
	Query string                  `json:"query"`
	Meta  *kiroMcpArgumentsMetaV2 `json:"_meta,omitempty"`
}

type kiroMcpArgumentsMetaV2 struct {
	IsValid        bool       `json:"_isValid"`
	ActivePath     []string   `json:"_activePath"`
	CompletedPaths [][]string `json:"_completedPaths"`
}

type kiroMcpResponseV2 struct {
	Error   *kiroMcpErrorV2  `json:"error,omitempty"`
	ID      string           `json:"id"`
	JSONRPC string           `json:"jsonrpc"`
	Result  *kiroMcpResultV2 `json:"result,omitempty"`
}

type kiroMcpErrorV2 struct {
	Code    *int    `json:"code,omitempty"`
	Message *string `json:"message,omitempty"`
}

type kiroMcpResultV2 struct {
	Content []kiroMcpContentV2 `json:"content"`
	IsError bool               `json:"isError"`
}

type kiroMcpContentV2 struct {
	ContentType string `json:"type"`
	Text        string `json:"text"`
}

type kiroWebSearchResultsV2 struct {
	Results      []kiroWebSearchResultV2 `json:"results"`
	TotalResults *int                    `json:"totalResults,omitempty"`
	Query        *string                 `json:"query,omitempty"`
	Error        *string                 `json:"error,omitempty"`
}

type kiroWebSearchResultV2 struct {
	Title                string  `json:"title"`
	URL                  string  `json:"url"`
	Snippet              *string `json:"snippet,omitempty"`
	PublishedDate        *int64  `json:"publishedDate,omitempty"`
	ID                   *string `json:"id,omitempty"`
	Domain               *string `json:"domain,omitempty"`
	MaxVerbatimWordLimit *int    `json:"maxVerbatimWordLimit,omitempty"`
	PublicDomain         *bool   `json:"publicDomain,omitempty"`
}

type kiroSearchIndicatorV2 struct {
	ToolUseID string
	Query     string
	Results   *kiroWebSearchResultsV2
}

type kiroSSEEventV2 struct {
	Event string
	Data  interface{}
}

func (e *kiroSSEEventV2) ToSSEString() string {
	dataBytes, _ := json.Marshal(e.Data)
	return "event: " + e.Event + "\ndata: " + string(dataBytes) + "\n\n"
}

type kiroBufferedStreamResultV2 struct {
	StopReason          string
	WebSearchQuery      string
	WebSearchToolUseID  string
	HasWebSearchToolUse bool
	WebSearchToolUseIdx int
}

type kiroWebSearchHandlerV2 struct {
	mcpEndpoint string
	httpClient  *http.Client
	authToken   string
	machineID   string
	authAttrs   map[string]string
}

const kiroMcpMaxRetriesV2 = 2

const kiroSearchGuidanceTemplateV2 = `<search_guidance>
Current date: %s (%s)

IMPORTANT: Evaluate the search results above carefully. If the results are:
- Mostly spam, SEO junk, or unrelated websites
- Missing actual information about the query topic
- Outdated or not matching the requested time frame

Then you MUST use the web_search tool again with a refined query. Try:
- Rephrasing in English for better coverage
- Using more specific keywords
- Adding date context

Do NOT apologize for bad results without first attempting a re-search.
</search_guidance>`

func isWebSearchToolV2(name, toolType string) bool {
	return name == "web_search" || strings.HasPrefix(toolType, "web_search") || toolType == "web_search_20250305"
}

func isWebSearchToolCallNameV2(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == "web_search" || name == "websearch" || name == "remote_web_search"
}

func buildSearchGuidanceV2(now time.Time) string {
	return fmt.Sprintf(kiroSearchGuidanceTemplateV2, now.Format("January 2, 2006"), now.Format("Monday"))
}

func buildWebSearchResultContentV2(results *kiroWebSearchResultsV2) []map[string]interface{} {
	searchContent := make([]map[string]interface{}, 0)
	if results == nil {
		return searchContent
	}
	for _, r := range results.Results {
		snippet := ""
		if r.Snippet != nil {
			snippet = *r.Snippet
		}
		searchContent = append(searchContent, map[string]interface{}{
			"type":              "web_search_result",
			"title":             r.Title,
			"url":               r.URL,
			"encrypted_content": snippet,
			"page_age":          nil,
		})
	}
	return searchContent
}

func rewriteWebSearchToolsV2(body []byte, rewrite func(name, toolType string, tool gjson.Result) (json.RawMessage, bool, error)) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil
	}
	updated := make([]json.RawMessage, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		name := strings.ToLower(tool.Get("name").String())
		toolType := strings.ToLower(tool.Get("type").String())
		rewritten, keep, err := rewrite(name, toolType, tool)
		if err != nil {
			return body, err
		}
		if keep {
			updated = append(updated, rewritten)
		}
	}
	if len(updated) == 0 {
		var payload map[string]interface{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return body, err
		}
		delete(payload, "tools")
		return json.Marshal(payload)
	}
	updatedJSON, err := json.Marshal(updated)
	if err != nil {
		return body, err
	}
	return sjson.SetRawBytes(body, "tools", updatedJSON)
}

func decodeEventJSONV2(payload string) (map[string]interface{}, bool) {
	var event map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return nil, false
	}
	return event, true
}

func isMessageLifecycleEventV2(eventType string) bool {
	switch eventType {
	case "message_start", "message_delta", "message_stop":
		return true
	default:
		return false
	}
}

func hasWebSearchToolV2(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return false
	}
	toolsArray := tools.Array()
	if len(toolsArray) != 1 {
		return false
	}
	tool := toolsArray[0]
	name := strings.ToLower(tool.Get("name").String())
	toolType := strings.ToLower(tool.Get("type").String())
	return isWebSearchToolV2(name, toolType)
}

func extractSearchQueryV2(body []byte) string {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return ""
	}
	firstMsg := messages.Array()[0]
	content := firstMsg.Get("content")
	var text string
	if content.IsArray() {
		for _, block := range content.Array() {
			if block.Get("type").String() == "text" {
				text = block.Get("text").String()
				break
			}
		}
	} else {
		text = content.String()
	}
	const prefix = "Perform a web search for the query: "
	if strings.HasPrefix(text, prefix) {
		text = text[len(prefix):]
	}
	return strings.TrimSpace(text)
}

func generateToolUseIDV2() string {
	return strings.ReplaceAll(uuid.New().String(), "-", "")[:22]
}

func generateMessageIDV2() string {
	return "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
}

func generateRandomID8V2() string {
	return strings.ToLower(strings.ReplaceAll(uuid.New().String(), "-", "")[:8])
}

func createMcpRequestV2(query string) (string, *kiroMcpRequestV2) {
	random22 := generateToolUseIDV2()
	timestamp := time.Now().UnixMilli()
	random8 := generateRandomID8V2()
	requestID := fmt.Sprintf("web_search_tooluse_%s_%d_%s", random22, timestamp, random8)
	toolUseID := "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:32]
	request := &kiroMcpRequestV2{
		ID:      requestID,
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: kiroMcpParamsV2{
			Name: "web_search",
			Arguments: kiroMcpArgumentsV2{
				Query: query,
				Meta: &kiroMcpArgumentsMetaV2{
					IsValid:        true,
					ActivePath:     []string{"query"},
					CompletedPaths: [][]string{{"query"}},
				},
			},
		},
	}
	return toolUseID, request
}

func buildMcpEndpointV2(region string) string {
	return fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)
}

func newWebSearchHandlerV2(mcpEndpoint, authToken string, httpClient *http.Client, machineID string, authAttrs map[string]string) *kiroWebSearchHandlerV2 {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &kiroWebSearchHandlerV2{
		mcpEndpoint: mcpEndpoint,
		httpClient:  httpClient,
		authToken:   authToken,
		machineID:   machineID,
		authAttrs:   authAttrs,
	}
}

func (h *kiroWebSearchHandlerV2) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	if h.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.authToken)
	}
	if h.machineID != "" {
		req.Header.Set("x-amzn-codewhisperer-fingerprint", h.machineID)
	}
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	for k, v := range h.authAttrs {
		if strings.HasPrefix(strings.ToLower(k), "x-") {
			req.Header.Set(k, v)
		}
	}
}

func (h *kiroWebSearchHandlerV2) callMcpAPI(request *kiroMcpRequestV2) (*kiroMcpResponseV2, error) {
	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MCP request: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt <= kiroMcpMaxRetriesV2; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			log.Warnf("kiro/websearch: MCP retry %d/%d after %v", attempt, kiroMcpMaxRetriesV2, backoff)
			time.Sleep(backoff)
		}
		req, err := http.NewRequest(http.MethodPost, h.mcpEndpoint, bytes.NewReader(requestBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP request: %w", err)
		}
		h.setHeaders(req)
		resp, err := h.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("MCP API request failed: %w", err)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read MCP response: %w", err)
			continue
		}
		if resp.StatusCode >= 502 && resp.StatusCode <= 504 {
			lastErr = fmt.Errorf("MCP API returned retryable status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("MCP API returned status %d: %s", resp.StatusCode, string(body))
		}
		var mcpResponse kiroMcpResponseV2
		if err := json.Unmarshal(body, &mcpResponse); err != nil {
			return nil, fmt.Errorf("failed to parse MCP response: %w", err)
		}
		if mcpResponse.Error != nil {
			code := -1
			if mcpResponse.Error.Code != nil {
				code = *mcpResponse.Error.Code
			}
			msg := "Unknown error"
			if mcpResponse.Error.Message != nil {
				msg = *mcpResponse.Error.Message
			}
			return nil, fmt.Errorf("MCP error %d: %s", code, msg)
		}
		return &mcpResponse, nil
	}
	return nil, lastErr
}

func parseSearchResultsV2(response *kiroMcpResponseV2) *kiroWebSearchResultsV2 {
	if response == nil || response.Result == nil || len(response.Result.Content) == 0 {
		return nil
	}
	content := response.Result.Content[0]
	if content.ContentType != "text" {
		return nil
	}
	var results kiroWebSearchResultsV2
	if err := json.Unmarshal([]byte(content.Text), &results); err != nil {
		log.Warnf("kiro/websearch: failed to parse search results: %v", err)
		return nil
	}
	return &results
}

func replaceWebSearchToolDescriptionV2(body []byte) ([]byte, error) {
	return rewriteWebSearchToolsV2(body, func(name, toolType string, tool gjson.Result) (json.RawMessage, bool, error) {
		if isWebSearchToolV2(name, toolType) {
			minimalTool := map[string]interface{}{
				"name":        "web_search",
				"description": "Search the web for information. Use this when the previous search results are insufficient or when you need additional information on a different aspect of the query. Provide a refined or different search query.",
				"input_schema": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"query": map[string]interface{}{
							"type":        "string",
							"description": "The search query to execute",
						},
					},
					"required":             []string{"query"},
					"additionalProperties": false,
				},
			}
			minimalJSON, err := json.Marshal(minimalTool)
			if err != nil {
				return nil, false, fmt.Errorf("failed to marshal minimal tool: %w", err)
			}
			return json.RawMessage(minimalJSON), true, nil
		}
		return json.RawMessage(tool.Raw), true, nil
	})
}

func stripWebSearchToolV2(body []byte) ([]byte, error) {
	return rewriteWebSearchToolsV2(body, func(name, toolType string, tool gjson.Result) (json.RawMessage, bool, error) {
		if isWebSearchToolV2(name, toolType) {
			return nil, false, nil
		}
		return json.RawMessage(tool.Raw), true, nil
	})
}

func formatSearchContextPromptV2(query string, results *kiroWebSearchResultsV2) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[Web Search Results for \"%s\"]\n", query))
	if results != nil && len(results.Results) > 0 {
		for i, r := range results.Results {
			sb.WriteString(fmt.Sprintf("%d. %s - %s\n", i+1, r.Title, r.URL))
			if r.Snippet != nil && *r.Snippet != "" {
				snippet := *r.Snippet
				if len(snippet) > 500 {
					snippet = snippet[:500] + "..."
				}
				sb.WriteString(fmt.Sprintf("   %s\n", snippet))
			}
		}
	} else {
		sb.WriteString("No results found.\n")
	}
	sb.WriteString("[End Web Search Results]")
	return sb.String()
}

func formatToolResultTextV2(results *kiroWebSearchResultsV2) string {
	if results == nil || len(results.Results) == 0 {
		return "No search results found."
	}
	text := fmt.Sprintf("Found %d search result(s):\n\n", len(results.Results))
	resultJSON, err := json.MarshalIndent(results.Results, "", "  ")
	if err != nil {
		return text + "Error formatting results."
	}
	return text + string(resultJSON)
}

func injectToolResultsClaudeV2(claudePayload []byte, toolUseID, query string, results *kiroWebSearchResultsV2) ([]byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(claudePayload, &payload); err != nil {
		return claudePayload, fmt.Errorf("failed to parse claude payload: %w", err)
	}
	messages, _ := payload["messages"].([]interface{})
	assistantMsg := map[string]interface{}{
		"role": "assistant",
		"content": []interface{}{map[string]interface{}{
			"type":  "tool_use",
			"id":    toolUseID,
			"name":  "web_search",
			"input": map[string]interface{}{"query": query},
		}},
	}
	messages = append(messages, assistantMsg)
	searchGuidance := buildSearchGuidanceV2(time.Now())
	userMsg := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     formatToolResultTextV2(results),
			},
			map[string]interface{}{
				"type": "text",
				"text": searchGuidance,
			},
		},
	}
	messages = append(messages, userMsg)
	payload["messages"] = messages
	result, err := json.Marshal(payload)
	if err != nil {
		return claudePayload, fmt.Errorf("failed to marshal updated payload: %w", err)
	}
	return result, nil
}

func injectToolResultsOpenAIV2(openAIPayload []byte, toolUseID, query string, results *kiroWebSearchResultsV2) ([]byte, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(openAIPayload, &payload); err != nil {
		return openAIPayload, fmt.Errorf("failed to parse openai payload: %w", err)
	}
	messages, _ := payload["messages"].([]interface{})
	assistantMsg := map[string]interface{}{
		"role":    "assistant",
		"content": "",
		"tool_calls": []interface{}{map[string]interface{}{
			"id":   toolUseID,
			"type": "function",
			"function": map[string]interface{}{
				"name":      "web_search",
				"arguments": fmt.Sprintf(`{"query":%q}`, query),
			},
		}},
	}
	messages = append(messages, assistantMsg)
	searchGuidance := buildSearchGuidanceV2(time.Now())
	toolMsg := map[string]interface{}{
		"role":         "tool",
		"tool_call_id": toolUseID,
		"content":      formatToolResultTextV2(results),
	}
	userMsg := map[string]interface{}{
		"role":    "user",
		"content": searchGuidance,
	}
	messages = append(messages, toolMsg, userMsg)
	payload["messages"] = messages
	result, err := json.Marshal(payload)
	if err != nil {
		return openAIPayload, fmt.Errorf("failed to marshal updated payload: %w", err)
	}
	return result, nil
}

func injectToolResultsV2(payload []byte, toolUseID, query string, results *kiroWebSearchResultsV2, sourceFormat string) ([]byte, error) {
	if sourceFormat == "claude" {
		return injectToolResultsClaudeV2(payload, toolUseID, query, results)
	}
	return injectToolResultsOpenAIV2(payload, toolUseID, query, results)
}

func injectSearchIndicatorsInResponseV2(responsePayload []byte, searches []kiroSearchIndicatorV2) ([]byte, error) {
	if len(searches) == 0 {
		return responsePayload, nil
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(responsePayload, &resp); err != nil {
		return responsePayload, fmt.Errorf("failed to parse response: %w", err)
	}
	existingContent, _ := resp["content"].([]interface{})
	newContent := make([]interface{}, 0, len(searches)*2+len(existingContent))
	for _, s := range searches {
		newContent = append(newContent, map[string]interface{}{
			"type":  "server_tool_use",
			"id":    s.ToolUseID,
			"name":  "web_search",
			"input": map[string]interface{}{"query": s.Query},
		})
		newContent = append(newContent, map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": s.ToolUseID,
			"content":     buildWebSearchResultContentV2(s.Results),
		})
	}
	newContent = append(newContent, existingContent...)
	resp["content"] = newContent
	result, err := json.Marshal(resp)
	if err != nil {
		return responsePayload, fmt.Errorf("failed to marshal response: %w", err)
	}
	return result, nil
}

func generateSearchIndicatorEventsV2(query, toolUseID string, searchResults *kiroWebSearchResultsV2, startIndex int) []kiroSSEEventV2 {
	inputJSON, _ := json.Marshal(map[string]string{"query": query})
	searchContent := buildWebSearchResultContentV2(searchResults)
	return []kiroSSEEventV2{
		{Event: "content_block_start", Data: map[string]interface{}{"type": "content_block_start", "index": startIndex, "content_block": map[string]interface{}{"id": toolUseID, "type": "server_tool_use", "name": "web_search", "input": map[string]interface{}{}}}},
		{Event: "content_block_delta", Data: map[string]interface{}{"type": "content_block_delta", "index": startIndex, "delta": map[string]interface{}{"type": "input_json_delta", "partial_json": string(inputJSON)}}},
		{Event: "content_block_stop", Data: map[string]interface{}{"type": "content_block_stop", "index": startIndex}},
		{Event: "content_block_start", Data: map[string]interface{}{"type": "content_block_start", "index": startIndex + 1, "content_block": map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": toolUseID, "content": searchContent}}},
		{Event: "content_block_stop", Data: map[string]interface{}{"type": "content_block_stop", "index": startIndex + 1}},
	}
}

func adjustStreamIndicesV2(data []byte, offset int) ([]byte, bool) {
	if len(data) == 0 {
		return data, true
	}
	var event map[string]interface{}
	if err := json.Unmarshal(data, &event); err != nil {
		return data, true
	}
	eventType, _ := event["type"].(string)
	if eventType == "message_start" {
		return data, false
	}
	switch eventType {
	case "content_block_start", "content_block_delta", "content_block_stop":
		if idx, ok := event["index"].(float64); ok {
			event["index"] = int(idx) + offset
			adjusted, err := json.Marshal(event)
			if err != nil {
				return data, true
			}
			return adjusted, true
		}
	}
	return data, true
}

func adjustSSEChunkV2(chunk []byte, offset int) ([]byte, bool) {
	chunkStr := string(chunk)
	if !strings.Contains(chunkStr, "data: ") {
		return chunk, true
	}
	var result strings.Builder
	hasContent := false
	lines := strings.Split(chunkStr, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "data: ") {
			dataPayload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if dataPayload == "[DONE]" {
				result.WriteString(line + "\n")
				hasContent = true
				continue
			}
			adjusted, shouldForward := adjustStreamIndicesV2([]byte(dataPayload), offset)
			if !shouldForward {
				continue
			}
			result.WriteString("data: " + string(adjusted) + "\n")
			hasContent = true
		} else if strings.HasPrefix(line, "event: ") {
			if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "data: ") {
				dataPayload := strings.TrimSpace(strings.TrimPrefix(lines[i+1], "data: "))
				if event, ok := decodeEventJSONV2(dataPayload); ok {
					if eventType, ok := event["type"].(string); ok && eventType == "message_start" {
						i++
						continue
					}
				}
			}
			result.WriteString(line + "\n")
			hasContent = true
		} else {
			result.WriteString(line + "\n")
			if strings.TrimSpace(line) != "" {
				hasContent = true
			}
		}
	}
	if !hasContent {
		return nil, false
	}
	return []byte(result.String()), true
}

func analyzeBufferedStreamV2(chunks [][]byte) kiroBufferedStreamResultV2 {
	result := kiroBufferedStreamResultV2{WebSearchToolUseIdx: -1}
	var currentToolName string
	currentToolIndex := -1
	var toolInputBuilder strings.Builder
	for _, chunk := range chunks {
		lines := strings.Split(string(chunk), "\n")
		for _, line := range lines {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			dataPayload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if dataPayload == "[DONE]" || dataPayload == "" {
				continue
			}
			var event map[string]interface{}
			if err := json.Unmarshal([]byte(dataPayload), &event); err != nil {
				continue
			}
			eventType, _ := event["type"].(string)
			switch eventType {
			case "message_delta":
				if delta, ok := event["delta"].(map[string]interface{}); ok {
					if sr, ok := delta["stop_reason"].(string); ok && sr != "" {
						result.StopReason = sr
					}
				}
			case "content_block_start":
				if cb, ok := event["content_block"].(map[string]interface{}); ok {
					if cbType, ok := cb["type"].(string); ok && cbType == "tool_use" {
						if name, ok := cb["name"].(string); ok {
							currentToolName = strings.ToLower(name)
							if idx, ok := event["index"].(float64); ok {
								currentToolIndex = int(idx)
							}
							if id, ok := cb["id"].(string); ok && isWebSearchToolCallNameV2(currentToolName) {
								result.WebSearchToolUseID = id
							}
							toolInputBuilder.Reset()
						}
					}
				}
			case "content_block_delta":
				if currentToolName != "" {
					if delta, ok := event["delta"].(map[string]interface{}); ok {
						if deltaType, ok := delta["type"].(string); ok && deltaType == "input_json_delta" {
							if partial, ok := delta["partial_json"].(string); ok {
								toolInputBuilder.WriteString(partial)
							}
						}
					}
				}
			case "content_block_stop":
				if isWebSearchToolCallNameV2(currentToolName) {
					result.HasWebSearchToolUse = true
					result.WebSearchToolUseIdx = currentToolIndex
					inputJSON := toolInputBuilder.String()
					var input map[string]string
					if err := json.Unmarshal([]byte(inputJSON), &input); err == nil {
						if q, ok := input["query"]; ok {
							result.WebSearchQuery = q
						}
					}
				}
				currentToolName = ""
				currentToolIndex = -1
				toolInputBuilder.Reset()
			}
		}
	}
	return result
}

func filterChunksForClientV2(chunks [][]byte, wsToolIndex int, indexOffset int) [][]byte {
	var filtered [][]byte
	for _, chunk := range chunks {
		lines := strings.Split(string(chunk), "\n")
		var resultBuilder strings.Builder
		hasContent := false
		for i := 0; i < len(lines); i++ {
			line := lines[i]
			if strings.HasPrefix(line, "data: ") {
				dataPayload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				if dataPayload == "[DONE]" {
					continue
				}
				event, ok := decodeEventJSONV2(dataPayload)
				if !ok {
					resultBuilder.WriteString(line + "\n")
					hasContent = true
					continue
				}
				eventType, _ := event["type"].(string)
				if isMessageLifecycleEventV2(eventType) {
					continue
				}
				if wsToolIndex >= 0 {
					if idx, ok := event["index"].(float64); ok && int(idx) == wsToolIndex {
						continue
					}
				}
				if indexOffset > 0 {
					switch eventType {
					case "content_block_start", "content_block_delta", "content_block_stop":
						if idx, ok := event["index"].(float64); ok {
							event["index"] = int(idx) + indexOffset
							adjusted, err := json.Marshal(event)
							if err == nil {
								resultBuilder.WriteString("data: " + string(adjusted) + "\n")
								hasContent = true
								continue
							}
						}
					}
				}
				resultBuilder.WriteString(line + "\n")
				hasContent = true
			} else if strings.HasPrefix(line, "event: ") {
				if i+1 < len(lines) && strings.HasPrefix(lines[i+1], "data: ") {
					nextData := strings.TrimSpace(strings.TrimPrefix(lines[i+1], "data: "))
					if nextEvent, ok := decodeEventJSONV2(nextData); ok {
						nextType, _ := nextEvent["type"].(string)
						if isMessageLifecycleEventV2(nextType) {
							i++
							continue
						}
						if wsToolIndex >= 0 {
							if idx, ok := nextEvent["index"].(float64); ok && int(idx) == wsToolIndex {
								i++
								continue
							}
						}
					}
				}
				resultBuilder.WriteString(line + "\n")
				hasContent = true
			} else {
				resultBuilder.WriteString(line + "\n")
				if strings.TrimSpace(line) != "" {
					hasContent = true
				}
			}
		}
		if hasContent {
			filtered = append(filtered, []byte(resultBuilder.String()))
		}
	}
	return filtered
}

func analyzeOpenAIWebSearchResponseV2(responsePayload []byte) kiroBufferedStreamResultV2 {
	result := kiroBufferedStreamResultV2{WebSearchToolUseIdx: -1}
	toolCalls := gjson.GetBytes(responsePayload, "choices.0.message.tool_calls")
	if !toolCalls.IsArray() {
		return result
	}
	for _, tc := range toolCalls.Array() {
		name := strings.ToLower(strings.TrimSpace(tc.Get("function.name").String()))
		if !isWebSearchToolCallNameV2(name) {
			continue
		}
		result.HasWebSearchToolUse = true
		result.WebSearchToolUseID = tc.Get("id").String()
		result.WebSearchQuery = extractQueryFromOpenAIArgsV2(tc.Get("function.arguments").String())
		break
	}
	if finishReason := strings.TrimSpace(gjson.GetBytes(responsePayload, "choices.0.finish_reason").String()); finishReason != "" {
		result.StopReason = finishReason
	}
	return result
}

func analyzeBufferedOpenAIStreamV2(chunks [][]byte) kiroBufferedStreamResultV2 {
	result := kiroBufferedStreamResultV2{WebSearchToolUseIdx: -1}
	type partialTool struct {
		id   string
		name string
		args strings.Builder
	}
	toolByIndex := map[int]*partialTool{}
	for _, chunk := range chunks {
		for _, line := range strings.Split(string(chunk), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			if payload == "" || payload == "[DONE]" || !gjson.Valid(payload) {
				continue
			}
			parsed := gjson.Parse(payload)
			for _, tc := range parsed.Get("choices.0.delta.tool_calls").Array() {
				idx := int(tc.Get("index").Int())
				pt := toolByIndex[idx]
				if pt == nil {
					pt = &partialTool{}
					toolByIndex[idx] = pt
				}
				if id := tc.Get("id").String(); id != "" {
					pt.id = id
				}
				if name := strings.ToLower(strings.TrimSpace(tc.Get("function.name").String())); name != "" {
					pt.name = name
				}
				if args := tc.Get("function.arguments").String(); args != "" {
					pt.args.WriteString(args)
				}
			}
			if finishReason := strings.TrimSpace(parsed.Get("choices.0.finish_reason").String()); finishReason != "" {
				result.StopReason = finishReason
			}
		}
	}
	for idx, pt := range toolByIndex {
		if pt == nil {
			continue
		}
		if !isWebSearchToolCallNameV2(pt.name) {
			continue
		}
		result.HasWebSearchToolUse = true
		result.WebSearchToolUseIdx = idx
		result.WebSearchToolUseID = pt.id
		result.WebSearchQuery = extractQueryFromOpenAIArgsV2(pt.args.String())
		break
	}
	return result
}

func extractQueryFromOpenAIArgsV2(args string) string {
	args = strings.TrimSpace(args)
	if args == "" {
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(args), &payload); err != nil {
		return ""
	}
	query, _ := payload["query"].(string)
	return strings.TrimSpace(query)
}
