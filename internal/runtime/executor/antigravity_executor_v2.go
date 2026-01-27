package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/cache"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/from_ir"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

const (
	antigravityNewAuthType = "antigravity"

	agv1internalGenerate = "/v1internal:generateContent"
	agv1internalStream   = "/v1internal:streamGenerateContent"

	antigravityUserAgentDefault = "antigravity/1.104.0 darwin/arm64"
)

// AntigravityExecutorV2 is a clean executor that uses ONLY translator_new.
// It intentionally does not depend on sdk/translator legacy request translation.
//
// It targets Cloud Code Assist v1internal endpoints with Antigravity envelope:
// {project, requestId, userAgent, requestType, model, request:{GeminiRequest}}
//
// Response unwrapping is handled via translator_wrapper.go (TranslateAntigravityResponse*).
type AntigravityExecutorV2 struct {
	cfg *config.Config
}

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

func NewAntigravityExecutorV2(cfg *config.Config) *AntigravityExecutorV2 {
	return &AntigravityExecutorV2{cfg: cfg}
}

func (e *AntigravityExecutorV2) Identifier() string { return antigravityNewAuthType }

func (e *AntigravityExecutorV2) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _, err := e.ensureAccessToken(req.Context(), auth)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return statusErr{code: http.StatusUnauthorized, msg: "missing access token"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

func (e *AntigravityExecutorV2) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("antigravity canonical executor: request is nil")
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

func (e *AntigravityExecutorV2) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return resp, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	// Ensure project_id (Code Assist) is populated even when the access token is still valid.
	// The refresh path already tries to populate project_id.
	if auth != nil {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		if auth.Metadata["project_id"] == nil {
			// Populate project_id via loadCodeAssist.
			if errProject := ensureAntigravityProjectID(ctx, e.cfg, auth, token); errProject != nil {
				logWithRequestID(ctx).Warnf("antigravity canonical executor: ensure project id failed: %v", errProject)
			}
		}
	}

	// Build IR request (source format -> IR)
	irReq, err := convertRequestToIR(opts.SourceFormat, baseModel, bytes.Clone(req.Payload), opts.Metadata)
	if err != nil {
		return resp, err
	}

	// Provider-specific metadata for Antigravity envelope.
	ensureAntigravityMetadata(irReq, auth, opts)

	// Convert IR -> Antigravity envelope.
	body, err := (&from_ir.AntigravityProvider{}).ConvertRequest(irReq)
	if err != nil {
		return resp, err
	}

	// Apply YAML payload rules under protocol "antigravity".
	// Historically, Antigravity rules target the inner request body.
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, "antigravity", "request", body, opts.OriginalRequest, requestedModel)

	// Apply Claude-specific tweaks and cleanup for non-Claude models
	if strings.Contains(baseModel, "claude") {
		body, _ = sjson.SetBytes(body, "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
	} else {
		body, _ = sjson.DeleteBytes(body, "request.generationConfig.maxOutputTokens")
	}

	_ = buildAntigravityEndpoint(auth, false, opts.Alt)
	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	attempts := antigravityRetryAttempts(auth, e.cfg)

	var lastErr error
	var lastStatus int
	var lastBody []byte

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		for idx, baseURL := range baseURLs {
		requestURL := strings.TrimSuffix(baseURL, "/") + agv1internalGenerate
		if opts.Alt != "" {
			requestURL += "?$alt=" + url.QueryEscape(opts.Alt)
		}

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
		if errReq != nil {
			return resp, errReq
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveAntigravityUserAgent(auth))
		httpReq.Header.Set("Accept", "application/json")
		if host := resolveHost(baseURL); host != "" {
			httpReq.Host = host
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			lastErr = errDo
			if idx+1 < len(baseURLs) {
				continue
			}
			return resp, errDo
		}

		data, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			logWithRequestID(ctx).Errorf("antigravity canonical executor: close response body error: %v", errClose)
		}
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
		appendAPIResponseChunk(ctx, e.cfg, data)

		if errRead != nil {
			recordAPIResponseError(ctx, e.cfg, errRead)
			lastErr = errRead
			if idx+1 < len(baseURLs) {
				continue
			}
			return resp, errRead
		}

		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			lastStatus = httpResp.StatusCode
			lastBody = data
			if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
				continue
			}
			if antigravityShouldRetryNoCapacity(httpResp.StatusCode, data) {
				if idx+1 < len(baseURLs) {
					logWithRequestID(ctx).Debugf("antigravity v2 executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if attempt+1 < attempts {
					delay := antigravityNoCapacityRetryDelay(attempt)
					log.Debugf("antigravity v2 executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
					if errWait := antigravityWait(ctx, delay); errWait != nil {
						return resp, errWait
					}
					continue attemptLoop
				}
			}
			break
		}

		reporter.publish(ctx, parseAntigravityUsage(data))
		translated, err := TranslateAntigravityResponseNonStream(e.cfg, opts.SourceFormat, data, req.Model)
		if err != nil {
			return resp, fmt.Errorf("translate response: %w", err)
		}
		resp = cliproxyexecutor.Response{Payload: translated}
		reporter.ensurePublished(ctx)
		return resp, nil
		}

		if lastStatus != 0 {
			sErr := statusErr{code: lastStatus, msg: string(lastBody)}
			if lastStatus == http.StatusTooManyRequests {
				if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
					sErr.retryAfter = retryAfter
				}
			}
			return resp, sErr
		}
		if lastErr != nil {
			return resp, lastErr
		}
	}

	if lastStatus != 0 {
		sErr := statusErr{code: lastStatus, msg: string(lastBody)}
		if lastStatus == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return resp, sErr
	}
	if lastErr != nil {
		return resp, lastErr
	}
	return resp, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity canonical executor: no base url available"}
}

