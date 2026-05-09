package executor

import (
	"encoding/json"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func TestTranslateResponseNonStreamAuto_OpenAIResponseTargetReturnsResponsesAPI(t *testing.T) {
	resp := []byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)

	translated, err := TranslateResponseNonStreamAuto(nil, "openai-response", sdktranslator.FromString("openai-response"), resp, "gpt-5.4")
	if err != nil {
		t.Fatalf("TranslateResponseNonStreamAuto() error = %v", err)
	}
	text := string(translated)
	if !strings.Contains(text, `"object": "response"`) {
		t.Fatalf("expected Responses API object, got %s", text)
	}
	if strings.Contains(text, `"object": "chat.completion"`) {
		t.Fatalf("did not expect chat.completion payload, got %s", text)
	}
}

func TestTranslateResponseStreamAuto_OpenAIResponseTargetReturnsResponsesEvents(t *testing.T) {
	chunk := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}")

	translated, err := TranslateResponseStreamAuto(nil, "openai-response", sdktranslator.FromString("openai-response"), chunk, "gpt-5.4", "resp_test", &UnifiedStreamState{})
	if err != nil {
		t.Fatalf("TranslateResponseStreamAuto() error = %v", err)
	}
	joined := stringJoinBytes(translated)
	if !strings.Contains(joined, "event: response.created") {
		t.Fatalf("expected response.created event, got %s", joined)
	}
	if !strings.Contains(joined, "event: response.output_item.added") {
		t.Fatalf("expected response.output_item.added event, got %s", joined)
	}
	if strings.Contains(joined, `"object": "chat.completion.chunk"`) {
		t.Fatalf("did not expect chat.completion.chunk payload, got %s", joined)
	}
}

func TestTranslateResponseStreamAuto_OpenAIResponseTargetPersistsResponsesState(t *testing.T) {
	state := &UnifiedStreamState{}
	first := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"o\"}}]}")
	second := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"k\"}}]}")

	translatedFirst, err := TranslateResponseStreamAuto(nil, "openai-response", sdktranslator.FromString("openai-response"), first, "gpt-5.4", "resp_test", state)
	if err != nil {
		t.Fatalf("TranslateResponseStreamAuto(first) error = %v", err)
	}
	translatedSecond, err := TranslateResponseStreamAuto(nil, "openai-response", sdktranslator.FromString("openai-response"), second, "gpt-5.4", "resp_test", state)
	if err != nil {
		t.Fatalf("TranslateResponseStreamAuto(second) error = %v", err)
	}

	joinedFirst := stringJoinBytes(translatedFirst)
	joinedSecond := stringJoinBytes(translatedSecond)
	if count := strings.Count(joinedFirst, "event: response.created") + strings.Count(joinedSecond, "event: response.created"); count != 1 {
		t.Fatalf("expected exactly one response.created across chunks, got %d\nfirst=%s\nsecond=%s", count, joinedFirst, joinedSecond)
	}
	if strings.Contains(joinedSecond, "event: response.created") {
		t.Fatalf("did not expect response.created on second chunk, got %s", joinedSecond)
	}
	if state.ResponsesState == nil {
		t.Fatal("ResponsesState was not preserved on UnifiedStreamState")
	}
	if state.ResponsesState.Seq <= 2 {
		t.Fatalf("expected sequence to advance across chunks, got %d", state.ResponsesState.Seq)
	}
	if state.ResponsesState.TextBuffer != "ok" {
		t.Fatalf("TextBuffer = %q, want ok", state.ResponsesState.TextBuffer)
	}
}

func TestTranslateResponseNonStreamAuto_OllamaUsesOllamaParser(t *testing.T) {
	resp := []byte(`{"model":"llama3","message":{"role":"assistant","content":"ok"},"done":true,"prompt_eval_count":2,"eval_count":3}`)

	translated, err := TranslateResponseNonStreamAuto(nil, "ollama", sdktranslator.FromString("openai"), resp, "llama3")
	if err != nil {
		t.Fatalf("TranslateResponseNonStreamAuto() error = %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(translated, &parsed); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if parsed["object"] != "chat.completion" {
		t.Fatalf("object = %#v, want chat.completion", parsed["object"])
	}
	choices, ok := parsed["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %#v", parsed["choices"])
	}
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if message["content"] != "ok" {
		t.Fatalf("content = %#v, want ok", message["content"])
	}
	usage := parsed["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(2) || usage["completion_tokens"] != float64(3) {
		t.Fatalf("usage = %#v, want prompt=2 completion=3", usage)
	}
}

func TestTranslateResponseStreamAuto_OllamaUsesOllamaParser(t *testing.T) {
	state := &UnifiedStreamState{}
	chunk := []byte(`{"model":"llama3","message":{"role":"assistant","content":"ok"},"done":false}`)

	translated, err := TranslateResponseStreamAuto(nil, "ollama", sdktranslator.FromString("openai"), chunk, "llama3", "msg_test", state)
	if err != nil {
		t.Fatalf("TranslateResponseStreamAuto() error = %v", err)
	}
	joined := stringJoinBytes(translated)
	if !strings.Contains(joined, `"object": "chat.completion.chunk"`) {
		t.Fatalf("expected chat.completion.chunk payload, got %s", joined)
	}
	if !strings.Contains(joined, `"content": "ok"`) {
		t.Fatalf("expected token content ok, got %s", joined)
	}
}

func stringJoinBytes(chunks [][]byte) string {
	b := strings.Builder{}
	for _, chunk := range chunks {
		b.Write(chunk)
	}
	return b.String()
}
