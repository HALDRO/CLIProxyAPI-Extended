package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"

	log "github.com/sirupsen/logrus"
)

const (
	// NOTE: Kilo OpenRouter-compatible API. Similar to OpenAI but proxied by Kilo.
	// Base used in kilocode-main: https://api.kilo.ai/api/openrouter/v1/
	// Kilo endpoint that accepts only /chat/completions.
	kilocodeProxyBaseURL = "https://api.kilo.ai/api/openrouter"
)

type cachedKiloBearerToken struct {
	token     string
	expiresAt time.Time
}

type KiloCodeExecutor struct {
	cfg *config.Config

	mu    sync.RWMutex
	cache map[string]*cachedKiloBearerToken
}

func NewKiloCodeExecutor(cfg *config.Config) *KiloCodeExecutor {
	return &KiloCodeExecutor{cfg: cfg, cache: make(map[string]*cachedKiloBearerToken)}
}

func (e *KiloCodeExecutor) Identifier() string { return "kilocode" }

func (e *KiloCodeExecutor) PrepareRequest(_ *http.Request, _ *cliproxyauth.Auth) error {
	return nil
}

func (e *KiloCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("kilocode executor: request is nil")
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

func (e *KiloCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	bearerToken, baseURL, err := e.ensureBearerToken(ctx, auth)
	if err != nil {
		return resp, err
	}
	if baseURL == "" {
		baseURL = kilocodeProxyBaseURL
	}

	baseModel := req.Model
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")

	// Prepare original payload for payload config comparison
	originalPayload := bytes.Clone(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		originalPayload = bytes.Clone(opts.OriginalRequest)
	}

	originalTranslated, body, err := sdktranslator.TranslateRequestPairE(ctx, from, to, baseModel, bytes.Clone(req.Payload), originalPayload, false)
	if err != nil {
		return resp, err
	}

	requestedModel := payloadRequestedModel(opts, baseModel)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+bearerToken)
	httpReq.Header.Set("User-Agent", "cli-proxy-kilocode")

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}

	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("kilocode executor: close response body error: %v", errClose)
		}
	}()

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}

	appendAPIResponseChunk(ctx, e.cfg, data)
	reporter.publish(ctx, parseOpenAIUsage(data))

	translatedResp, errTranslate := TranslateOpenAIResponseNonStreamForced(from, data, req.Model)
	if errTranslate != nil {
		return resp, fmt.Errorf("kilocode executor: translation failed: %w", errTranslate)
	}
	if translatedResp == nil {
		return resp, fmt.Errorf("kilocode executor: translation returned nil")
	}

	resp = cliproxyexecutor.Response{Payload: translatedResp}
	return resp, nil
}

func (e *KiloCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	bearerToken, baseURL, err := e.ensureBearerToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	if baseURL == "" {
		baseURL = kilocodeProxyBaseURL
	}

	baseModel := req.Model
	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")

	originalPayload := bytes.Clone(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		originalPayload = bytes.Clone(opts.OriginalRequest)
	}

	originalTranslated, body, err := sdktranslator.TranslateRequestPairE(ctx, from, to, baseModel, bytes.Clone(req.Payload), originalPayload, true)
	if err != nil {
		return nil, err
	}

	requestedModel := payloadRequestedModel(opts, baseModel)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, to.String(), "", body, originalTranslated, requestedModel)

	url := strings.TrimSuffix(baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+bearerToken)
	httpReq.Header.Set("User-Agent", "cli-proxy-kilocode")

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		appendAPIResponseChunk(ctx, e.cfg, data)
		return nil, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out

	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("kilocode executor: close response body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 20_971_520)
		var param any

		for scanner.Scan() {
			line := scanner.Bytes()
			trimmed := bytes.TrimSpace(line)
			// Kilo/OpenRouter keep-alive lines are NOT JSON, e.g. ": OPENROUTER PROCESSING" or ": ping".
			// They must be ignored, otherwise downstream OpenAI chunk parser will fail.
			if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte(":")) {
				continue
			}

			appendAPIResponseChunk(ctx, e.cfg, trimmed)

			if detail, ok := parseOpenAIStreamUsage(trimmed); ok {
				reporter.publish(ctx, detail)
			}

			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), body, bytes.Clone(trimmed), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}

		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
			return
		}

		reporter.ensurePublished(ctx)
	}()

	return stream, nil
}

func (e *KiloCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Roo proxy is OpenAI-like; token count logic can reuse generic openai token counting.
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "count tokens not supported for kilocode"}
}

func (e *KiloCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	// Force mint of a fresh session token.
	_, _, err := e.mintBearerToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	return auth, nil
}

func (e *KiloCodeExecutor) ensureBearerToken(ctx context.Context, auth *cliproxyauth.Auth) (token string, baseURL string, err error) {
	if auth == nil {
		return "", "", statusErr{code: http.StatusUnauthorized, msg: "missing auth"}
	}

	baseURL = strings.TrimSpace(metaStringValue(auth.Metadata, "base_url"))
	if baseURL == "" {
		baseURL = kilocodeProxyBaseURL
	}

	key := auth.ID
	if key == "" {
		key = auth.EnsureIndex()
	}

	e.mu.RLock()
	cached := e.cache[key]
	e.mu.RUnlock()
	if cached != nil && strings.TrimSpace(cached.token) != "" && cached.expiresAt.After(time.Now().Add(30*time.Second)) {
		return cached.token, baseURL, nil
	}

	token, exp, err := e.mintBearerToken(ctx, auth)
	if err != nil {
		return "", baseURL, err
	}

	e.mu.Lock()
	e.cache[key] = &cachedKiloBearerToken{token: token, expiresAt: exp}
	e.mu.Unlock()

	return token, baseURL, nil
}

func (e *KiloCodeExecutor) mintBearerToken(ctx context.Context, auth *cliproxyauth.Auth) (token string, expiresAt time.Time, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if auth == nil {
		return "", time.Time{}, statusErr{code: http.StatusUnauthorized, msg: "kilocode auth is nil"}
	}

	bearer := strings.TrimSpace(metaStringValue(auth.Metadata, "bearer_token"))
	if bearer == "" {
		// Backward compatibility: if old file still has session_token_jwt, use it.
		bearer = strings.TrimSpace(metaStringValue(auth.Metadata, "session_token_jwt"))
	}
	if bearer == "" {
		return "", time.Time{}, statusErr{code: http.StatusUnauthorized, msg: "kilocode auth missing bearer_token"}
	}

	// We don't mint/refresh server-side here; token is issued by Kilo device auth.
	// TTL is unknown; keep a conservative short cache to avoid unbounded reuse.
	expiresAt = time.Now().Add(10 * time.Minute)
	return bearer, expiresAt, nil
}
