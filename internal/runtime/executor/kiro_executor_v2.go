/**
 * @file Kiro (Amazon Q) executor implementation
 * @description Optimized executor for Kiro provider with Canonical IR architecture.
 * Includes retry logic, quota fallback, JWT validation, and agentic optimizations.
 */

package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	kiroclaude "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/kiro/claude"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/from_ir"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/to_ir"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	// Kiro API common constants
	kiroDefaultRegionV2 = "us-east-1"

	// Primary endpoint (Amazon Q) - region aware
	// NOTE: Q endpoint supports /generateAssistantResponse and does NOT require X-Amz-Target.
	kiroPrimaryURLTemplateV2 = "https://q.%s.amazonaws.com/generateAssistantResponse"
	// Fallback endpoint (CodeWhisperer) - region aware
	kiroFallbackURLTemplateV2 = "https://codewhisperer.%s.amazonaws.com/generateAssistantResponse"

	kiroRefreshSkew    = 5 * time.Minute
	kiroRequestTimeout = 120 * time.Second
	kiroMaxRetries     = 2

	// Socket retry configuration constants
	kiroSocketMaxRetriesV2    = 3
	kiroSocketBaseRetryDelayV2 = 1 * time.Second
	kiroSocketMaxRetryDelayV2  = 30 * time.Second

	// User-Agent strings (CLI style for non-IDC auth)
	kiroUserAgentV2     = "aws-sdk-rust/1.3.9 os/macos lang/rust/1.87.0"
	kiroFullUserAgentV2 = "aws-sdk-rust/1.3.9 ua/2.1 api/ssooidc/1.88.0 os/macos lang/rust/1.87.0 m/E app/AmazonQ-For-CLI"

	// Kiro API headers
	// Q endpoint uses plain JSON.
	kiroContentTypeV2 = "application/json"
	// CodeWhisperer endpoint requires X-Amz-Target.
	kiroTargetV2 = "AmazonCodeWhispererStreamingService.GenerateAssistantResponse"
	// Streaming responses are returned as AWS EventStream frames.
	kiroAcceptStreamV2 = "application/vnd.amazon.eventstream"

	// Kiro-specific headers (match upstream executor behavior)
	kiroAgentModeHeaderV2 = "vibe"

	// kiroAgenticSystemPrompt prevents AWS Kiro API timeouts during large file operations.
	kiroAgenticSystemPrompt = `
 # CRITICAL: CHUNKED WRITE PROTOCOL (MANDATORY)
 
 You MUST follow these rules for ALL file operations. Violation causes server timeouts and task failure.
 
 ## ABSOLUTE LIMITS
 - **MAXIMUM 350 LINES** per single write/edit operation - NO EXCEPTIONS
 - **RECOMMENDED 300 LINES** or less for optimal performance
 - **NEVER** write entire files in one operation if >300 lines
 
 ## MANDATORY CHUNKED WRITE STRATEGY
 
 ### For NEW FILES (>300 lines total):
 1. FIRST: Write initial chunk (first 250-300 lines) using write_to_file/fsWrite
 2. THEN: Append remaining content in 250-300 line chunks using file append operations
 3. REPEAT: Continue appending until complete
 
 ### For EDITING EXISTING FILES:
 1. Use surgical edits (apply_diff/targeted edits) - change ONLY what's needed
 2. NEVER rewrite entire files - use incremental modifications
 3. Split large refactors into multiple small, focused edits
 
 REMEMBER: When in doubt, write LESS per operation. Multiple small operations > one large operation.`
)

// kiroModelMapping maps model IDs to Kiro API model IDs.
// Comprehensive mapping supporting all model name formats.
var kiroModelMapping = map[string]string{
	// Amazon Q format (amazonq- prefix) - same API as Kiro
	"amazonq-auto":                       "auto",
	"amazonq-claude-opus-4-6":            "claude-opus-4.6",
	"amazonq-claude-opus-4-5":            "claude-opus-4.5",
	"amazonq-claude-sonnet-4-5":          "claude-sonnet-4.5",
	"amazonq-claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"amazonq-claude-sonnet-4":            "claude-sonnet-4",
	"amazonq-claude-sonnet-4-20250514":   "claude-sonnet-4",
	"amazonq-claude-haiku-4-5":           "claude-haiku-4.5",
	// Kiro format (kiro- prefix) - valid model names that should be preserved
	"kiro-claude-opus-4-6":            "claude-opus-4.6",
	"kiro-claude-opus-4-5":            "claude-opus-4.5",
	"kiro-claude-sonnet-4-5":          "claude-sonnet-4.5",
	"kiro-claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"kiro-claude-sonnet-4":            "claude-sonnet-4",
	"kiro-claude-sonnet-4-20250514":   "claude-sonnet-4",
	"kiro-claude-haiku-4-5":           "claude-haiku-4.5",
	"kiro-auto":                       "auto",
	// Native format (no prefix) - used by Kiro IDE directly
	"claude-opus-4-6":            "claude-opus-4.6",
	"claude-opus-4.6":            "claude-opus-4.6",
	"claude-opus-4-5":            "claude-opus-4.5",
	"claude-opus-4.5":            "claude-opus-4.5",
	"claude-haiku-4-5":           "claude-haiku-4.5",
	"claude-haiku-4.5":           "claude-haiku-4.5",
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4.5",
	"claude-sonnet-4.5":          "claude-sonnet-4.5",
	"claude-sonnet-4":            "claude-sonnet-4",
	"claude-sonnet-4-20250514":   "claude-sonnet-4",
	"auto":                       "auto",
	// Agentic variants (same backend model IDs, but with special system prompt)
	"claude-opus-4.6-agentic":        "claude-opus-4.6",
	"claude-opus-4.5-agentic":        "claude-opus-4.5",
	"claude-sonnet-4.5-agentic":      "claude-sonnet-4.5",
	"claude-sonnet-4-agentic":        "claude-sonnet-4",
	"claude-haiku-4.5-agentic":       "claude-haiku-4.5",
	"kiro-claude-opus-4-6-agentic":   "claude-opus-4.6",
	"kiro-claude-opus-4-5-agentic":   "claude-opus-4.5",
	"kiro-claude-sonnet-4-5-agentic": "claude-sonnet-4.5",
	"kiro-claude-sonnet-4-agentic":   "claude-sonnet-4",
	"kiro-claude-haiku-4-5-agentic":  "claude-haiku-4.5",
}

// retryableHTTPStatusCodesV2 defines HTTP status codes that are retryable (500, 502, 503, 504).
var retryableHTTPStatusCodesV2 = map[int]bool{
	500: true,
	502: true,
	503: true,
	504: true,
}

// Global FingerprintManager for dynamic User-Agent generation per token
var (
	globalFingerprintManagerV2     *kiroauth.FingerprintManager
	globalFingerprintManagerOnceV2 sync.Once
)

func getGlobalFingerprintManagerV2() *kiroauth.FingerprintManager {
	globalFingerprintManagerOnceV2.Do(func() {
		globalFingerprintManagerV2 = kiroauth.NewFingerprintManager()
		log.Infof("kiro-v2: initialized global FingerprintManager for dynamic UA generation")
	})
	return globalFingerprintManagerV2
}

// Global pooled HTTP client for connection reuse
var (
	kiroHTTPClientPoolV2     *http.Client
	kiroHTTPClientPoolOnceV2 sync.Once
)

