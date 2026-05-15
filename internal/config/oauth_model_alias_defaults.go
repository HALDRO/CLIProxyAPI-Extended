package config

import "strings"

// defaultKiroAliases returns default oauth-model-alias entries for Kiro.
// These aliases expose standard Claude IDs for Kiro-prefixed upstream models.
func defaultKiroAliases() []OAuthModelAlias {
	return []OAuthModelAlias{}
}

// defaultGitHubCopilotAliases returns default oauth-model-alias entries for
// GitHub Copilot Claude models. It exposes hyphen-style IDs used by clients.
func defaultGitHubCopilotAliases() []OAuthModelAlias {
	return []OAuthModelAlias{
		{Name: "claude-haiku-4.5", Alias: "claude-haiku-4-5", Fork: true},
		{Name: "claude-opus-4.1", Alias: "claude-opus-4-1", Fork: true},
		{Name: "claude-opus-4.5", Alias: "claude-opus-4-5", Fork: true},
		{Name: "claude-opus-4.6", Alias: "claude-opus-4-6", Fork: true},
		{Name: "claude-opus-4.7", Alias: "claude-opus-4-7", Fork: true},
		{Name: "claude-sonnet-4.5", Alias: "claude-sonnet-4-5", Fork: true},
		{Name: "claude-sonnet-4.6", Alias: "claude-sonnet-4-6", Fork: true},
	}
}

// GitHubCopilotAliasesFromModels generates oauth-model-alias entries from a dynamic
// list of model IDs fetched from the Copilot API. It auto-creates aliases for
// models whose ID contains a dot (e.g. "claude-opus-4.6" → "claude-opus-4-6"),
// which is the pattern used by Claude models on Copilot.
func GitHubCopilotAliasesFromModels(modelIDs []string) []OAuthModelAlias {
	var aliases []OAuthModelAlias
	seen := make(map[string]struct{})
	for _, id := range modelIDs {
		if !strings.Contains(id, ".") {
			continue
		}
		hyphenID := strings.ReplaceAll(id, ".", "-")
		key := id + "→" + hyphenID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		aliases = append(aliases, OAuthModelAlias{Name: id, Alias: hyphenID, Fork: true})
	}
	return aliases
}