func (e *AntigravityExecutorV2) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	token, updatedAuth, errToken := e.ensureAccessToken(ctx, auth)
	if errToken != nil {
		return nil, errToken
	}
	if updatedAuth != nil {
		auth = updatedAuth
	}

	reporter := newUsageReporter(ctx, e.Identifier(), baseModel, auth)
	defer reporter.trackFailure(ctx, &err)

	// Ensure project_id (Code Assist) is populated even when the access token is still valid.
	// The refresh path already tries to populate project_id.
	if auth != nil {
		if auth.Metadata == nil {
			auth.Metadata = make(map[string]any)
		}
		if auth.Metadata["project_id"] == nil {
			// Populate project_id via loadCodeAssist.
			if errProject := ensureAntigravityProjectID(ctx, e.cfg, auth, token); errProject != nil {
				logWithRequestID(ctx).Warnf("antigravity canonical executor: ensure project id failed: %v", errProject)
			}
		}
	}

	irReq, err := convertRequestToIR(opts.SourceFormat, baseModel, bytes.Clone(req.Payload), opts.Metadata)
	if err != nil {
		return nil, err
	}

	sessionID := ensureAntigravityMetadata(irReq, auth, opts)

	body, err := (&from_ir.AntigravityProvider{}).ConvertRequest(irReq)
	if err != nil {
		return nil, err
	}
	requestedModel := payloadRequestedModel(opts, req.Model)
	body = applyPayloadConfigWithRoot(e.cfg, baseModel, "antigravity", "request", body, opts.OriginalRequest, requestedModel)

	// Apply Claude-specific tweaks and cleanup for non-Claude models
	if strings.Contains(baseModel, "claude") {
		body, _ = sjson.SetBytes(body, "request.toolConfig.functionCallingConfig.mode", "VALIDATED")
	} else {
		body, _ = sjson.DeleteBytes(body, "request.generationConfig.maxOutputTokens")
	}

	_ = buildAntigravityEndpoint(auth, true, opts.Alt)
	baseURLs := antigravityBaseURLFallbackOrder(auth)
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)

	attempts := antigravityRetryAttempts(auth, e.cfg)

	var lastErr error
	var lastStatus int
	var lastBody []byte