func getKiroPooledHTTPClientV2() *http.Client {
	kiroHTTPClientPoolOnceV2.Do(func() {
		transport := &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 20,
			MaxConnsPerHost:     50,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true,
		}
		kiroHTTPClientPoolV2 = &http.Client{Transport: transport}
		log.Debugf("kiro-v2: initialized pooled HTTP client (MaxIdleConns=100, MaxConnsPerHost=50)")
	})
	return kiroHTTPClientPoolV2
}

// newPooledHTTPClientV2 returns a pooled client, or a proxy-aware client if proxy is configured.
func newPooledHTTPClientV2(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}
	if proxyURL != "" {
		return newProxyAwareHTTPClient(ctx, cfg, auth, timeout)
	}
	pooled := getKiroPooledHTTPClientV2()
	if timeout > 0 {
		return &http.Client{Transport: pooled.Transport, Timeout: timeout}
	}
	return pooled
}

// retryConfigV2 holds retry configuration for V2 executor.
type retryConfigV2 struct {
	MaxRetries      int
	BaseDelay       time.Duration
	MaxDelay        time.Duration
	RetryableErrors []string
}

func defaultRetryConfigV2() retryConfigV2 {
	return retryConfigV2{
		MaxRetries: kiroSocketMaxRetriesV2,
		BaseDelay:  kiroSocketBaseRetryDelayV2,
		MaxDelay:   kiroSocketMaxRetryDelayV2,
		RetryableErrors: []string{
			"connection reset", "connection refused", "broken pipe",
			"EOF", "timeout", "temporary failure", "no such host",
			"network is unreachable", "i/o timeout",
		},
	}
}

// isRetryableErrorV2 checks if an error is retryable (network errors, timeouts, etc.).
func isRetryableErrorV2(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var syscallErr syscall.Errno
	if errors.As(err, &syscallErr) {
		switch syscallErr {
		case syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE,
			syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			return true
		}
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Err != nil {
			return isRetryableErrorV2(opErr.Err)
		}
		return true
	}
	errMsg := strings.ToLower(err.Error())
	for _, pattern := range defaultRetryConfigV2().RetryableErrors {
		if strings.Contains(errMsg, pattern) {
			return true
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	return false
}

func calculateRetryDelayV2(attempt int) time.Duration {
	cfg := defaultRetryConfigV2()
	return kiroauth.ExponentialBackoffWithJitter(attempt, cfg.BaseDelay, cfg.MaxDelay)
}

// getTokenKeyV2 returns a unique key for rate limiting based on auth credentials.
func getTokenKeyV2(auth *coreauth.Auth) string {
	if auth != nil && auth.ID != "" {
		return auth.ID
	}
	token := getMetaString(auth.Metadata, "access_token", "accessToken")
	if len(token) > 16 {
		return token[:16]
	}
	return token
}

// isIDCAuthV2 checks if the auth uses IDC (Identity Center) authentication.
func isIDCAuthV2(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	authMethod, _ := auth.Metadata["auth_method"].(string)
	return strings.ToLower(authMethod) == "idc"
}

// applyDynamicFingerprintV2 applies token-specific fingerprint headers to the request.
// IDC auth gets dynamic UA, others get static Amazon Q CLI style headers.
func applyDynamicFingerprintV2(req *http.Request, auth *coreauth.Auth) {
	if isIDCAuthV2(auth) {
		tokenKey := getTokenKeyV2(auth)
		fp := getGlobalFingerprintManagerV2().GetFingerprint(tokenKey)
		req.Header.Set("User-Agent", fp.BuildUserAgent())
		req.Header.Set("X-Amz-User-Agent", fp.BuildAmzUserAgent())
		log.Debugf("kiro-v2: dynamic fingerprint for token %s...", tokenKey[:min(8, len(tokenKey))])
	} else {
		req.Header.Set("User-Agent", kiroUserAgentV2)
		req.Header.Set("X-Amz-User-Agent", kiroFullUserAgentV2)
	}
}

type KiroExecutorV2 struct {
	cfg       *config.Config
	refreshMu sync.Mutex // Serializes token refresh operations
}

func NewKiroExecutorV2(cfg *config.Config) *KiroExecutorV2 {
	return &KiroExecutorV2{cfg: cfg}
}

func (e *KiroExecutorV2) Identifier() string { return constant.Kiro }

// PrepareRequest prepares the HTTP request with necessary headers and authentication.
func (e *KiroExecutorV2) PrepareRequest(_ *http.Request, _ *coreauth.Auth) error {
	return nil
}

// HttpRequest executes an HTTP request with Kiro credentials.
func (e *KiroExecutorV2) HttpRequest(ctx context.Context, auth *coreauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kiro executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// isJWTExpired checks if a JWT access token has expired.
// Optimized: extracts exp claim without full JSON unmarshal when possible.
func isJWTExpired(token string) bool {
	if token == "" {
		return true
	}

	// JWT format: header.payload.signature
	firstDot := strings.Index(token, ".")
	if firstDot == -1 {
		return false // Not a JWT, assume valid
	}
	secondDot := strings.Index(token[firstDot+1:], ".")
	if secondDot == -1 {
		return false // Not a JWT, assume valid
	}

	payload := token[firstDot+1 : firstDot+1+secondDot]

	// Base64URL decode (add padding if needed)
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		// Try with standard padding
		padded := payload
		switch len(payload) % 4 {
		case 2:
			padded += "=="
		case 3:
			padded += "="
		}
		decoded, err = base64.StdEncoding.DecodeString(padded)
		if err != nil {
			return false // Can't decode, assume valid
		}
	}

	// Fast exp extraction using string search instead of full JSON unmarshal
	expIdx := strings.Index(string(decoded), `"exp":`)
	if expIdx == -1 {
		return false // No exp claim, assume valid
	}

	// Parse the number after "exp":
	numStart := expIdx + 6
	for numStart < len(decoded) && (decoded[numStart] == ' ' || decoded[numStart] == '\t') {
		numStart++
	}

	numEnd := numStart
	for numEnd < len(decoded) && decoded[numEnd] >= '0' && decoded[numEnd] <= '9' {
		numEnd++
	}

	if numEnd == numStart {
		return false // No valid number, assume valid
	}

	// Parse exp timestamp
	var exp int64
	for i := numStart; i < numEnd; i++ {
		exp = exp*10 + int64(decoded[i]-'0')
	}

	if exp == 0 {
		return false
	}

	expTime := time.Unix(exp, 0)
	return time.Now().After(expTime) || time.Until(expTime) < time.Minute
}

// determineOrigin returns the origin based on model type.
// Opus models use AI_EDITOR (Kiro IDE quota), others use CLI (Amazon Q quota).
func (e *KiroExecutorV2) determineOrigin(model string) string {
	if strings.Contains(strings.ToLower(model), "opus") {
		return "AI_EDITOR"
	}
	return "CLI"
}

// isAgenticModel checks if the model is an agentic variant.
func (e *KiroExecutorV2) isAgenticModel(model string) bool {
	return strings.HasSuffix(model, "-agentic")
}

func (e *KiroExecutorV2) ensureValidToken(ctx context.Context, auth *coreauth.Auth) (string, *coreauth.Auth, error) {
	if auth == nil {
		return "", nil, fmt.Errorf("kiro: auth is nil")
	}
	token := getMetaString(auth.Metadata, "access_token", "accessToken")
	expiry := parseTokenExpiry(auth.Metadata)

	// Check both metadata expiry and JWT expiry (single call)
	jwtExpired := isJWTExpired(token)
	if token != "" && expiry.After(time.Now().Add(kiroRefreshSkew)) && !jwtExpired {
		return token, nil, nil
	}

	log.Debugf("kiro: token needs refresh (expiry: %v, jwt_expired: %v)", expiry, jwtExpired)
	updatedAuth, err := e.Refresh(ctx, auth)
	if err != nil {
		return "", nil, fmt.Errorf("kiro: token refresh failed: %w", err)
	}
	return getMetaString(updatedAuth.Metadata, "access_token", "accessToken"), updatedAuth, nil
}

func (e *KiroExecutorV2) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.refreshMu.Lock()
	defer e.refreshMu.Unlock()

	// Double-check after acquiring lock
	if auth.Metadata != nil {
		if lastRefresh, ok := auth.Metadata["last_refresh"].(string); ok {
			if refreshTime, err := time.Parse(time.RFC3339, lastRefresh); err == nil {
				if time.Since(refreshTime) < 30*time.Second {
					log.Debugf("kiro: token was recently refreshed, skipping")
					return auth, nil
				}
			}
		}
	}

	var creds kiroauth.KiroTokenStorage
	data, _ := json.Marshal(auth.Metadata)
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, err
	}

	var newTokenData *kiroauth.KiroTokenData
	var err error

	if strings.EqualFold(strings.TrimSpace(creds.AuthMethod), "idc") || strings.EqualFold(strings.TrimSpace(creds.AuthMethod), "builder-id") {
		sso := kiroauth.NewSSOOIDCClient(e.cfg)
		region := strings.TrimSpace(creds.Region)
		if region == "" {
			newTokenData, err = sso.RefreshToken(ctx, creds.ClientID, creds.ClientSecret, creds.RefreshToken)
		} else {
			startURL := strings.TrimSpace(creds.StartURL)
			if startURL == "" {
				newTokenData, err = sso.RefreshToken(ctx, creds.ClientID, creds.ClientSecret, creds.RefreshToken)
			} else {
				newTokenData, err = sso.RefreshTokenWithRegion(ctx, creds.ClientID, creds.ClientSecret, creds.RefreshToken, region, startURL)
			}
		}
	} else {
		oauth := kiroauth.NewKiroOAuth(e.cfg)
		newTokenData, err = oauth.RefreshToken(ctx, creds.RefreshToken)
	}
	if err != nil {
		return nil, err
	}

	newMeta := map[string]interface{}{
		"type":          constant.Kiro,
		"access_token":  newTokenData.AccessToken,
		"refresh_token": newTokenData.RefreshToken,
		"profile_arn":   newTokenData.ProfileArn,
		"expires_at":    newTokenData.ExpiresAt,
		"auth_method":   creds.AuthMethod,
		"provider":      creds.Provider,
		"client_id":     creds.ClientID,
		"client_secret": creds.ClientSecret,
		"region":        creds.Region,
		"start_url":     creds.StartURL,
		"email":         creds.Email,
		"last_refresh":  time.Now().Format(time.RFC3339),
	}

	updatedAuth := auth.Clone()
	updatedAuth.Metadata = newMeta
	updatedAuth.LastRefreshedAt = time.Now()
	if store, ok := auth.Storage.(*kiroauth.KiroTokenStorage); ok {
		store.AccessToken = newTokenData.AccessToken
		store.RefreshToken = newTokenData.RefreshToken
		store.ProfileArn = newTokenData.ProfileArn
		store.ExpiresAt = newTokenData.ExpiresAt
		store.AuthMethod = creds.AuthMethod
		store.Provider = creds.Provider
		store.ClientID = creds.ClientID
		store.ClientSecret = creds.ClientSecret
		store.Region = creds.Region
		store.StartURL = creds.StartURL
		store.Email = creds.Email
	}

	log.Infof("kiro: token refreshed successfully")
	return updatedAuth, nil
}

