package executor

import (
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

func stringJoinBytes(chunks [][]byte) string {
	b := strings.Builder{}
	for _, chunk := range chunks {
		b.Write(chunk)
	}
	return b.String()
}
