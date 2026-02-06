package kilocode

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	baseauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth"
)

// TokenStorage is persisted to auths/kilocode-*.json.
// It follows the same shape as internal auth storages: include "type" and the
// minimal fields needed to refresh runtime access tokens.
type TokenStorage struct {
	Type string `json:"type"`

	BearerToken string `json:"bearer_token"`
	UserEmail   string `json:"user_email"`

	// Deprecated legacy fields (Roo Clerk flow). Kept for backward compatibility.
	ClientToken    string  `json:"client_token"`
	SessionID      string  `json:"session_id"`
	OrganizationID *string `json:"organization_id"`
	SessionTokenJWT string `json:"session_token_jwt"`
	ExpiresAt       string `json:"expires_at"`

	LastRefresh string `json:"last_refresh"`

	Alias string `json:"alias"`
}

var _ baseauth.TokenStorage = (*TokenStorage)(nil)

func (t *TokenStorage) SaveTokenToFile(authFilePath string) error {
	if strings.TrimSpace(authFilePath) == "" {
		return fmt.Errorf("kilocode token storage: authFilePath is empty")
	}
	if strings.TrimSpace(t.Type) == "" {
		t.Type = "kilocode"
	}
	if strings.TrimSpace(t.LastRefresh) == "" {
		t.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	}

	payload, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("kilocode token storage: marshal failed: %w", err)
	}
	if err := os.WriteFile(authFilePath, payload, 0o600); err != nil {
		return fmt.Errorf("kilocode token storage: write failed: %w", err)
	}
	return nil
}