type requestContext struct {
	ctx          context.Context
	auth         *coreauth.Auth
	req          cliproxyexecutor.Request
	token        string
	tokenKey     string
	kiroModelID  string
	requestID    string
	irReq        *ir.UnifiedChatRequest
	kiroBody     []byte
	origin       string
	isAgentic    bool
	sourceFormat string

	apiRegion   string
	useFallback bool
}

func (e *KiroExecutorV2) prepareRequest(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, sourceFormat string) (*requestContext, error) {
	rc := &requestContext{
		ctx:          ctx,
		auth:         auth,
		req:          req,
		requestID:    uuid.New().String()[:8],
		origin:       e.determineOrigin(req.Model),
		isAgentic:    e.isAgenticModel(req.Model),
		sourceFormat: sourceFormat,
	}

	var err error
	rc.token, rc.auth, err = e.ensureValidToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	if rc.auth == nil {
		rc.auth = auth
	}

	rc.kiroModelID = mapModelID(req.Model)
	rc.tokenKey = getTokenKeyV2(rc.auth)

	// Parse request based on source format
	sanitizedPayload := []byte(ir.SanitizeText(string(req.Payload)))
	switch sourceFormat {
	case "claude":
		rc.irReq, err = to_ir.ParseClaudeRequest(sanitizedPayload)
	default:
		// Default to OpenAI format (covers "openai", "cline", etc.)
		rc.irReq, err = to_ir.ParseOpenAIRequest(sanitizedPayload)
	}
	if err != nil {
		return nil, err
	}
	rc.irReq.Model = rc.kiroModelID

	// Initialize metadata if needed (single check)
	if rc.irReq.Metadata == nil {
		rc.irReq.Metadata = make(map[string]any)
	}

	// Set profile ARN (only for social auth; AWS SSO OIDC must NOT send it)
	if arn := getMetaString(rc.auth.Metadata, "profile_arn", "profileArn"); arn != "" {
		if shouldSendProfileArn(rc.auth) {
			rc.irReq.Metadata["profileArn"] = arn
		}
	}

	// Set origin for quota management
	rc.irReq.Metadata["origin"] = rc.origin

	// Inject agentic system prompt if needed
	if rc.isAgentic {
		e.injectAgenticPrompt(rc.irReq)
	}

	// Determine API region (do NOT use OIDC region for API calls)
	rc.apiRegion = determineKiroAPIRegion(rc.auth)
	if rc.apiRegion == "" {
		rc.apiRegion = kiroDefaultRegionV2
	}

	rc.kiroBody, err = (&from_ir.KiroProvider{}).ConvertRequest(rc.irReq)
	return rc, err
}

func (e *KiroExecutorV2) injectAgenticPrompt(req *ir.UnifiedChatRequest) {
	// Find or create system message
	for i, msg := range req.Messages {
		if msg.Role == ir.RoleSystem {
			// Append to existing system message
			for j, part := range msg.Content {
				if part.Type == ir.ContentTypeText {
					req.Messages[i].Content[j].Text += "\n" + kiroAgenticSystemPrompt
					return
				}
			}
		}
	}
	// No system message found, prepend one
	systemMsg := ir.Message{
		Role: ir.RoleSystem,
		Content: []ir.ContentPart{{
			Type: ir.ContentTypeText,
			Text: kiroAgenticSystemPrompt,
		}},
	}
	req.Messages = append([]ir.Message{systemMsg}, req.Messages...)
}

