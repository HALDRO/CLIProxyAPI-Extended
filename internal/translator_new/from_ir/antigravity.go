package from_ir

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/to_ir"
)

const antigravityIdentity = "You are Antigravity, a powerful agentic AI coding assistant designed by the Google Deepmind team working on Advanced Agentic Coding.\n" +
	"You are pair programming with a USER to solve their coding task. The task may require creating a new codebase, modifying or debugging an existing codebase, or simply answering a question.\n" +
	"**Absolute paths only**\n" +
	"**Proactiveness**"

// AntigravityProvider converts UnifiedChatRequest into Antigravity v1internal envelope.
//
// Expected metadata keys (optional):
// - project_id: string
// - request_type: string ("agent"|"web_search"|"image_gen")
// - request_id: string
// - user_agent: string
// - session_id: string (used for thoughtSignature injection)
//
// NOTE: The inner request format is Gemini (AI Studio) JSON.
//
// Behavior notes (parity with NiceAG reference):
// - Deep-clean "[undefined]" placeholders.
// - Inject session thoughtSignature into functionCall parts if missing.
// - Inject Antigravity identity into systemInstruction (non-image requests).
// - For image_gen: strip tools + systemInstruction, attach imageConfig.
type AntigravityProvider struct{}

func (p *AntigravityProvider) ConvertRequest(req *ir.UnifiedChatRequest) ([]byte, error) {
	if req == nil {
		return nil, nil
	}

	innerJSON, err := (&GeminiProvider{}).ConvertRequest(req)
	if err != nil {
		return nil, err
	}

	// Unmarshal inner request to mutate safely.
	var inner any
	if err := json.Unmarshal(innerJSON, &inner); err != nil {
		return nil, err
	}

	// Deep clean "[undefined]" values (Cherry Studio compatibility).
	if m, ok := inner.(map[string]any); ok {
		ir.DeepCleanUndefined(m)
	}

	// Inject cached thoughtSignature into functionCall parts when missing.
	// This is critical for tool loops when clients strip thoughtSignature.
	sessionID := metaString(req.Metadata, "session_id")
	if sessionID != "" {
		injectThoughtSignature(inner, sessionID)
	}

	// [FIX] Clean tool declarations (remove forbidden Schema fields and redundant search decls)
	// Similar to NiceAG reference wrap_request logic.
	if tools, ok := inner.(map[string]any)["tools"].([]any); ok {
		for _, t := range tools {
			if toolMap, ok := t.(map[string]any); ok {
				if decls, ok := toolMap["functionDeclarations"].([]any); ok {
					var cleanedDecls []any
					for _, d := range decls {
						if declMap, ok := d.(map[string]any); ok {
							// Filter out redundant networking tools if configured (optional, skipping for now to keep canonical)
							// Clean parameters schema
							if params, ok := declMap["parameters"].(map[string]any); ok {
								// Re-run schema cleaner (enhanced) to be safe
								declMap["parameters"] = to_ir.CleanJsonSchemaEnhanced(params)
							}
							cleanedDecls = append(cleanedDecls, declMap)
						}
					}
					toolMap["functionDeclarations"] = cleanedDecls
				}
			}
		}
	}

	// Build envelope fields.
	projectID := strings.TrimSpace(metaString(req.Metadata, "project_id"))
	requestType := strings.TrimSpace(metaString(req.Metadata, "request_type"))
	requestID := strings.TrimSpace(metaString(req.Metadata, "request_id"))
	userAgent := strings.TrimSpace(metaString(req.Metadata, "user_agent"))
	if requestID == "" {
		requestID = "agent-" + uuid.NewString()
	} else if !strings.HasPrefix(requestID, "agent-") {
		// Enforce agent- prefix if missing
		requestID = "agent-" + requestID
	}
	if userAgent == "" {
		userAgent = "antigravity"
	}
	if requestType == "" {
		requestType = "agent"
	}

	// Inject Antigravity identity injection for non-image requests.
	if requestType != "image_gen" {
		injectAntigravityIdentity(inner)
	} else {
		applyImageGenTweaks(inner, req)
	}

	// [FIX] Ensure we remove maxOutputTokens if it exceeds 8192 for non-Claude models to prevent 400 errors
	// or unexpected behavior. Canonical translator might have set it.
	// Actually, let's just rely on upstream defaults unless explicitly set low.
	// We can inspect inner["generationConfig"] and remove maxOutputTokens if it looks like a default.
	if genConfig, ok := inner.(map[string]any)["generationConfig"].(map[string]any); ok {
		if maxTok, ok := genConfig["maxOutputTokens"].(float64); ok && maxTok > 8192 && !strings.Contains(req.Model, "claude") {
			// Remove potentially unsafe large token limit for Gemini models
			delete(genConfig, "maxOutputTokens")
		}
	}

	// Marshal inner request back.
	innerRaw, err := json.Marshal(inner)
	if err != nil {
		return nil, err
	}

	envelope := map[string]any{
		"project":     projectID,
		"requestId":   requestID,
		"request":     json.RawMessage(innerRaw),
		"model":       req.Model,
		"userAgent":   userAgent,
		"requestType": requestType,
	}

	return json.Marshal(envelope)
}

