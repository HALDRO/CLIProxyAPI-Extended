package registry

import "strings"

// ParseProviderPrefixedModelID parses model IDs with optional visual provider prefixes.
//
// Supported display formats:
//   - "[Gemini CLI] gemini-2.5-pro" -> ("gemini-2.5-pro", "gemini-cli")
//   - "gemini-2.5-pro"             -> ("gemini-2.5-pro", "")
//
// Returns normalized model ID and provider ID (not label).
func ParseProviderPrefixedModelID(modelID string) (normalized string, providerID string) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", ""
	}

	if !strings.HasPrefix(modelID, "[") {
		return modelID, ""
	}

	idx := strings.Index(modelID, "]")
	if idx <= 1 || idx+1 >= len(modelID) {
		return modelID, ""
	}

	label := strings.TrimSpace(modelID[1:idx])
	normalized = strings.TrimSpace(modelID[idx+1:])
	if normalized == "" {
		return modelID, ""
	}

	providerID = labelToProviderID(label)
	return normalized, providerID
}

func formatProviderPrefixedModelID(provider, modelID string) string {
	provider = strings.TrimSpace(provider)
	modelID = strings.TrimSpace(modelID)
	if provider == "" || modelID == "" {
		return modelID
	}

	label := providerIDToLabel(provider)
	return "[" + label + "] " + modelID
}

// providerIDToLabel converts provider ID to display label
func providerIDToLabel(provider string) string {
	switch strings.ToLower(provider) {
	case "gemini-cli":
		return "Gemini CLI"
	case "antigravity":
		return "Antigravity"
	case "vertex":
		return "Vertex"
	case "aistudio":
		return "AI Studio"
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "cline":
		return "Cline"
	case "qwen":
		return "Qwen"
	case "kiro":
		return "Kiro"
	case "openai", "openai-compatibility":
		return "OpenAI"
	default:
		return provider
	}
}

func labelToProviderID(label string) string {
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "gemini cli":
		return "gemini-cli"
	case "antigravity":
		return "antigravity"
	case "vertex":
		return "vertex"
	case "ai studio":
		return "aistudio"
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "cline":
		return "cline"
	case "qwen":
		return "qwen"
	case "kiro":
		return "kiro"
	case "openai":
		return "openai"
	default:
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(label), " ", "-"))
	}
}