func (e *KiroExecutorV2) buildHTTPRequest(rc *requestContext) (*http.Request, error) {
	url := fmt.Sprintf(kiroPrimaryURLTemplateV2, rc.apiRegion)
	if rc.useFallback {
		url = fmt.Sprintf(kiroFallbackURLTemplateV2, rc.apiRegion)
	}

	httpReq, err := http.NewRequestWithContext(rc.ctx, http.MethodPost, url, bytes.NewReader(rc.kiroBody))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", kiroContentTypeV2)
	httpReq.Header.Set("Accept", kiroAcceptStreamV2)

	// Q endpoint does NOT require X-Amz-Target.
	if rc.useFallback {
		httpReq.Header.Set("X-Amz-Target", kiroTargetV2)
	}

	// Kiro-specific headers (match upstream behavior)
	httpReq.Header.Set("x-amzn-kiro-agent-mode", kiroAgentModeHeaderV2)
	httpReq.Header.Set("x-amzn-codewhisperer-optout", "true")

	// Apply dynamic fingerprint (IDC gets dynamic UA, others get static)
	applyDynamicFingerprintV2(httpReq, rc.auth)

	if rc.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+rc.token)
	}
	return httpReq, nil
}

func (e *KiroExecutorV2) Execute(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Check for pure web_search request — route to MCP endpoint
	if kiroclaude.HasWebSearchTool(req.Payload) {
		log.Infof("kiro-v2: detected pure web_search request (non-stream), routing to MCP endpoint")
		return e.handleWebSearchV2(ctx, auth, req, opts)
	}

	rc, err := e.prepareRequest(ctx, auth, req, opts.SourceFormat.String())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	// Rate limiting & cooldown pre-check
	rateLimiter := kiroauth.GetGlobalRateLimiter()
	cooldownMgr := kiroauth.GetGlobalCooldownManager()
	if cooldownMgr.IsInCooldown(rc.tokenKey) {
		remaining := cooldownMgr.GetRemainingCooldown(rc.tokenKey)
		reason := cooldownMgr.GetCooldownReason(rc.tokenKey)
		log.Warnf("kiro-v2: token %s in cooldown (reason: %s), remaining: %v", rc.tokenKey, reason, remaining)
		return cliproxyexecutor.Response{}, fmt.Errorf("kiro: token is in cooldown for %v (reason: %s)", remaining, reason)
	}
	rateLimiter.WaitForToken(rc.tokenKey)

	return e.executeWithRetry(rc)
}

func (e *KiroExecutorV2) executeWithRetry(rc *requestContext) (cliproxyexecutor.Response, error) {
	var lastErr error
	currentOrigin := rc.origin
	initialOrigin := rc.origin
	useFallbackURL := false
	rateLimiter := kiroauth.GetGlobalRateLimiter()
	cooldownMgr := kiroauth.GetGlobalCooldownManager()
	maxAttempts := kiroMaxRetries + kiroSocketMaxRetriesV2 // Combined retry budget

	for attempt := 0; attempt <= maxAttempts; attempt++ {
		// Update origin in request body if changed from initial
		if currentOrigin != initialOrigin {
			rc.irReq.Metadata["origin"] = currentOrigin
			var err error
			rc.kiroBody, err = (&from_ir.KiroProvider{}).ConvertRequest(rc.irReq)
			if err != nil {
				return cliproxyexecutor.Response{}, err
			}
			initialOrigin = currentOrigin
		}

		rc.useFallback = useFallbackURL
		httpReq, err := e.buildHTTPRequest(rc)
		if err != nil {
			return cliproxyexecutor.Response{}, err
		}

		client := newPooledHTTPClientV2(rc.ctx, e.cfg, rc.auth, kiroRequestTimeout)
		resp, err := client.Do(httpReq)
		if err != nil {
			if isRetryableErrorV2(err) && attempt < maxAttempts {
				if !useFallbackURL {
					log.Warnf("kiro-v2: primary endpoint failed (retryable), trying fallback: %v", err)
					useFallbackURL = true
				} else {
					delay := calculateRetryDelayV2(attempt)
					log.Warnf("kiro-v2: network error (attempt %d/%d), retrying in %v: %v", attempt+1, maxAttempts, delay, err)
					time.Sleep(delay)
				}
				lastErr = err
				continue
			}
			return cliproxyexecutor.Response{}, err
		}

		// Handle 429 (quota exhausted) - switch origin or enter cooldown
		if resp.StatusCode == http.StatusTooManyRequests {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			rateLimiter.MarkTokenFailed(rc.tokenKey)

			if currentOrigin == "CLI" {
				log.Warnf("kiro-v2: CLI quota exhausted (429), switching to AI_EDITOR")
				currentOrigin = "AI_EDITOR"
				continue
			}
			// Both origins exhausted — enter cooldown
			cooldownMgr.SetCooldown(rc.tokenKey, 60*time.Second, "quota_exhausted_429")
			lastErr = fmt.Errorf("quota exhausted: %s", string(body))
			continue
		}

		// Handle 401/403 - refresh token and retry
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// Detect suspended/disabled account
			bodyStr := strings.ToLower(string(body))
			if strings.Contains(bodyStr, "suspended") || strings.Contains(bodyStr, "disabled") {
				cooldownMgr.SetCooldown(rc.tokenKey, 5*time.Minute, "account_suspended")
				return cliproxyexecutor.Response{}, fmt.Errorf("account suspended/disabled: %s", string(body))
			}

			if attempt < maxAttempts {
				log.Warnf("kiro-v2: auth error %d, refreshing token (attempt %d/%d)", resp.StatusCode, attempt+1, maxAttempts)
				refreshedAuth, refreshErr := e.Refresh(rc.ctx, rc.auth)
				if refreshErr != nil {
					lastErr = fmt.Errorf("token refresh failed: %w", refreshErr)
					continue
				}
				if saver, ok := refreshedAuth.Storage.(interface{ Save() error }); ok {
					if err := saver.Save(); err != nil {
						log.Warnf("kiro-v2: failed to persist refreshed auth: %v", err)
					}
				}
				rc.auth = refreshedAuth
				rc.token = getMetaString(refreshedAuth.Metadata, "access_token", "accessToken")
				rc.tokenKey = getTokenKeyV2(refreshedAuth)
				continue
			}
			return cliproxyexecutor.Response{}, fmt.Errorf("auth error %d: %s", resp.StatusCode, string(body))
		}

		// Handle 5xx - retryable server errors
		if retryableHTTPStatusCodesV2[resp.StatusCode] && attempt < maxAttempts {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			delay := calculateRetryDelayV2(attempt)
			log.Warnf("kiro-v2: server error %d (attempt %d/%d), retrying in %v: %s", resp.StatusCode, attempt+1, maxAttempts, delay, string(body))
			lastErr = fmt.Errorf("server error %d: %s", resp.StatusCode, string(body))
			time.Sleep(delay)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Warnf("kiro-v2: upstream error %d for model %s: %s", resp.StatusCode, rc.req.Model, string(body))
			return cliproxyexecutor.Response{}, fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(body))
		}

		// Success — mark token as successful
		rateLimiter.MarkTokenSuccess(rc.tokenKey)

		defer resp.Body.Close()
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/vnd.amazon.eventstream") {
			return e.handleEventStreamResponse(resp.Body, rc.req.Model, rc.sourceFormat)
		}
		return e.handleJSONResponse(resp.Body, rc.req.Model, rc.sourceFormat)
	}

	if lastErr != nil {
		return cliproxyexecutor.Response{}, lastErr
	}
	return cliproxyexecutor.Response{}, fmt.Errorf("kiro: max retries exceeded")
}

