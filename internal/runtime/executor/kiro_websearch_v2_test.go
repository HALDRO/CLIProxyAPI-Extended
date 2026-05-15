package executor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestHasWebSearchToolV2(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "single web_search name", body: `{"tools":[{"name":"web_search"}]}`, want: true},
		{name: "single web_search type variant", body: `{"tools":[{"type":"web_search_20250305"}]}`, want: true},
		{name: "case normalized", body: `{"tools":[{"name":"WEB_SEARCH"}]}`, want: true},
		{name: "multiple tools disabled", body: `{"tools":[{"name":"web_search"},{"name":"read"}]}`, want: false},
		{name: "non-web tool", body: `{"tools":[{"name":"read"}]}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasWebSearchToolV2([]byte(tt.body)); got != tt.want {
				t.Fatalf("hasWebSearchToolV2() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractSearchQueryV2(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "string content strips prefix", body: `{"messages":[{"content":"Perform a web search for the query: weather in shanghai"}]}`, want: "weather in shanghai"},
		{name: "array text block", body: `{"messages":[{"content":[{"type":"text","text":"latest kiro rs endpoint"}]}]}`, want: "latest kiro rs endpoint"},
		{name: "no text block", body: `{"messages":[{"content":[{"type":"image","source":"x"}]}]}`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractSearchQueryV2([]byte(tt.body)); got != tt.want {
				t.Fatalf("extractSearchQueryV2() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReplaceAndStripWebSearchToolV2(t *testing.T) {
	body := []byte(`{"tools":[{"name":"web_search","description":"old","type":"function"},{"name":"read","description":"keep"}]}`)
	replaced, err := replaceWebSearchToolDescriptionV2(body)
	if err != nil {
		t.Fatalf("replaceWebSearchToolDescriptionV2() error = %v", err)
	}
	var replacedParsed map[string]any
	if err := json.Unmarshal(replaced, &replacedParsed); err != nil {
		t.Fatalf("unmarshal replaced payload: %v", err)
	}
	tools := replacedParsed["tools"].([]any)
	first := tools[0].(map[string]any)
	if first["name"] != "web_search" {
		t.Fatalf("first tool name = %#v", first["name"])
	}
	if first["description"] == "old" {
		t.Fatalf("expected web_search description to be replaced, got %#v", first["description"])
	}
	second := tools[1].(map[string]any)
	if second["name"] != "read" {
		t.Fatalf("second tool name = %#v, want read", second["name"])
	}

	stripped, err := stripWebSearchToolV2(replaced)
	if err != nil {
		t.Fatalf("stripWebSearchToolV2() error = %v", err)
	}
	var strippedParsed map[string]any
	if err := json.Unmarshal(stripped, &strippedParsed); err != nil {
		t.Fatalf("unmarshal stripped payload: %v", err)
	}
	strippedTools := strippedParsed["tools"].([]any)
	if len(strippedTools) != 1 {
		t.Fatalf("stripped tools len = %d, want 1", len(strippedTools))
	}
	if strippedTools[0].(map[string]any)["name"] != "read" {
		t.Fatalf("remaining tool = %#v, want read", strippedTools[0])
	}

	onlySearch := []byte(`{"tools":[{"name":"web_search"}]}`)
	removed, err := stripWebSearchToolV2(onlySearch)
	if err != nil {
		t.Fatalf("stripWebSearchToolV2(only search) error = %v", err)
	}
	var removedParsed map[string]any
	if err := json.Unmarshal(removed, &removedParsed); err != nil {
		t.Fatalf("unmarshal removed payload: %v", err)
	}
	if _, exists := removedParsed["tools"]; exists {
		t.Fatalf("expected tools key to be removed entirely, got %#v", removedParsed["tools"])
	}
}

func TestInjectToolResultsClaudeV2(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"Perform a web search"}]}`)
	results := &kiroWebSearchResultsV2{Results: []kiroWebSearchResultV2{{Title: "Doc", URL: "https://example.com", Snippet: stringPtr("Result")}}}
	out, err := injectToolResultsClaudeV2(payload, "srvtoolu_1", "kiro endpoint trait", results)
	if err != nil {
		t.Fatalf("injectToolResultsClaudeV2() error = %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal claude output: %v", err)
	}
	messages := parsed["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(messages))
	}
	assistant := messages[1].(map[string]any)
	content := assistant["content"].([]any)
	toolUse := content[0].(map[string]any)
	if toolUse["name"] != "web_search" {
		t.Fatalf("tool_use name = %#v", toolUse["name"])
	}
	user := messages[2].(map[string]any)
	userContent := user["content"].([]any)
	if userContent[0].(map[string]any)["tool_use_id"] != "srvtoolu_1" {
		t.Fatalf("tool_result tool_use_id = %#v", userContent[0])
	}
	if text := userContent[1].(map[string]any)["text"].(string); text == "" || !containsInsensitive(text, "search_guidance") {
		t.Fatalf("expected search guidance text, got %q", text)
	}
}