func metaString(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	if v, ok := meta[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func injectThoughtSignature(inner any, sessionID string) {
	sig := cache.GetSessionThoughtSignature(sessionID)
	if sig == "" {
		return
	}

	obj, ok := inner.(map[string]any)
	if !ok {
		return
	}
	contents, ok := obj["contents"].([]any)
	if !ok {
		return
	}

	for _, content := range contents {
		cObj, ok := content.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := cObj["parts"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			pObj, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if _, hasFC := pObj["functionCall"]; !hasFC {
				continue
			}
			if existing, ok := pObj["thoughtSignature"].(string); ok && strings.TrimSpace(existing) != "" {
				continue
			}
			pObj["thoughtSignature"] = sig
		}
	}
}

func injectAntigravityIdentity(inner any) {
	root, ok := inner.(map[string]any)
	if !ok {
		return
	}

	si, ok := root["systemInstruction"].(map[string]any)
	if !ok {
		root["systemInstruction"] = map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": antigravityIdentity}},
		}
		return
	}

	if _, ok := si["role"]; !ok {
		si["role"] = "user"
	}

	parts, ok := si["parts"].([]any)
	if !ok || len(parts) == 0 {
		si["parts"] = []any{map[string]any{"text": antigravityIdentity}}
		return
	}

	if first, ok := parts[0].(map[string]any); ok {
		if txt, ok := first["text"].(string); ok {
			if strings.Contains(txt, "You are Antigravity") {
				return
			}
		}
	}

	si["parts"] = append([]any{map[string]any{"text": antigravityIdentity}}, parts...)
}

func applyImageGenTweaks(inner any, req *ir.UnifiedChatRequest) {
	root, ok := inner.(map[string]any)
	if !ok {
		return
	}

	// Image generation does not support tools or system prompts.
	delete(root, "tools")
	delete(root, "toolConfig")
	delete(root, "systemInstruction")

	gen, ok := root["generationConfig"].(map[string]any)
	if !ok {
		gen = map[string]any{}
		root["generationConfig"] = gen
	}

	// Clean generationConfig (avoid incompatible fields).
	delete(gen, "thinkingConfig")
	delete(gen, "responseMimeType")
	delete(gen, "responseModalities")

	if req != nil && req.ImageConfig != nil {
		img := map[string]any{}
		if strings.TrimSpace(req.ImageConfig.AspectRatio) != "" {
			img["aspectRatio"] = req.ImageConfig.AspectRatio
		}
		if strings.TrimSpace(req.ImageConfig.ImageSize) != "" {
			img["imageSize"] = req.ImageConfig.ImageSize
		}
		if len(img) > 0 {
			gen["imageConfig"] = img
		}
	}
}