func (e *KiroExecutorV2) handleEventStreamResponse(body io.ReadCloser, model string, sourceFormat string) (cliproxyexecutor.Response, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, 52_428_800) // 50MB buffer to handle large AWS EventStream frames
	scanner.Split(splitAWSEventStream)
	state := to_ir.NewKiroStreamState()
	// Use model registry context length when available (fallback remains 200k)
	if info := registry.LookupModelInfo("kiro-"+strings.ReplaceAll(model, ".", "-"), "kiro"); info != nil && info.ContextLength > 0 {
		state.SetContextWindowTokens(info.ContextLength)
	}
	for scanner.Scan() {
		payload, err := parseEventPayload(scanner.Bytes())
		if err == nil {
			state.ProcessChunk(payload)
		}
	}

	msg := &ir.Message{Role: ir.RoleAssistant, ToolCalls: state.ToolCalls}
	if state.AccumulatedContent != "" {
		msg.Content = append(msg.Content, ir.ContentPart{Type: ir.ContentTypeText, Text: state.AccumulatedContent})
	}

	messageID := "chatcmpl-" + uuid.New().String()
	converted, err := e.convertToSourceFormat([]ir.Message{*msg}, nil, model, messageID, sourceFormat)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: converted}, nil
}

func (e *KiroExecutorV2) handleJSONResponse(body io.ReadCloser, model string, sourceFormat string) (cliproxyexecutor.Response, error) {
	rawData, err := io.ReadAll(body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	messages, usage, err := to_ir.ParseKiroResponse(rawData)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	messageID := "chatcmpl-" + uuid.New().String()
	converted, err := e.convertToSourceFormat(messages, usage, model, messageID, sourceFormat)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	return cliproxyexecutor.Response{Payload: converted}, nil
}

// convertToSourceFormat converts IR messages to the appropriate response format based on sourceFormat.
func (e *KiroExecutorV2) convertToSourceFormat(messages []ir.Message, usage *ir.Usage, model, messageID, sourceFormat string) ([]byte, error) {
	switch sourceFormat {
	case "claude":
		return from_ir.ToClaudeResponse(messages, usage, model, messageID)
	default:
		// Default to OpenAI format
		return from_ir.ToOpenAIChatCompletion(messages, usage, model, messageID)
	}
}

func (e *KiroExecutorV2) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (<-chan cliproxyexecutor.StreamChunk, error) {
	// Check for pure web_search request — route to MCP endpoint
	if kiroclaude.HasWebSearchTool(req.Payload) {
		log.Infof("kiro-v2: detected pure web_search request, routing to MCP endpoint")
		return e.handleWebSearchStreamV2(ctx, auth, req, opts)
	}

	rc, err := e.prepareRequest(ctx, auth, req, opts.SourceFormat.String())
	if err != nil {
		return nil, err
	}

	// Rate limiting & cooldown pre-check
	rateLimiter := kiroauth.GetGlobalRateLimiter()
	cooldownMgr := kiroauth.GetGlobalCooldownManager()
	if cooldownMgr.IsInCooldown(rc.tokenKey) {
		remaining := cooldownMgr.GetRemainingCooldown(rc.tokenKey)
		reason := cooldownMgr.GetCooldownReason(rc.tokenKey)
		log.Warnf("kiro-v2: token %s in cooldown (reason: %s), remaining: %v", rc.tokenKey, reason, remaining)
		return nil, fmt.Errorf("kiro: token is in cooldown for %v (reason: %s)", remaining, reason)
	}
	rateLimiter.WaitForToken(rc.tokenKey)

	return e.executeStreamWithRetry(rc)
}

func (e *KiroExecutorV2) executeStreamWithRetry(rc *requestContext) (<-chan cliproxyexecutor.StreamChunk, error) {
	var lastErr error
	currentOrigin := rc.origin
	initialOrigin := rc.origin
	useFallbackURL := false
	rateLimiter := kiroauth.GetGlobalRateLimiter()
	cooldownMgr := kiroauth.GetGlobalCooldownManager()
	maxAttempts := kiroMaxRetries + kiroSocketMaxRetriesV2

	for attempt := 0; attempt <= maxAttempts; attempt++ {
		// Update origin in request body if changed from initial
		if currentOrigin != initialOrigin {
			rc.irReq.Metadata["origin"] = currentOrigin
			var err error
			rc.kiroBody, err = (&from_ir.KiroProvider{}).ConvertRequest(rc.irReq)
			if err != nil {
				return nil, err
			}
			initialOrigin = currentOrigin
		}

		rc.useFallback = useFallbackURL
		httpReq, err := e.buildHTTPRequest(rc)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Connection", "keep-alive")

		client := newPooledHTTPClientV2(rc.ctx, e.cfg, rc.auth, 0)
		resp, err := client.Do(httpReq)
		if err != nil {
			if isRetryableErrorV2(err) && attempt < maxAttempts {
				if !useFallbackURL {
					log.Warnf("kiro-v2: stream primary endpoint failed (retryable), trying fallback: %v", err)
					useFallbackURL = true
				} else {
					delay := calculateRetryDelayV2(attempt)
					log.Warnf("kiro-v2: stream network error (attempt %d/%d), retrying in %v: %v", attempt+1, maxAttempts, delay, err)
					time.Sleep(delay)
				}
				lastErr = err
				continue
			}
			return nil, err
		}

		// Handle 429 (quota exhausted)
		if resp.StatusCode == http.StatusTooManyRequests {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			rateLimiter.MarkTokenFailed(rc.tokenKey)

			if currentOrigin == "CLI" {
				log.Warnf("kiro-v2: stream CLI quota exhausted (429), switching to AI_EDITOR")
				currentOrigin = "AI_EDITOR"
				continue
			}
			cooldownMgr.SetCooldown(rc.tokenKey, 60*time.Second, "quota_exhausted_429")
			lastErr = fmt.Errorf("quota exhausted: %s", string(body))
			continue
		}

		// Handle 401/403
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			bodyStr := strings.ToLower(string(body))
			if strings.Contains(bodyStr, "suspended") || strings.Contains(bodyStr, "disabled") {
				cooldownMgr.SetCooldown(rc.tokenKey, 5*time.Minute, "account_suspended")
				return nil, fmt.Errorf("account suspended/disabled: %s", string(body))
			}

			if attempt < maxAttempts {
				log.Warnf("kiro-v2: stream auth error %d, refreshing token", resp.StatusCode)
				refreshedAuth, refreshErr := e.Refresh(rc.ctx, rc.auth)
				if refreshErr != nil {
					lastErr = fmt.Errorf("token refresh failed: %w", refreshErr)
					continue
				}
				if saver, ok := refreshedAuth.Storage.(interface{ Save() error }); ok {
					if err := saver.Save(); err != nil {
						log.Warnf("kiro-v2: failed to persist refreshed auth: %v", err)
					}
				}
				rc.auth = refreshedAuth
				rc.token = getMetaString(refreshedAuth.Metadata, "access_token", "accessToken")
				rc.tokenKey = getTokenKeyV2(refreshedAuth)
				continue
			}
			return nil, fmt.Errorf("auth error %d: %s", resp.StatusCode, string(body))
		}

		// Handle 5xx - retryable server errors
		if retryableHTTPStatusCodesV2[resp.StatusCode] && attempt < maxAttempts {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			delay := calculateRetryDelayV2(attempt)
			log.Warnf("kiro-v2: stream server error %d (attempt %d/%d), retrying in %v", resp.StatusCode, attempt+1, maxAttempts, delay)
			lastErr = fmt.Errorf("server error %d: %s", resp.StatusCode, string(body))
			time.Sleep(delay)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			log.Warnf("kiro-v2: stream upstream error %d for model %s: %s", resp.StatusCode, rc.req.Model, string(body))
			return nil, fmt.Errorf("upstream error %d: %s", resp.StatusCode, string(body))
		}

		// Success — mark token as successful
		rateLimiter.MarkTokenSuccess(rc.tokenKey)

		out := make(chan cliproxyexecutor.StreamChunk)
		go e.processStream(resp, rc.req.Model, rc.req.Payload, rc.sourceFormat, out)
		return out, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("kiro: max retries exceeded for stream")
}

func (e *KiroExecutorV2) processStream(resp *http.Response, model string, requestPayload []byte, sourceFormat string, out chan<- cliproxyexecutor.StreamChunk) {
	defer resp.Body.Close()
	defer close(out)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(nil, 52_428_800) // 50MB buffer to handle large AWS EventStream frames
	scanner.Split(splitAWSEventStream)
	state := to_ir.NewKiroStreamState()
	// Use model registry context length when available (fallback remains 200k)
	if info := registry.LookupModelInfo("kiro-"+strings.ReplaceAll(model, ".", "-"), "kiro"); info != nil && info.ContextLength > 0 {
		state.SetContextWindowTokens(info.ContextLength)
	}
	messageID := "chatcmpl-" + uuid.New().String()
	idx := 0

	// Create Claude stream state if needed
	var claudeState *from_ir.ClaudeStreamState
	if sourceFormat == "claude" {
		claudeState = from_ir.NewClaudeStreamState()
	}

	for scanner.Scan() {
		payload, err := parseEventPayload(scanner.Bytes())
		if err != nil {
			continue
		}
		events, _ := state.ProcessChunk(payload)
		for _, ev := range events {
			chunk, err := e.convertStreamChunkToSourceFormat(ev, model, messageID, idx, sourceFormat, claudeState)
			if err == nil && len(chunk) > 0 {
				out <- cliproxyexecutor.StreamChunk{Payload: chunk}
				idx++
			}
		}
	}

	// Build finish event with usage
	finish := ir.UnifiedEvent{
		Type:         ir.EventTypeFinish,
		FinishReason: state.DetermineFinishReason(),
		Usage:        state.Usage,
	}

	// Fallback: estimate tokens if API didn't return them
	if finish.Usage == nil || finish.Usage.TotalTokens == 0 {
		// Try to use real tokenizer for accurate prompt token count
		var promptTokens int64
		if enc, err := tokenizerForModel("claude"); err == nil {
			if count, err := countOpenAIChatTokens(enc, requestPayload); err == nil {
				promptTokens = count
			}
		}
		// Fallback for prompt tokens if tokenizer failed
		if promptTokens == 0 {
			promptTokens = int64(len(requestPayload) / 4)
			if promptTokens == 0 && len(requestPayload) > 0 {
				promptTokens = 1
			}
		}

		// Estimate completion tokens from accumulated content
		var completionTokens int
		if enc, err := tokenizerForModel("claude"); err == nil {
			if count, err := enc.Count(state.AccumulatedContent); err == nil {
				completionTokens = count
			}
		}
		// Fallback for completion tokens
		if completionTokens == 0 {
			completionTokens = len(state.AccumulatedContent) / 4
			if completionTokens == 0 && len(state.AccumulatedContent) > 0 {
				completionTokens = 1
			}
		}

		finish.Usage = &ir.Usage{
			PromptTokens:     int(promptTokens),
			CompletionTokens: completionTokens,
			TotalTokens:      int(promptTokens) + completionTokens,
		}
	}

	chunk, err := e.convertStreamChunkToSourceFormat(finish, model, messageID, idx, sourceFormat, claudeState)
	if err == nil && len(chunk) > 0 {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
}

// convertStreamChunkToSourceFormat converts a streaming event to the appropriate format.
func (e *KiroExecutorV2) convertStreamChunkToSourceFormat(ev ir.UnifiedEvent, model, messageID string, idx int, sourceFormat string, claudeState *from_ir.ClaudeStreamState) ([]byte, error) {
	switch sourceFormat {
	case "claude":
		return from_ir.ToClaudeSSE(ev, model, messageID, claudeState)
	default:
		return from_ir.ToOpenAIChunk(ev, model, messageID, idx)
	}
}

func (e *KiroExecutorV2) CountTokens(ctx context.Context, auth *coreauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Kiro uses Claude models, so we use the O200kBase tokenizer (good approximation for Claude)
	enc, err := tokenizerForModel("claude") // Will use O200kBase fallback
	if err != nil {
		// Fallback to heuristic if tokenizer fails
		estTokens := len(req.Payload) / 4
		if estTokens == 0 && len(req.Payload) > 0 {
			estTokens = 1
		}
		return cliproxyexecutor.Response{Payload: []byte(fmt.Sprintf(`{"total_tokens": %d}`, estTokens))}, nil
	}

	count, err := countOpenAIChatTokens(enc, req.Payload)
	if err != nil {
		// Fallback to heuristic if counting fails
		estTokens := len(req.Payload) / 4
		if estTokens == 0 && len(req.Payload) > 0 {
			estTokens = 1
		}
		return cliproxyexecutor.Response{Payload: []byte(fmt.Sprintf(`{"total_tokens": %d}`, estTokens))}, nil
	}

	usageJSON := buildOpenAIUsageJSON(count)
	return cliproxyexecutor.Response{Payload: usageJSON}, nil
}

// Helper functions

func getMetaString(meta map[string]interface{}, keys ...string) string {
	if meta == nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := meta[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func determineKiroAPIRegion(auth *coreauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}

	// Priority 1: explicit override
	if r, ok := auth.Metadata["api_region"].(string); ok {
		r = strings.TrimSpace(r)
		if r != "" {
			return r
		}
	}

	// Priority 2: extract from profile_arn
	if arn, ok := auth.Metadata["profile_arn"].(string); ok {
		if r := extractRegionFromProfileARNV2(strings.TrimSpace(arn)); r != "" {
			return r
		}
	}

	// IMPORTANT: Do NOT use auth.Metadata["region"] here.
	// That field may refer to OIDC/refresh region and can break API calls.
	return ""
}

func extractRegionFromProfileARNV2(profileArn string) string {
	if profileArn == "" {
		return ""
	}
	parts := strings.Split(profileArn, ":")
	if len(parts) >= 4 && parts[3] != "" {
		return parts[3]
	}
	return ""
}

func shouldSendProfileArn(auth *coreauth.Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return true
	}

	// Check 1: auth_method field
	if authMethod, ok := auth.Metadata["auth_method"].(string); ok {
		s := strings.ToLower(strings.TrimSpace(authMethod))
		if s == "builder-id" || s == "idc" {
			return false
		}
	}

	// Check 2: auth_type field
	if authType, ok := auth.Metadata["auth_type"].(string); ok {
		s := strings.ToLower(strings.TrimSpace(authType))
		if s == "aws_sso_oidc" {
			return false
		}
	}

	// Check 3: client_id + client_secret presence
	_, hasClientID := auth.Metadata["client_id"].(string)
	_, hasClientSecret := auth.Metadata["client_secret"].(string)
	if hasClientID && hasClientSecret {
		return false
	}

	return true
}

func parseTokenExpiry(meta map[string]interface{}) time.Time {
	if meta == nil {
		return time.Time{}
	}
	for _, key := range []string{"expires_at", "expiresAt"} {
		if exp, ok := meta[key].(string); ok && exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				return t
			}
		}
	}
	return time.Time{}
}

