package executor

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/ir"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/translator_new/to_ir"
)

func TestKiroExecutorV2DetermineOrigin(t *testing.T) {
	e := &KiroExecutorV2{}
	if got := e.determineOrigin("kiro-claude-opus-4-6"); got != "AI_EDITOR" {
		t.Fatalf("origin for opus = %q, want AI_EDITOR", got)
	}
	if got := e.determineOrigin("kiro-claude-sonnet-4-5"); got != "AI_EDITOR" {
		t.Fatalf("origin for kiro sonnet = %q, want AI_EDITOR", got)
	}
	if got := e.determineOrigin("claude-sonnet-4-6"); got != "AI_EDITOR" {
		t.Fatalf("origin for bare sonnet = %q, want AI_EDITOR", got)
	}
	if got := e.determineOrigin("amazonq-claude-sonnet-4-6"); got != "CLI" {
		t.Fatalf("origin for amazonq sonnet = %q, want CLI", got)
	}
	if got := e.determineOrigin("kiro-auto"); got != "AI_EDITOR" {
		t.Fatalf("origin for kiro auto = %q, want AI_EDITOR", got)
	}
}

func TestKiroExecutorV2DetermineKiroAPIRegion(t *testing.T) {
	tests := []struct {
		name string
		auth *coreauth.Auth
		want string
	}{
		{name: "nil auth", auth: nil, want: ""},
		{name: "explicit api region wins", auth: &coreauth.Auth{Metadata: map[string]any{"api_region": "eu-west-1", "profile_arn": "arn:aws:codewhisperer:us-east-1:123:profile/x", "region": "ap-south-1"}}, want: "eu-west-1"},
		{name: "falls back to profile arn region", auth: &coreauth.Auth{Metadata: map[string]any{"profile_arn": "arn:aws:codewhisperer:us-west-2:123456789012:profile/default", "region": "ap-south-1"}}, want: "us-west-2"},
		{name: "ignores plain region field", auth: &coreauth.Auth{Metadata: map[string]any{"region": "ap-south-1"}}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := determineKiroAPIRegion(tt.auth); got != tt.want {
				t.Fatalf("determineKiroAPIRegion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKiroExecutorV2ExtractRegionFromProfileARN(t *testing.T) {
	if got := extractRegionFromProfileARNV2("arn:aws:codewhisperer:eu-central-1:123456789012:profile/default"); got != "eu-central-1" {
		t.Fatalf("extractRegionFromProfileARNV2() = %q, want eu-central-1", got)
	}
	if got := extractRegionFromProfileARNV2("arn:aws:codewhisperer::123456789012:profile/default"); got != "" {
		t.Fatalf("extractRegionFromProfileARNV2() for malformed arn = %q, want empty", got)
	}
}

func TestKiroExecutorV2ShouldSendProfileArn(t *testing.T) {
	tests := []struct {
		name string
		auth *coreauth.Auth
		want bool
	}{
		{name: "nil auth", auth: nil, want: true},
		{name: "builder id suppressed", auth: &coreauth.Auth{Metadata: map[string]any{"auth_method": "builder-id"}}, want: false},
		{name: "aws sso oidc suppressed", auth: &coreauth.Auth{Metadata: map[string]any{"auth_type": "aws_sso_oidc"}}, want: false},
		{name: "idc allowed", auth: &coreauth.Auth{Metadata: map[string]any{"auth_method": "idc"}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSendProfileArn(tt.auth); got != tt.want {
				t.Fatalf("shouldSendProfileArn() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKiroExecutorV2ParseTokenExpiryAndMetaString(t *testing.T) {
	expiry := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	meta := map[string]any{
		"expiresAt":    expiry.Format(time.RFC3339),
		"access_token": "tok-a",
		"accessToken":  "tok-b",
	}
	if got := parseTokenExpiry(meta); !got.Equal(expiry) {
		t.Fatalf("parseTokenExpiry() = %v, want %v", got, expiry)
	}
	if got := getMetaString(meta, "missing", "access_token", "accessToken"); got != "tok-a" {
		t.Fatalf("getMetaString() = %q, want tok-a", got)
	}
	if got := parseTokenExpiry(map[string]any{"expires_at": "invalid"}); !got.IsZero() {
		t.Fatalf("parseTokenExpiry(invalid) = %v, want zero", got)
	}
}

func TestKiroExecutorV2MapModelID(t *testing.T) {
	tests := map[string]string{
		"kiro-claude-opus-4-6-agentic":   "claude-opus-4.6",
		"kiro-claude-sonnet-4-6":         "claude-sonnet-4.6",
		"kiro-claude-sonnet-4-6-agentic": "claude-sonnet-4.6",
		"amazonq-claude-sonnet-4-5":      "claude-sonnet-4.5",
		"amazonq-claude-sonnet-4-6":      "claude-sonnet-4.6",
		"amazonq-custom-model":           "custom-model",
		"kiro-auto":                      "auto",
		"amazonq-auto":                   "auto",
		"auto":                           "auto",
	}
	for input, want := range tests {
		if got := mapModelID(input); got != want {
			t.Fatalf("mapModelID(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestKiroExecutorV2PrepareRequestAppliesMetadataAndFallbacks(t *testing.T) {
	e := &KiroExecutorV2{}
	auth := &coreauth.Auth{Metadata: map[string]any{
		"access_token": "token-123",
		"expires_at":   time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"profile_arn":  "arn:aws:codewhisperer:eu-west-1:123456789012:profile/default",
		"api_region":   "us-west-2",
	}}
	req := cliproxyexecutor.Request{
		Model: "kiro-claude-opus-4-6",
		Payload: []byte(`{
			"model":"ignored-by-prepare",
			"messages":[{"role":"user","content":"hello from prepareRequest"}]
		}`),
		Format: sdktranslator.FormatOpenAI,
	}

	rc, err := e.prepareRequest(t.Context(), auth, req, sdktranslator.FormatOpenAI.String())
	if err != nil {
		t.Fatalf("prepareRequest() error = %v", err)
	}
	if rc.origin != "AI_EDITOR" {
		t.Fatalf("origin = %q, want AI_EDITOR", rc.origin)
	}
	if rc.kiroModelID != "claude-opus-4.6" {
		t.Fatalf("kiroModelID = %q, want claude-opus-4.6", rc.kiroModelID)
	}
	if rc.apiRegion != "us-west-2" {
		t.Fatalf("apiRegion = %q, want us-west-2", rc.apiRegion)
	}
	if rc.irReq == nil || rc.irReq.Metadata == nil {
		t.Fatal("expected ir request metadata to be initialized")
	}
	if got := rc.irReq.Metadata["origin"]; got != "AI_EDITOR" {
		t.Fatalf("origin metadata = %#v, want AI_EDITOR", got)
	}
	if got, _ := rc.irReq.Metadata["profileArn"].(string); got != "arn:aws:codewhisperer:eu-west-1:123456789012:profile/default" {
		t.Fatalf("profileArn metadata = %q, want upstream profile arn", got)
	}
	if sid, _ := rc.irReq.Metadata["session_id"].(string); strings.TrimSpace(sid) == "" {
		t.Fatal("expected session_id fallback to be derived")
	}
	if rc.irReq.Thinking != nil {
		t.Fatalf("did not expect implicit thinking for plain opus request, got %#v", rc.irReq.Thinking)
	}
	if !bytes.Contains(rc.kiroBody, []byte(`"conversationState"`)) {
		t.Fatalf("kiro body missing conversationState: %s", string(rc.kiroBody))
	}
}

func TestKiroExecutorV2PrepareRequestAppliesDefaultThinkingForExplicitThinkingSignals(t *testing.T) {
	e := &KiroExecutorV2{}
	auth := &coreauth.Auth{Metadata: map[string]any{
		"access_token": "token-123",
		"expires_at":   time.Now().Add(2 * time.Hour).Format(time.RFC3339),
	}}
	req := cliproxyexecutor.Request{
		Model:   "kiro-claude-sonnet-4-5-thinking",
		Payload: []byte(`{"messages":[{"role":"user","content":"reason carefully"}]}`),
		Format:  sdktranslator.FormatOpenAI,
	}
	rc, err := e.prepareRequest(t.Context(), auth, req, sdktranslator.FormatOpenAI.String())
	if err != nil {
		t.Fatalf("prepareRequest() error = %v", err)
	}
	if rc.irReq.Thinking == nil || !rc.irReq.Thinking.IncludeThoughts || rc.irReq.Thinking.Budget != 16000 {
		t.Fatalf("expected default thinking config from model signal, got %#v", rc.irReq.Thinking)
	}
}

func TestKiroExecutorV2PrepareRequestSuppressesProfileArnForBuilderID(t *testing.T) {
	e := &KiroExecutorV2{}
	auth := &coreauth.Auth{Metadata: map[string]any{
		"access_token": "token-123",
		"expires_at":   time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"profile_arn":  "arn:aws:codewhisperer:us-east-1:123456789012:profile/default",
		"auth_method":  "builder-id",
	}}
	req := cliproxyexecutor.Request{
		Model:   "kiro-claude-sonnet-4-5",
		Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`),
		Format:  sdktranslator.FormatOpenAI,
	}
	rc, err := e.prepareRequest(t.Context(), auth, req, sdktranslator.FormatOpenAI.String())
	if err != nil {
		t.Fatalf("prepareRequest() error = %v", err)
	}
	if _, exists := rc.irReq.Metadata["profileArn"]; exists {
		t.Fatalf("expected profileArn to be suppressed, metadata = %#v", rc.irReq.Metadata)
	}
	if rc.origin != "AI_EDITOR" {
		t.Fatalf("origin = %q, want AI_EDITOR", rc.origin)
	}
}

func TestKiroExecutorV2BuildHTTPRequest(t *testing.T) {
	e := &KiroExecutorV2{}
	auth := &coreauth.Auth{Metadata: map[string]any{
		"machine_id": "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
	}}
	rc := &requestContext{
		ctx:       t.Context(),
		auth:      auth,
		token:     "token-123",
		kiroBody:  []byte(`{"conversationState":{}}`),
		apiRegion: "us-west-2",
	}

	primary, err := e.buildHTTPRequest(rc)
	if err != nil {
		t.Fatalf("buildHTTPRequest(primary) error = %v", err)
	}
	if got := primary.URL.String(); got != "https://q.us-west-2.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("primary url = %q", got)
	}
	if got := primary.Header.Get("X-Amz-Target"); got != "" {
		t.Fatalf("primary X-Amz-Target = %q, want empty", got)
	}
	if got := primary.Header.Get("Authorization"); got != "Bearer token-123" {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
	if got := primary.Header.Get("Accept"); got != kiroAcceptStreamV2 {
		t.Fatalf("Accept = %q, want %q", got, kiroAcceptStreamV2)
	}
	if got := primary.Header.Get("x-amzn-kiro-agent-mode"); got != kiroAgentModeHeaderV2 {
		t.Fatalf("agent mode header = %q, want %q", got, kiroAgentModeHeaderV2)
	}

	rc.useFallback = true
	fallback, err := e.buildHTTPRequest(rc)
	if err != nil {
		t.Fatalf("buildHTTPRequest(fallback) error = %v", err)
	}
	if got := fallback.URL.String(); got != "https://codewhisperer.us-west-2.amazonaws.com/generateAssistantResponse" {
		t.Fatalf("fallback url = %q", got)
	}
	if got := fallback.Header.Get("X-Amz-Target"); got != kiroTargetV2 {
		t.Fatalf("fallback X-Amz-Target = %q, want %q", got, kiroTargetV2)
	}
}

func TestKiroExecutorV2StateHelpers(t *testing.T) {
	state := to_ir.NewKiroStreamState()
	state.ToolCalls = append(state.ToolCalls, ir.ToolCall{ID: "call_1"})
	if !stateHasToolCall(state, "call_1") {
		t.Fatal("expected stateHasToolCall to find call_1")
	}
	if stateHasToolCall(state, "missing") {
		t.Fatal("did not expect stateHasToolCall to find missing call")
	}
	if shouldFinalizeKiroStreamAfterError(nil) {
		t.Fatal("nil state should not finalize")
	}
	empty := to_ir.NewKiroStreamState()
	if shouldFinalizeKiroStreamAfterError(empty) {
		t.Fatal("empty state should not finalize")
	}
	empty.PendingContent = "tail"
	if !shouldFinalizeKiroStreamAfterError(empty) {
		t.Fatal("pending content should trigger finalize")
	}
}

func TestKiroExecutorV2SplitAWSEventStream(t *testing.T) {
	frame := buildAWSEventFrameForTest([]byte(`{"content":"hello"}`), nil)
	advance, token, err := splitAWSEventStream(frame, false)
	if err != nil {
		t.Fatalf("splitAWSEventStream(valid) error = %v", err)
	}
	if advance != len(frame) {
		t.Fatalf("advance = %d, want %d", advance, len(frame))
	}
	if !bytes.Equal(token, frame) {
		t.Fatalf("token mismatch")
	}

	advance, token, err = splitAWSEventStream([]byte{0x00, 0x01}, false)
	if err != nil || advance != 0 || token != nil {
		t.Fatalf("short non-eof split = (%d, %#v, %v), want (0,nil,nil)", advance, token, err)
	}

	advance, token, err = splitAWSEventStream([]byte{0x00, 0x01}, true)
	if err != nil || advance != 2 || token != nil {
		t.Fatalf("short eof split = (%d, %#v, %v), want (2,nil,nil)", advance, token, err)
	}

	invalid := make([]byte, 20)
	binary.BigEndian.PutUint32(invalid[0:4], 15)
	advance, token, err = splitAWSEventStream(invalid, false)
	if err != nil || advance != 1 || token != nil {
		t.Fatalf("invalid length split = (%d, %#v, %v), want (1,nil,nil)", advance, token, err)
	}
}

func TestKiroExecutorV2ParseEventPayload(t *testing.T) {
	payload := []byte(`{"toolUseEvent":{"toolUseId":"tooluse_1","name":"read","input":{"filePath":"a.txt"}}}`)
	frame := buildAWSEventFrameForTest(payload, nil)
	decoded, err := parseEventPayload(frame)
	if err != nil {
		t.Fatalf("parseEventPayload(valid) error = %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Fatalf("decoded payload = %s, want %s", string(decoded), string(payload))
	}

	if _, err := parseEventPayload([]byte{1, 2, 3}); err == nil || !strings.Contains(err.Error(), "short frame") {
		t.Fatalf("expected short frame error, got %v", err)
	}

	brokenCRC := bytes.Clone(frame)
	brokenCRC[8] ^= 0xFF
	if _, err := parseEventPayload(brokenCRC); err == nil || !strings.Contains(err.Error(), "crc mismatch") {
		t.Fatalf("expected crc mismatch, got %v", err)
	}

	badBounds := buildAWSEventFrameForTest(payload, func(frame []byte) {
		binary.BigEndian.PutUint32(frame[4:8], uint32(len(frame)))
		crc := crc32.ChecksumIEEE(frame[0:8])
		binary.BigEndian.PutUint32(frame[8:12], crc)
	})
	if _, err := parseEventPayload(badBounds); err == nil || !strings.Contains(err.Error(), "bounds") {
		t.Fatalf("expected bounds error, got %v", err)
	}
}

func buildAWSEventFrameForTest(payload []byte, mutate func(frame []byte)) []byte {
	headersLen := 0
	totalLen := 12 + headersLen + len(payload) + 4
	frame := make([]byte, totalLen)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLen))
	binary.BigEndian.PutUint32(frame[4:8], uint32(headersLen))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	copy(frame[12:], payload)
	if mutate != nil {
		mutate(frame)
	}
	return frame
}
