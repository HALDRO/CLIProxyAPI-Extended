// Package claude provides exported web search helpers for kiro_executor_v2.
// These wrap the unexported implementations in kiro_executor.go (v1) so that
// the v2 executor can call them via the kiroclaude package alias.
package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/kiro"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// FetchToolDescription
// ---------------------------------------------------------------------------

var (
	toolDescMu      sync.Mutex
	toolDescFetched atomic.Bool
)

// FetchToolDescription calls MCP tools/list to get the web_search tool
// description and caches it via SetWebSearchDescription.
// Safe to call concurrently — only one goroutine fetches at a time.
// fp and authAttrs are passed through to NewWebSearchHandler for header injection.
func FetchToolDescription(mcpEndpoint, authToken string, httpClient *http.Client, fp *kiroauth.Fingerprint, authAttrs map[string]string) {
	if toolDescFetched.Load() {
		return
	}

	toolDescMu.Lock()
	defer toolDescMu.Unlock()

	if toolDescFetched.Load() {
		return
	}

	handler := NewWebSearchHandler(mcpEndpoint, authToken, httpClient, fp, authAttrs)
	reqBody := []byte(`{"id":"tools_list","jsonrpc":"2.0","method":"tools/list"}`)

	req, err := http.NewRequest("POST", mcpEndpoint, bytes.NewReader(reqBody))
	if err != nil {
		log.Warnf("kiro/websearch: failed to create tools/list request: %v", err)
		return
	}
	handler.setHeaders(req)

	resp, err := handler.httpClient.Do(req)
	if err != nil {
		log.Warnf("kiro/websearch: tools/list request failed: %v", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Warnf("kiro/websearch: tools/list returned status %d", resp.StatusCode)
		return
	}

	var result struct {
		Result *struct {
			Tools []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil || result.Result == nil {
		log.Warnf("kiro/websearch: failed to parse tools/list response")
		return
	}

	for _, tool := range result.Result.Tools {
		if tool.Name == "web_search" && tool.Description != "" {
			SetWebSearchDescription(tool.Description)
			toolDescFetched.Store(true)
			log.Infof("kiro/websearch: cached web_search description (%d bytes)", len(tool.Description))
			return
		}
	}
	log.Warnf("kiro/websearch: web_search tool not found in tools/list response")
}

// ---------------------------------------------------------------------------
// WebSearchHandler (exported)
// ---------------------------------------------------------------------------

const mcpMaxRetries = 2

// WebSearchHandler handles web search requests via Kiro MCP API.
type WebSearchHandler struct {
	mcpEndpoint string
	httpClient  *http.Client
	authToken   string
	fp          *kiroauth.Fingerprint
	authAttrs   map[string]string
}

// NewWebSearchHandler creates a new exported WebSearchHandler.
func NewWebSearchHandler(mcpEndpoint, authToken string, httpClient *http.Client, fp *kiroauth.Fingerprint, authAttrs map[string]string) *WebSearchHandler {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &WebSearchHandler{
		mcpEndpoint: mcpEndpoint,
		httpClient:  httpClient,
		authToken:   authToken,
		fp:          fp,
		authAttrs:   authAttrs,
	}
}

// setHeaders sets standard MCP API headers on the request.
func (h *WebSearchHandler) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	if h.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+h.authToken)
	}
	if h.fp != nil {
		req.Header.Set("x-amzn-codewhisperer-fingerprint", h.fp.KiroHash)
	}
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	for k, v := range h.authAttrs {
		if strings.HasPrefix(strings.ToLower(k), "x-") {
			req.Header.Set(k, v)
		}
	}
}

// CallMcpAPI calls the Kiro MCP API with retry logic.
func (h *WebSearchHandler) CallMcpAPI(request *McpRequest) (*McpResponse, error) {
	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal MCP request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= mcpMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<attempt) * time.Second
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			log.Warnf("kiro/websearch: MCP retry %d/%d after %v", attempt, mcpMaxRetries, backoff)
			time.Sleep(backoff)
		}

		req, err := http.NewRequest("POST", h.mcpEndpoint, bytes.NewReader(requestBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create HTTP request: %w", err)
		}
		h.setHeaders(req)

		resp, err := h.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("MCP API request failed: %w", err)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read MCP response: %w", err)
			continue
		}

		if resp.StatusCode >= 502 && resp.StatusCode <= 504 {
			lastErr = fmt.Errorf("MCP API returned retryable status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("MCP API returned status %d: %s", resp.StatusCode, string(body))
		}

		var mcpResponse McpResponse
		if err := json.Unmarshal(body, &mcpResponse); err != nil {
			return nil, fmt.Errorf("failed to parse MCP response: %w", err)
		}
		if mcpResponse.Error != nil {
			code := -1
			if mcpResponse.Error.Code != nil {
				code = *mcpResponse.Error.Code
			}
			msg := "Unknown error"
			if mcpResponse.Error.Message != nil {
				msg = *mcpResponse.Error.Message
			}
			return nil, fmt.Errorf("MCP error %d: %s", code, msg)
		}
		return &mcpResponse, nil
	}
	return nil, lastErr
}

// ---------------------------------------------------------------------------
// StripWebSearchTool
// ---------------------------------------------------------------------------

// StripWebSearchTool removes web_search tools from the tools array in a Claude payload.
func StripWebSearchTool(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body, nil
	}

	var kept []json.RawMessage
	for _, tool := range tools.Array() {
		name := strings.ToLower(tool.Get("name").String())
		toolType := strings.ToLower(tool.Get("type").String())
		if isWebSearchTool(name, toolType) {
			continue
		}
		kept = append(kept, json.RawMessage(tool.Raw))
	}

	if len(kept) == 0 {
		// Remove tools array entirely
		var payload map[string]interface{}
		if err := json.Unmarshal(body, &payload); err != nil {
			return body, err
		}
		delete(payload, "tools")
		return json.Marshal(payload)
	}

	keptJSON, err := json.Marshal(kept)
	if err != nil {
		return body, err
	}
	return sjsonSetRawBytes(body, "tools", keptJSON)
}

// sjsonSetRawBytes is a helper to avoid importing sjson in this file.
func sjsonSetRawBytes(body []byte, path string, value []byte) ([]byte, error) {
	// Manual JSON replacement for "tools" field
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, err
	}
	payload[path] = value
	return json.Marshal(payload)
}

// ---------------------------------------------------------------------------
// GenerateMessageID
// ---------------------------------------------------------------------------

// GenerateMessageID generates a Claude-style message ID (msg_...).
func GenerateMessageID() string {
	return "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
}