func mapModelID(model string) string {
	// Strip -agentic suffix for API call (it's only used for system prompt injection)
	baseModel := strings.TrimSuffix(model, "-agentic")

	// Check explicit mapping (mainly for amazonq- prefix)
	if mapped, ok := kiroModelMapping[baseModel]; ok {
		return mapped
	}

	// Strip amazonq- prefix if present (fallback)
	if strings.HasPrefix(baseModel, "amazonq-") {
		return strings.TrimPrefix(baseModel, "amazonq-")
	}

	// Return as-is (native Kiro format: auto, claude-opus-4.5, etc.)
	return baseModel
}

func splitAWSEventStream(data []byte, atEOF bool) (int, []byte, error) {
	if len(data) < 4 {
		if atEOF && len(data) > 0 {
			return len(data), nil, nil
		}
		return 0, nil, nil
	}
	totalLen := int(binary.BigEndian.Uint32(data[0:4]))
	if totalLen < 16 || totalLen > 16*1024*1024 {
		return 1, nil, nil
	}
	if len(data) < totalLen {
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	}
	return totalLen, data[:totalLen], nil
}

func parseEventPayload(frame []byte) ([]byte, error) {
	if len(frame) < 16 {
		return nil, fmt.Errorf("short frame")
	}
	if binary.BigEndian.Uint32(frame[8:12]) != crc32.ChecksumIEEE(frame[0:8]) {
		return nil, fmt.Errorf("crc mismatch")
	}
	totalLen := int(binary.BigEndian.Uint32(frame[0:4]))
	headersLen := int(binary.BigEndian.Uint32(frame[4:8]))
	start, end := 12+headersLen, totalLen-4
	if start >= end || end > len(frame) {
		return nil, fmt.Errorf("bounds")
	}
	return frame[start:end], nil
}