func TestInjectSearchIndicatorsInResponseV2(t *testing.T) {
	response := []byte(`{"content":[{"type":"text","text":"final answer"}]}`)
	results := &kiroWebSearchResultsV2{Results: []kiroWebSearchResultV2{{Title: "Doc", URL: "https://example.com", Snippet: stringPtr("Snippet")}}}
	out, err := injectSearchIndicatorsInResponseV2(response, []kiroSearchIndicatorV2{{ToolUseID: "srvtoolu_1", Query: "kiro endpoint trait", Results: results}})
	if err != nil {
		t.Fatalf("injectSearchIndicatorsInResponseV2() error = %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	content := parsed["content"].([]any)
	if len(content) != 3 {
		t.Fatalf("content len = %d, want 3", len(content))
	}
	if content[0].(map[string]any)["type"] != "server_tool_use" {
		t.Fatalf("first content block = %#v, want server_tool_use", content[0])
	}
	if content[1].(map[string]any)["type"] != "web_search_tool_result" {
		t.Fatalf("second content block = %#v, want web_search_tool_result", content[1])
	}
	if content[2].(map[string]any)["type"] != "text" {
		t.Fatalf("third content block = %#v, want original text", content[2])
	}
}

func TestGenerateAndAdjustSearchIndicatorEventsV2(t *testing.T) {
	results := &kiroWebSearchResultsV2{Results: []kiroWebSearchResultV2{{Title: "Doc", URL: "https://example.com", Snippet: stringPtr("Snippet")}}}
	events := generateSearchIndicatorEventsV2("kiro endpoint trait", "srvtoolu_1", results, 4)
	if len(events) != 5 {
		t.Fatalf("indicator events len = %d, want 5", len(events))
	}
	sse := events[0].ToSSEString()
	if sse == "" || !containsInsensitive(sse, "content_block_start") {
		t.Fatalf("unexpected SSE serialization: %q", sse)
	}

	adjusted, shouldForward := adjustStreamIndicesV2([]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta"}}`), 3)
	if !shouldForward {
		t.Fatal("expected adjusted event to be forwarded")
	}
	var adjustedEvent map[string]any
	if err := json.Unmarshal(adjusted, &adjustedEvent); err != nil {
		t.Fatalf("unmarshal adjusted event: %v", err)
	}
	if adjustedEvent["index"].(float64) != 4 {
		t.Fatalf("adjusted index = %#v, want 4", adjustedEvent["index"])
	}

	if _, shouldForward := adjustStreamIndicesV2([]byte(`{"type":"message_start"}`), 2); shouldForward {
		t.Fatal("message_start should be filtered out")
	}

	chunk := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\"}}\n\n")
	adjustedChunk, ok := adjustSSEChunkV2(chunk, 2)
	if !ok {
		t.Fatal("expected adjusted chunk to be forwarded")
	}
	if !containsInsensitive(string(adjustedChunk), `"index":3`) {
		t.Fatalf("adjusted chunk = %s, want index 3", string(adjustedChunk))
	}
}

func TestAnalyzeBufferedStreamAndFilterChunksV2(t *testing.T) {
	chunks := [][]byte{
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"name\":\"web_search\",\"id\":\"srvtoolu_abc\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\\\"kiro endpoint trait\\\"}\"}}\n\n"),
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"}}\n\n"),
	}
	analysis := analyzeBufferedStreamV2(chunks)
	if !analysis.HasWebSearchToolUse {
		t.Fatal("expected web search tool use to be detected")
	}
	if analysis.WebSearchToolUseID != "srvtoolu_abc" {
		t.Fatalf("tool use id = %q, want srvtoolu_abc", analysis.WebSearchToolUseID)
	}
	if analysis.WebSearchQuery != "kiro endpoint trait" {
		t.Fatalf("query = %q, want kiro endpoint trait", analysis.WebSearchQuery)
	}
	if analysis.WebSearchToolUseIdx != 0 {
		t.Fatalf("tool use index = %d, want 0", analysis.WebSearchToolUseIdx)
	}
	if analysis.StopReason != "tool_use" {
		t.Fatalf("stop reason = %q, want tool_use", analysis.StopReason)
	}

	clientChunks := filterChunksForClientV2(chunks, 0, 2)
	if len(clientChunks) == 0 {
		t.Fatal("expected filtered chunks to remain")
	}
	joined := string(bytes.Join(clientChunks, []byte("\n")))
	if containsInsensitive(joined, `"index":0`) {
		t.Fatalf("expected tool index 0 events to be filtered, got %s", joined)
	}
	if containsInsensitive(joined, "message_delta") {
		t.Fatalf("expected message_delta to be filtered, got %s", joined)
	}
}

