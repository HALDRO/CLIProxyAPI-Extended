package translator_new

import (
	"bytes"
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	executor "github.com/router-for-me/CLIProxyAPI/v6/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/from_ir"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

// Adapter wires translator_new into sdk/translator via CanonicalAdapter.
//
// This lives under internal/ so it can import internal translator_new implementation.
// The SDK calls it through an interface without importing internal/.
type Adapter struct {
	Cfg *config.Config
}

func (a *Adapter) TranslateRequest(ctx context.Context, from, to sdktranslator.Format, model string, rawJSON []byte, stream bool) ([]byte, error) {
	cfg := a.Cfg
	payload := bytes.Clone(rawJSON)

	switch to.String() {
	case "gemini":
		return executor.TranslateToGemini(cfg, from, model, payload, stream, nil)
	case "gemini-cli", "antigravity":
		return executor.TranslateToGeminiCLI(cfg, from, model, payload, stream, nil)
	case "claude":
		return executor.TranslateToClaude(cfg, from, model, payload, stream, nil)
	case "openai":
		return executor.TranslateToOpenAI(cfg, from, model, payload, stream, nil, executor.FormatChatCompletions)
	case "codex":
		// Codex uses a stricter Responses API upstream.
		return executor.TranslateToCodex(cfg, from, model, payload, stream, nil)
	default:
		return nil, fmt.Errorf("canonical translator: unsupported request target format %q", to.String())
	}
}

func (a *Adapter) TranslateNonStream(ctx context.Context, from, to sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) (string, error) {
	cfg := a.Cfg

	provider := from.String()
	translated, err := executor.TranslateResponseNonStreamAuto(cfg, provider, to, bytes.Clone(rawJSON), model)
	if err != nil {
		return "", err
	}
	return string(translated), nil
}

func (a *Adapter) TranslateStream(ctx context.Context, from, to sdktranslator.Format, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) ([]string, error) {
	cfg := a.Cfg
	provider := from.String()
	messageID := "chatcmpl-" + model

	var state any
	if param != nil {
		state = *param
	}
	if state == nil {
		switch provider {
		case "gemini", "gemini-cli", "antigravity", "aistudio":
			state = &executor.GeminiCLIStreamState{ClaudeState: from_ir.NewClaudeStreamState()}
		case "claude":
			state = from_ir.NewClaudeStreamState()
		case "openai", "codex", "cline", "ollama":
			state = &executor.OpenAIStreamState{}
		default:
			return nil, fmt.Errorf("canonical translator: unsupported stream provider %q", provider)
		}
		if param != nil {
			*param = state
		}
	}

	chunks, err := executor.TranslateResponseStreamAuto(cfg, provider, to, bytes.Clone(rawJSON), model, messageID, state)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, string(c))
	}
	return out, nil
}