const maxWebSearchIterationsV2 = 5

// handleWebSearchV2 handles pure web_search requests for the non-streaming Execute path.
func (e *KiroExecutorV2) handleWebSearchV2(
	ctx context.Context,
	auth *coreauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (cliproxyexecutor.Response, error) {
	query := kiroclaude.ExtractSearchQuery(req.Payload)
	if query == "" {
		log.Warnf("kiro-v2/websearch: failed to extract search query, falling back to normal Execute")
		return e.Execute(ctx, auth, withoutWebSearchV2(req), opts)
	}

	region := e.getAPIRegionV2(auth)
	mcpEndpoint := fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)

	token, updatedAuth, err := e.ensureValidToken(ctx, auth)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("kiro-v2/websearch: token error: %w", err)
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	fp, authAttrs := e.getMCPAuthContextV2(auth, token)
	httpClient := e.newMCPHTTPClientV2(ctx, auth)
	kiroclaude.FetchToolDescription(mcpEndpoint, token, httpClient, fp, authAttrs)

	_, mcpRequest := kiroclaude.CreateMcpRequest(query)
	handler := kiroclaude.NewWebSearchHandler(mcpEndpoint, token, httpClient, fp, authAttrs)
	mcpResponse, mcpErr := handler.CallMcpAPI(mcpRequest)

	var searchResults *kiroclaude.WebSearchResults
	if mcpErr != nil {
		log.Warnf("kiro-v2/websearch: MCP API call failed: %v, continuing with empty results", mcpErr)
	} else {
		searchResults = kiroclaude.ParseSearchResults(mcpResponse)
	}

	resultCount := 0
	if searchResults != nil {
		resultCount = len(searchResults.Results)
	}
	log.Infof("kiro-v2/websearch: non-stream: got %d results for query: %s", resultCount, query)

	toolUseID := fmt.Sprintf("srvtoolu_%s", kiroclaude.GenerateToolUseID())
	modifiedPayload, err := kiroclaude.InjectToolResultsClaude(bytes.Clone(req.Payload), toolUseID, query, searchResults)
	if err != nil {
		log.Warnf("kiro-v2/websearch: failed to inject tool results: %v, falling back", err)
		return e.Execute(ctx, auth, withoutWebSearchV2(req), opts)
	}

	modifiedPayload, _ = kiroclaude.StripWebSearchTool(modifiedPayload)
	modifiedReq := req
	modifiedReq.Payload = modifiedPayload
	resp, err := e.Execute(ctx, auth, modifiedReq, opts)
	if err != nil {
		return resp, err
	}

	indicators := []kiroclaude.SearchIndicator{{
		ToolUseID: toolUseID,
		Query:     query,
		Results:   searchResults,
	}}
	if injected, injErr := kiroclaude.InjectSearchIndicatorsInResponse(resp.Payload, indicators); injErr == nil {
		resp.Payload = injected
	} else {
		log.Warnf("kiro-v2/websearch: failed to inject search indicators: %v", injErr)
	}

	return resp, nil
}

// handleWebSearchStreamV2 handles pure web_search requests for the streaming ExecuteStream path.
// Implements a search loop: MCP search → inject results → call Kiro API → analyze → loop or return.
func (e *KiroExecutorV2) handleWebSearchStreamV2(
	ctx context.Context,
	auth *coreauth.Auth,
	req cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
) (<-chan cliproxyexecutor.StreamChunk, error) {
	query := kiroclaude.ExtractSearchQuery(req.Payload)
	if query == "" {
		log.Warnf("kiro-v2/websearch: failed to extract search query, falling back to normal stream")
		return e.ExecuteStream(ctx, auth, withoutWebSearchV2(req), opts)
	}

	region := e.getAPIRegionV2(auth)
	mcpEndpoint := fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)

	token, updatedAuth, err := e.ensureValidToken(ctx, auth)
	if err != nil {
		return nil, fmt.Errorf("kiro-v2/websearch: token error: %w", err)
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	fp, authAttrs := e.getMCPAuthContextV2(auth, token)
	httpClient := e.newMCPHTTPClientV2(ctx, auth)
	kiroclaude.FetchToolDescription(mcpEndpoint, token, httpClient, fp, authAttrs)

	out := make(chan cliproxyexecutor.StreamChunk)

	go func() {
		defer close(out)

		msgStart := kiroclaude.SseEvent{
			Event: "message_start",
			Data: map[string]interface{}{
				"type": "message_start",
				"message": map[string]interface{}{
					"id":            kiroclaude.GenerateMessageID(),
					"type":          "message",
					"role":          "assistant",
					"model":         req.Model,
					"content":       []interface{}{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage": map[string]interface{}{
						"input_tokens":                len(req.Payload) / 4,
						"output_tokens":               0,
						"cache_creation_input_tokens": 0,
						"cache_read_input_tokens":     0,
					},
				},
			},
		}
		select {
		case <-ctx.Done():
			return
		case out <- cliproxyexecutor.StreamChunk{Payload: []byte(msgStart.ToSSEString())}:
		}

		contentBlockIndex := 0
		currentQuery := query
		currentToolUseID := fmt.Sprintf("srvtoolu_%s", kiroclaude.GenerateToolUseID())

		simplifiedPayload, simplifyErr := kiroclaude.ReplaceWebSearchToolDescription(bytes.Clone(req.Payload))
		if simplifyErr != nil {
			log.Warnf("kiro-v2/websearch: failed to simplify web_search tool: %v", simplifyErr)
			simplifiedPayload = bytes.Clone(req.Payload)
		}
		currentPayload := simplifiedPayload

		for iteration := 0; iteration < maxWebSearchIterationsV2; iteration++ {
			log.Infof("kiro-v2/websearch: iteration %d/%d — query: %s",
				iteration+1, maxWebSearchIterationsV2, currentQuery)

			_, mcpRequest := kiroclaude.CreateMcpRequest(currentQuery)
			handler := kiroclaude.NewWebSearchHandler(mcpEndpoint, token, httpClient, fp, authAttrs)
			mcpResponse, mcpErr := handler.CallMcpAPI(mcpRequest)

			var searchResults *kiroclaude.WebSearchResults
			if mcpErr != nil {
				log.Warnf("kiro-v2/websearch: MCP failed: %v", mcpErr)
			} else {
				searchResults = kiroclaude.ParseSearchResults(mcpResponse)
			}

			searchEvents := kiroclaude.GenerateSearchIndicatorEvents(currentQuery, currentToolUseID, searchResults, contentBlockIndex)
			for _, event := range searchEvents {
				select {
				case <-ctx.Done():
					return
				case out <- cliproxyexecutor.StreamChunk{Payload: []byte(event.ToSSEString())}:
				}
			}
			contentBlockIndex += 2

			var injectErr error
			currentPayload, injectErr = kiroclaude.InjectToolResultsClaude(currentPayload, currentToolUseID, currentQuery, searchResults)
			if injectErr != nil {
				log.Warnf("kiro-v2/websearch: inject failed: %v", injectErr)
				e.sendFallbackTextV2(ctx, out, contentBlockIndex, currentQuery, searchResults)
				break
			}

			kiroChunks, kiroErr := e.callKiroAndBufferV2(ctx, auth, req, opts, currentPayload)
			if kiroErr != nil {
				log.Warnf("kiro-v2/websearch: Kiro API failed at iteration %d: %v", iteration+1, kiroErr)
				e.sendFallbackTextV2(ctx, out, contentBlockIndex, currentQuery, searchResults)
				break
			}

			analysis := kiroclaude.AnalyzeBufferedStream(kiroChunks)
			log.Infof("kiro-v2/websearch: iteration %d — stop: %s, has_tool_use: %v, query: %s",
				iteration+1, analysis.StopReason, analysis.HasWebSearchToolUse, analysis.WebSearchQuery)

			if analysis.HasWebSearchToolUse && analysis.WebSearchQuery != "" && iteration+1 < maxWebSearchIterationsV2 {
				filtered := kiroclaude.FilterChunksForClient(kiroChunks, analysis.WebSearchToolUseIndex, contentBlockIndex)
				for _, chunk := range filtered {
					select {
					case <-ctx.Done():
						return
					case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
					}
				}
				currentQuery = analysis.WebSearchQuery
				currentToolUseID = analysis.WebSearchToolUseId
				continue
			}

			for _, chunk := range kiroChunks {
				if contentBlockIndex > 0 && len(chunk) > 0 {
					adjusted, shouldFwd := kiroclaude.AdjustSSEChunk(chunk, contentBlockIndex)
					if !shouldFwd {
						continue
					}
					select {
					case <-ctx.Done():
						return
					case out <- cliproxyexecutor.StreamChunk{Payload: adjusted}:
					}
				} else {
					select {
					case <-ctx.Done():
						return
					case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
					}
				}
			}
			log.Infof("kiro-v2/websearch: completed after %d iteration(s)", iteration+1)
			return
		}

		log.Warnf("kiro-v2/websearch: reached max iterations (%d)", maxWebSearchIterationsV2)
	}()

	return out, nil
}

