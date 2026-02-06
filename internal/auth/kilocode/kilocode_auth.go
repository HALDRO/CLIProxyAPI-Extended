package kilocode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

const (
	DefaultKiloAPIBaseURL = "https://api.kilo.ai"
)

type deviceAuthInitiateResponse struct {
	Code            string `json:"code"`
	VerificationURL string `json:"verificationUrl"`
	ExpiresIn       int    `json:"expiresIn"`
}

type deviceAuthPollResponse struct {
	Status    string `json:"status"`
	Token     string `json:"token"`
	UserEmail string `json:"userEmail"`
}

type AuthService struct {
	httpClient *http.Client
	apiBaseURL string
}

func NewAuthService(cfg *config.Config) *AuthService {
	client := &http.Client{Timeout: 30 * time.Second}
	if cfg != nil {
		client = util.SetProxy(&cfg.SDKConfig, client)
	}
	return &AuthService{httpClient: client, apiBaseURL: DefaultKiloAPIBaseURL}
}

func (s *AuthService) SetAPIBaseURL(base string) {
	if base != "" {
		s.apiBaseURL = base
	}
}

func (s *AuthService) InitiateDeviceAuth(ctx context.Context) (*deviceAuthInitiateResponse, error) {
	endpoint := s.apiBaseURL + "/api/device-auth/codes"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("kilocode device-auth initiate failed: %d: %s", resp.StatusCode, string(body))
	}

	var out deviceAuthInitiateResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("kilocode device-auth initiate parse json: %w", err)
	}
	if out.Code == "" || out.VerificationURL == "" || out.ExpiresIn <= 0 {
		return nil, fmt.Errorf("kilocode device-auth initiate returned incomplete payload")
	}
	return &out, nil
}

func (s *AuthService) PollDeviceAuth(ctx context.Context, code string) (*deviceAuthPollResponse, int, error) {
	endpoint := s.apiBaseURL + "/api/device-auth/codes/" + code
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	status := resp.StatusCode
	body, _ := io.ReadAll(resp.Body)

	// 202 pending, 403 denied, 410 expired, 200 approved
	if status == http.StatusAccepted || status == http.StatusForbidden || status == http.StatusGone {
		// For these statuses, Kilo returns either empty body or {status:...}
		var out deviceAuthPollResponse
		_ = json.Unmarshal(body, &out)
		if out.Status == "" {
			switch status {
			case http.StatusAccepted:
				out.Status = "pending"
			case http.StatusForbidden:
				out.Status = "denied"
			case http.StatusGone:
				out.Status = "expired"
			}
		}
		return &out, status, nil
	}

	if status < 200 || status >= 300 {
		return nil, status, fmt.Errorf("kilocode device-auth poll failed: %d: %s", status, string(body))
	}

	var out deviceAuthPollResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, status, fmt.Errorf("kilocode device-auth poll parse json: %w", err)
	}
	return &out, status, nil
}