func TestAnalyzeOpenAIWebSearchResponseAndArgsV2(t *testing.T) {
	response := []byte(`{
		"choices":[{
			"message":{
				"tool_calls":[
					{"id":"srvtoolu_1","function":{"name":"remote_web_search","arguments":"{\"query\":\"latest kiro rs endpoint\"}"}}
				]
			},
			"finish_reason":"tool_calls"
		}]
	}`)
	analysis := analyzeOpenAIWebSearchResponseV2(response)
	if !analysis.HasWebSearchToolUse {
		t.Fatal("expected non-stream openai analysis to detect web search")
	}
	if analysis.WebSearchToolUseID != "srvtoolu_1" {
		t.Fatalf("tool use id = %q, want srvtoolu_1", analysis.WebSearchToolUseID)
	}
	if analysis.WebSearchQuery != "latest kiro rs endpoint" {
		t.Fatalf("query = %q, want latest kiro rs endpoint", analysis.WebSearchQuery)
	}
	if analysis.StopReason != "tool_calls" {
		t.Fatalf("stop reason = %q, want tool_calls", analysis.StopReason)
	}

	if got := extractQueryFromOpenAIArgsV2(`{"query":"  spaced query  "}`); got != "spaced query" {
		t.Fatalf("extractQueryFromOpenAIArgsV2() = %q, want spaced query", got)
	}
	if got := extractQueryFromOpenAIArgsV2(`not-json`); got != "" {
		t.Fatalf("extractQueryFromOpenAIArgsV2(invalid) = %q, want empty", got)
	}
}

func containsInsensitive(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func TestInjectToolResultsOpenAIV2_AppendsToolAndUserMessages(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"Perform a web search for the query: weather in shanghai"}],"tools":[{"type":"function","function":{"name":"web_search","description":"search","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}]}`)
	toolUseID := "srvtoolu_test123"
	query := "weather in shanghai"
	results := &kiroWebSearchResultsV2{Results: []kiroWebSearchResultV2{{Title: "Weather", URL: "https://example.com", Snippet: stringPtr("Sunny")}}}

	out, err := injectToolResultsOpenAIV2(payload, toolUseID, query, results)
	if err != nil {
		t.Fatalf("injectToolResultsOpenAIV2 error: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	messages, ok := parsed["messages"].([]interface{})
	if !ok {
		t.Fatalf("messages missing or wrong type: %#v", parsed["messages"])
	}
	if len(messages) != 4 {
		t.Fatalf("messages len = %d, want 4", len(messages))
	}

	assistant, ok := messages[1].(map[string]interface{})
	if !ok {
		t.Fatalf("assistant message wrong type: %#v", messages[1])
	}
	if assistant["role"] != "assistant" {
		t.Fatalf("assistant role = %#v", assistant["role"])
	}
	toolCalls, ok := assistant["tool_calls"].([]interface{})
	if !ok || len(toolCalls) != 1 {
		t.Fatalf("assistant tool_calls invalid: %#v", assistant["tool_calls"])
	}
	toolCall := toolCalls[0].(map[string]interface{})
	if toolCall["id"] != toolUseID {
		t.Fatalf("tool call id = %#v, want %q", toolCall["id"], toolUseID)
	}
	function := toolCall["function"].(map[string]interface{})
	if function["name"] != "web_search" {
		t.Fatalf("tool name = %#v", function["name"])
	}
	if function["arguments"] != `{"query":"weather in shanghai"}` {
		t.Fatalf("tool arguments = %#v", function["arguments"])
	}

	toolMsg, ok := messages[2].(map[string]interface{})
	if !ok {
		t.Fatalf("tool message wrong type: %#v", messages[2])
	}
	if toolMsg["role"] != "tool" {
		t.Fatalf("tool role = %#v", toolMsg["role"])
	}
	if toolMsg["tool_call_id"] != toolUseID {
		t.Fatalf("tool_call_id = %#v, want %q", toolMsg["tool_call_id"], toolUseID)
	}

	userMsg, ok := messages[3].(map[string]interface{})
	if !ok {
		t.Fatalf("user message wrong type: %#v", messages[3])
	}
	if userMsg["role"] != "user" {
		t.Fatalf("user role = %#v", userMsg["role"])
	}
	content, _ := userMsg["content"].(string)
	if content == "" {
		t.Fatal("expected search guidance content")
	}
}

func TestAnalyzeBufferedOpenAIStreamV2_DetectsWebSearchToolCall(t *testing.T) {
	t.Parallel()
	chunks := [][]byte{
		[]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"srvtoolu_123\",\"type\":\"function\",\"function\":{\"name\":\"web_search\",\"arguments\":\"\"}}]},\"finish_reason\":null}]}\n\n"),
		[]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"query\\\":\\\"weather in shanghai\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}

	analysis := analyzeBufferedOpenAIStreamV2(chunks)
	if !analysis.HasWebSearchToolUse {
		t.Fatal("expected web search tool use to be detected")
	}
	if analysis.WebSearchToolUseID != "srvtoolu_123" {
		t.Fatalf("tool use id = %q, want srvtoolu_123", analysis.WebSearchToolUseID)
	}
	if analysis.WebSearchQuery != "weather in shanghai" {
		t.Fatalf("query = %q, want weather in shanghai", analysis.WebSearchQuery)
	}
	if analysis.StopReason != "tool_calls" {
		t.Fatalf("stop reason = %q, want tool_calls", analysis.StopReason)
	}
	if analysis.WebSearchToolUseIdx != 0 {
		t.Fatalf("tool use idx = %d, want 0", analysis.WebSearchToolUseIdx)
	}
}

func stringPtr(value string) *string {
	return &value
}
