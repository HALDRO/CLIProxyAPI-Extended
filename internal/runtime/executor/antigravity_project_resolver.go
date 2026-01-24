package executor

import (
	"context"
	"fmt"
	"strings"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func ensureAntigravityProjectID(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, accessToken string) error {
	if auth == nil {
		return nil
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	if auth.Metadata["project_id"] != nil {
		return nil
	}

	token := strings.TrimSpace(accessToken)
	if token == "" {
		return nil
	}

	client := newProxyAwareHTTPClient(ctx, cfg, auth, 0)
	projectID, errFetch := sdkAuth.FetchAntigravityProjectID(ctx, token, client)
	if errFetch != nil {
		return fmt.Errorf("fetch project id: %w", errFetch)
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil
	}
	auth.Metadata["project_id"] = projectID
	return nil
}