attemptLoop:
	for attempt := 0; attempt < attempts; attempt++ {
		for idx, baseURL := range baseURLs {
		requestURL := strings.TrimSuffix(baseURL, "/") + agv1internalStream
		if opts.Alt != "" {
			requestURL += "?$alt=" + url.QueryEscape(opts.Alt)
		} else {
			requestURL += "?alt=sse"
		}

		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
		if errReq != nil {
			return nil, errReq
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+token)
		httpReq.Header.Set("User-Agent", resolveAntigravityUserAgent(auth))
		httpReq.Header.Set("Accept", "text/event-stream")
		if host := resolveHost(baseURL); host != "" {
			httpReq.Host = host
		}

		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			recordAPIResponseError(ctx, e.cfg, errDo)
			lastErr = errDo
			if idx+1 < len(baseURLs) {
				continue
			}
			return nil, errDo
		}

		if httpResp.StatusCode < http.StatusOK || httpResp.StatusCode >= http.StatusMultipleChoices {
			data, _ := io.ReadAll(httpResp.Body)
			_ = httpResp.Body.Close()
			appendAPIResponseChunk(ctx, e.cfg, data)
			lastStatus = httpResp.StatusCode
			lastBody = data
			if httpResp.StatusCode == http.StatusTooManyRequests && idx+1 < len(baseURLs) {
				continue
			}
			if antigravityShouldRetryNoCapacity(httpResp.StatusCode, data) {
				if idx+1 < len(baseURLs) {
					logWithRequestID(ctx).Debugf("antigravity v2 executor: no capacity on base url %s, retrying with fallback base url: %s", baseURL, baseURLs[idx+1])
					continue
				}
				if attempt+1 < attempts {
					delay := antigravityNoCapacityRetryDelay(attempt)
					log.Debugf("antigravity v2 executor: no capacity for model %s, retrying in %s (attempt %d/%d)", baseModel, delay, attempt+1, attempts)
					if errWait := antigravityWait(ctx, delay); errWait != nil {
						return nil, errWait
					}
					continue attemptLoop
				}
			}
			break
		}

		out := make(chan cliproxyexecutor.StreamChunk)
		stream = out
		go func(resp *http.Response) {
			defer close(out)
			defer func() {
				if errClose := resp.Body.Close(); errClose != nil {
					log.Errorf("antigravity canonical executor: close response body error: %v", errClose)
				}
			}()

			// Peek first JSON data line for bootstrap reliability.
			br := bufio.NewReader(resp.Body)
			firstLine, errPeek := readFirstSSEDataLine(ctx, br, 30*time.Second)
			if errPeek != nil {
				recordAPIResponseError(ctx, e.cfg, errPeek)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errPeek}
				return
			}
			if len(firstLine) == 0 {
				recordAPIResponseError(ctx, e.cfg, fmt.Errorf("empty first stream chunk"))
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("empty first stream chunk")}
				return
			}

			state := NewAntigravityStreamState(opts.OriginalRequest)
			messageID := "chatcmpl-" + baseModel
			// Pass through: the executor yields raw SSE bytes; handler bootstrap can retry before first byte.
			scanner := bufio.NewScanner(io.MultiReader(bytes.NewReader(firstLine), br))
			scanner.Buffer(nil, streamScannerBuffer)

			for scanner.Scan() {
				line := scanner.Bytes()
				appendAPIResponseChunk(ctx, e.cfg, line)

				// Cache thoughtSignature for tool loops from envelope if present.
				cacheThoughtSignatureFromAntigravityChunk(sessionID, line)

				// Filter usage metadata on non-terminal chunks.
				line = FilterSSEUsageMetadata(line)

				payload := jsonPayload(line)
				if payload == nil {
					continue
				}

				if detail, ok := parseAntigravityStreamUsage(payload); ok {
					reporter.publish(ctx, detail)
				}

				chunks, errConv := TranslateAntigravityResponseStream(e.cfg, opts.SourceFormat, payload, req.Model, messageID, state)
				if errConv != nil {
					out <- cliproxyexecutor.StreamChunk{Err: errConv}
					return
				}
				for i := range chunks {
					out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}
				}
			}

			// Finalize stream.
			tail, errTail := TranslateAntigravityResponseStream(e.cfg, opts.SourceFormat, []byte("[DONE]"), req.Model, messageID, state)
			if errTail == nil {
				for i := range tail {
					out <- cliproxyexecutor.StreamChunk{Payload: tail[i]}
				}
			}

			if errScan := scanner.Err(); errScan != nil {
				recordAPIResponseError(ctx, e.cfg, errScan)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
				return
			}
			reporter.ensurePublished(ctx)
		}(httpResp)

		return stream, nil
		}

		if lastStatus != 0 {
			sErr := statusErr{code: lastStatus, msg: string(lastBody)}
			if lastStatus == http.StatusTooManyRequests {
				if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
					sErr.retryAfter = retryAfter
				}
			}
			return nil, sErr
		}
		if lastErr != nil {
			return nil, lastErr
		}
	}

	if lastStatus != 0 {
		sErr := statusErr{code: lastStatus, msg: string(lastBody)}
		if lastStatus == http.StatusTooManyRequests {
			if retryAfter, parseErr := parseRetryDelay(lastBody); parseErr == nil && retryAfter != nil {
				sErr.retryAfter = retryAfter
			}
		}
		return nil, sErr
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, statusErr{code: http.StatusServiceUnavailable, msg: "antigravity canonical executor: no base url available"}
}

func (e *AntigravityExecutorV2) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	// Delegate to existing refresh implementation (OAuth refresh + project_id).
	// This keeps request/response translation fully canonical while reusing proven auth logic.
	old := NewAntigravityExecutor(e.cfg)
	return old.Refresh(ctx, auth)
}

func (e *AntigravityExecutorV2) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// For now, call the upstream endpoint using the same request translation as legacy.
	// This does not affect the canonical chat flow and can be refactored later.
	old := NewAntigravityExecutor(e.cfg)
	return old.CountTokens(ctx, auth, req, opts)
}

func (e *AntigravityExecutorV2) ensureAccessToken(ctx context.Context, auth *cliproxyauth.Auth) (string, *cliproxyauth.Auth, error) {
	old := NewAntigravityExecutor(e.cfg)
	return old.ensureAccessToken(ctx, auth)
}