// callKiroAndBufferV2 calls the Kiro API via the V2 streaming path and buffers all chunks.
func (e *KiroExecutorV2) callKiroAndBufferV2(
	ctx context.Context,
	auth *coreauth.Auth,
	originalReq cliproxyexecutor.Request,
	opts cliproxyexecutor.Options,
	claudePayload []byte,
) ([][]byte, error) {
	strippedPayload, _ := kiroclaude.StripWebSearchTool(claudePayload)

	modifiedReq := originalReq
	modifiedReq.Payload = strippedPayload

	rc, err := e.prepareRequest(ctx, auth, modifiedReq, opts.SourceFormat.String())
	if err != nil {
		return nil, fmt.Errorf("kiro-v2/websearch: prepare failed: %w", err)
	}

	rc.sourceFormat = "claude"

	stream, err := e.executeStreamWithRetry(rc)
	if err != nil {
		return nil, err
	}

	var chunks [][]byte
	for chunk := range stream {
		if chunk.Err != nil {
			return chunks, chunk.Err
		}
		if len(chunk.Payload) > 0 {
			chunks = append(chunks, bytes.Clone(chunk.Payload))
		}
	}

	log.Debugf("kiro-v2/websearch: buffered %d chunks", len(chunks))
	return chunks, nil
}

// sendFallbackTextV2 sends a text summary when the Kiro API fails during the search loop.
func (e *KiroExecutorV2) sendFallbackTextV2(
	ctx context.Context,
	out chan<- cliproxyexecutor.StreamChunk,
	contentBlockIndex int,
	query string,
	searchResults *kiroclaude.WebSearchResults,
) {
	summary := kiroclaude.FormatSearchContextPrompt(query, searchResults)

	events := []kiroclaude.SseEvent{
		{
			Event: "content_block_start",
			Data: map[string]interface{}{
				"type":  "content_block_start",
				"index": contentBlockIndex,
				"content_block": map[string]interface{}{
					"type": "text",
					"text": "",
				},
			},
		},
		{
			Event: "content_block_delta",
			Data: map[string]interface{}{
				"type":  "content_block_delta",
				"index": contentBlockIndex,
				"delta": map[string]interface{}{
					"type": "text_delta",
					"text": summary,
				},
			},
		},
		{
			Event: "content_block_stop",
			Data: map[string]interface{}{
				"type":  "content_block_stop",
				"index": contentBlockIndex,
			},
		},
		{
			Event: "message_delta",
			Data: map[string]interface{}{
				"type": "message_delta",
				"delta": map[string]interface{}{
					"stop_reason":   "end_turn",
					"stop_sequence": nil,
				},
				"usage": map[string]interface{}{
					"output_tokens": len(summary) / 4,
				},
			},
		},
		{
			Event: "message_stop",
			Data: map[string]interface{}{
				"type": "message_stop",
			},
		},
	}

	for _, event := range events {
		select {
		case <-ctx.Done():
			return
		case out <- cliproxyexecutor.StreamChunk{Payload: []byte(event.ToSSEString())}:
		}
	}
}

// withoutWebSearchV2 returns a copy of the request with web_search tool stripped.
func withoutWebSearchV2(req cliproxyexecutor.Request) cliproxyexecutor.Request {
	stripped, err := kiroclaude.StripWebSearchTool(bytes.Clone(req.Payload))
	if err != nil {
		return req
	}
	modified := req
	modified.Payload = stripped
	return modified
}

// getAPIRegionV2 returns the Kiro API region from auth metadata.
func (e *KiroExecutorV2) getAPIRegionV2(auth *coreauth.Auth) string {
	if region := determineKiroAPIRegion(auth); region != "" {
		return region
	}
	return kiroDefaultRegionV2
}

// getMCPAuthContextV2 extracts fingerprint and auth attributes for MCP calls.
func (e *KiroExecutorV2) getMCPAuthContextV2(auth *coreauth.Auth, token string) (*kiroauth.Fingerprint, map[string]string) {
	tokenKey := ""
	if auth != nil {
		tokenKey = auth.ID
	}
	fp := getGlobalFingerprintManager().GetFingerprint(tokenKey)

	var authAttrs map[string]string
	if auth != nil {
		authAttrs = auth.Attributes
	}
	return fp, authAttrs
}

// newMCPHTTPClientV2 creates an HTTP client for MCP API calls.
func (e *KiroExecutorV2) newMCPHTTPClientV2(ctx context.Context, auth *coreauth.Auth) *http.Client {
	return newProxyAwareHTTPClient(ctx, e.cfg, auth, 30*time.Second)
}
