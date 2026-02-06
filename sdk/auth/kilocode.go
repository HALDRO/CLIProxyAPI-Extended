package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kilocode"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

// KiloCodeAuthenticator implements the interactive login flow for KiloCode.
//
// UX goal: user runs one command, browser opens, then we poll device auth and save bearer token.
//
// Official flow (kilo.ai):
//   - POST https://api.kilo.ai/api/device-auth/codes
//   - Open verificationUrl in browser
//   - Poll GET https://api.kilo.ai/api/device-auth/codes/{code}
//   - Save returned bearer token
type KiloCodeAuthenticator struct{}

func NewKiloCodeAuthenticator() *KiloCodeAuthenticator {
	return &KiloCodeAuthenticator{}
}

func (a *KiloCodeAuthenticator) Provider() string {
	return "kilocode"
}

// RefreshLead: we can refresh shortly before expiry.
func (a *KiloCodeAuthenticator) RefreshLead() *time.Duration {
	d := 2 * time.Minute
	return &d
}

func (a *KiloCodeAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil {
		return nil, fmt.Errorf("kilocode auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	svc := kilocode.NewAuthService(cfg)

	init, err := svc.InitiateDeviceAuth(ctx)
	if err != nil {
		return nil, err
	}

	fmt.Println("\nKiloCode login (device auth):")
	fmt.Println("1) A browser window will open.")
	fmt.Println("2) Confirm the sign-in in the browser.")
	fmt.Println("3) This CLI will keep polling until approved.")
	fmt.Println("\nLogin URL:")
	fmt.Println(init.VerificationURL)
	fmt.Println("\nVerification code:")
	fmt.Println(init.Code)

	if !opts.NoBrowser {
		_ = openBrowser(init.VerificationURL)
	}

	deadline := time.Now().Add(time.Duration(init.ExpiresIn) * time.Second)
	pollInterval := 3 * time.Second

	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("kilocode device auth: expired")
		}

		pollResp, statusCode, err := svc.PollDeviceAuth(ctx, strings.TrimSpace(init.Code))
		if err != nil {
			return nil, err
		}

		switch strings.ToLower(strings.TrimSpace(pollResp.Status)) {
		case "pending":
			// continue
		case "denied":
			return nil, fmt.Errorf("kilocode device auth: denied")
		case "expired":
			return nil, fmt.Errorf("kilocode device auth: expired")
		case "approved":
			if strings.TrimSpace(pollResp.Token) == "" {
				return nil, fmt.Errorf("kilocode device auth: approved but token empty")
			}

			now := time.Now().UTC()
			storage := &kilocode.TokenStorage{
				Type:         "kilocode",
				BearerToken:  strings.TrimSpace(pollResp.Token),
				UserEmail:    strings.TrimSpace(pollResp.UserEmail),
				LastRefresh:  now.Format(time.RFC3339),
				Alias:        "",
			}

			idPart := randomHex(8)
			fileName := fmt.Sprintf("kilocode-%s.json", idPart)

			record := &coreauth.Auth{
				ID:        fileName,
				Provider:  a.Provider(),
				FileName:  fileName,
				Label:     "kilocode",
				Status:    coreauth.StatusActive,
				CreatedAt: now,
				UpdatedAt: now,
				Metadata: map[string]any{
					"type":         "kilocode",
					"bearer_token": storage.BearerToken,
					"user_email":   storage.UserEmail,
					"last_refresh": storage.LastRefresh,
					"source":       "kilo-device-auth",
					"http_status":  statusCode,
				},
				Storage: storage,
			}

			fmt.Println("\n\n✓ KiloCode authentication completed successfully!")
			return record, nil
		default:
			// Some servers might respond with empty JSON on 202.
			if statusCode == 202 {
				// pending
			} else {
				return nil, fmt.Errorf("kilocode device auth: unexpected status=%q (http=%d)", pollResp.Status, statusCode)
			}
		}

		time.Sleep(pollInterval)
	}
}

func openBrowser(u string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		return exec.Command("open", u).Start()
	default:
		return exec.Command("xdg-open", u).Start()
	}
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