func buildAntigravityEndpoint(auth *cliproxyauth.Auth, stream bool, alt string) string {
	base := antigravityBaseURLFallbackOrder(auth)
	baseURL := ""
	if len(base) > 0 {
		baseURL = base[0]
	}
	path := agv1internalGenerate
	if stream {
		path = agv1internalStream
	}

	u := strings.TrimSuffix(baseURL, "/") + path
	if stream {
		if alt != "" {
			u += "?$alt=" + url.QueryEscape(alt)
		} else {
			u += "?alt=sse"
		}
	} else if alt != "" {
		u += "?$alt=" + url.QueryEscape(alt)
	}
	return u
}

func resolveAntigravityUserAgent(auth *cliproxyauth.Auth) string {
	if auth != nil {
		if auth.Attributes != nil {
			if ua := strings.TrimSpace(auth.Attributes["user_agent"]); ua != "" {
				return ua
			}
		}
		if auth.Metadata != nil {
			if ua, ok := auth.Metadata["user_agent"].(string); ok {
				if trimmed := strings.TrimSpace(ua); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return antigravityUserAgentDefault
}

func ensureAntigravityMetadata(irReq *ir.UnifiedChatRequest, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) (sessionID string) {
	if irReq == nil {
		return ""
	}
	if irReq.Metadata == nil {
		irReq.Metadata = make(map[string]any)
	}

	// Ensure project id is available.
	if _, ok := irReq.Metadata["project_id"]; !ok {
		projectID := ""
		if auth != nil && auth.Metadata != nil {
			if pid, ok := auth.Metadata["project_id"].(string); ok {
				projectID = strings.TrimSpace(pid)
			}
		}
		if projectID != "" {
			irReq.Metadata["project_id"] = projectID
		}
	}

	// request_id: use idempotency_key if present.
	requestID := ""
	if opts.Metadata != nil {
		if key, ok := opts.Metadata["idempotency_key"].(string); ok {
			if trimmed := strings.TrimSpace(key); trimmed != "" {
				requestID = "agent-" + trimmed
			}
		}
	}
	if requestID == "" {
		requestID = "agent-" + uuid.NewString()
	}
	irReq.Metadata["request_id"] = requestID
	irReq.Metadata["user_agent"] = "antigravity"

	// session_id: stable hash from original request; fallback to envelope request.sessionId.
	if sid, ok := irReq.Metadata["session_id"].(string); ok {
		sessionID = strings.TrimSpace(sid)
	}
	if sessionID == "" {
		sessionID = from_ir.DeriveSessionID(opts.OriginalRequest)
		if sessionID != "" {
			irReq.Metadata["session_id"] = sessionID
		}
	}

	// requestType selection: simple mapping, allow override.
	if _, ok := irReq.Metadata["request_type"]; !ok {
		requestType := "agent"
		// Web search intent via networking tools.
		if hasGoogleSearch(irReq) {
			requestType = "web_search"
		}
		irReq.Metadata["request_type"] = requestType
	}

	if _, ok := irReq.Metadata["raw_request"]; !ok {
		var raw map[string]any
		if err := json.Unmarshal(opts.OriginalRequest, &raw); err == nil {
			irReq.Metadata["raw_request"] = raw
		}
	}
	if m, ok := irReq.Metadata["raw_request"].(map[string]any); ok {
		ir.DeepCleanUndefined(m)
	}

	return sessionID
}

func hasGoogleSearch(req *ir.UnifiedChatRequest) bool {
	if req == nil {
		return false
	}
	// google_search is stored in metadata by OpenAI parser.
	if req.Metadata != nil {
		if _, ok := req.Metadata["google_search"]; ok {
			return true
		}
	}
	return false
}

func readFirstSSEDataLine(ctx context.Context, r *bufio.Reader, timeout time.Duration) ([]byte, error) {
	deadline := time.Now().Add(timeout)
	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for first stream line")
		}
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			continue
		}
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			return line, nil
		}
	}
}

func cacheThoughtSignatureFromAntigravityChunk(sessionID string, line []byte) {
	if strings.TrimSpace(sessionID) == "" {
		return
	}
	payload := jsonPayload(line)
	if payload == nil || !gjson.ValidBytes(payload) {
		return
	}
	// Prefer unwrapped "response" but handle both.
	parts := gjson.GetBytes(payload, "response.candidates.0.content.parts")
	if !parts.Exists() {
		parts = gjson.GetBytes(payload, "candidates.0.content.parts")
	}
	if !parts.IsArray() {
		return
	}
	for _, part := range parts.Array() {
		sig := strings.TrimSpace(part.Get("thoughtSignature").String())
		if sig == "" {
			sig = strings.TrimSpace(part.Get("thought_signature").String())
		}
		if cache.HasValidThoughtSignature(sig) {
			cache.CacheSessionThoughtSignature(sessionID, sig)
			return
		}
	}
}

var _ = errors.Is
