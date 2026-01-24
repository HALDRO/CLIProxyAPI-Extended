package cache

import (
	"strings"
	"sync"
	"time"
)

const (
	thoughtSignatureTTL         = 2 * time.Hour
	minThoughtSignatureLength   = 50
	skipThoughtSignatureLiteral = "skip_thought_signature_validator"
)

type thoughtSignatureEntry struct {
	signature string
	expiresAt time.Time
}

type thoughtSignatureCache struct {
	mu sync.RWMutex
	// latest valid signature per session
	bySession map[string]thoughtSignatureEntry
}

var globalThoughtSignatureCache = &thoughtSignatureCache{bySession: make(map[string]thoughtSignatureEntry)}

func CacheSessionThoughtSignature(sessionID, signature string) {
	sessionID = strings.TrimSpace(sessionID)
	signature = strings.TrimSpace(signature)
	if sessionID == "" || signature == "" {
		return
	}
	if signature != skipThoughtSignatureLiteral && len(signature) < minThoughtSignatureLength {
		return
	}
	globalThoughtSignatureCache.mu.Lock()
	globalThoughtSignatureCache.bySession[sessionID] = thoughtSignatureEntry{
		signature: signature,
		expiresAt: time.Now().Add(thoughtSignatureTTL),
	}
	globalThoughtSignatureCache.mu.Unlock()
}

func GetSessionThoughtSignature(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ""
	}
	globalThoughtSignatureCache.mu.RLock()
	entry, ok := globalThoughtSignatureCache.bySession[sessionID]
	globalThoughtSignatureCache.mu.RUnlock()
	if !ok {
		return ""
	}
	if time.Now().After(entry.expiresAt) {
		globalThoughtSignatureCache.mu.Lock()
		delete(globalThoughtSignatureCache.bySession, sessionID)
		globalThoughtSignatureCache.mu.Unlock()
		return ""
	}
	return entry.signature
}

func HasValidThoughtSignature(signature string) bool {
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return false
	}
	if signature == skipThoughtSignatureLiteral {
		return true
	}
	return len(signature) >= minThoughtSignatureLength
}

func ClearSessionThoughtSignatureCache() {
	globalThoughtSignatureCache.mu.Lock()
	globalThoughtSignatureCache.bySession = make(map[string]thoughtSignatureEntry)
	globalThoughtSignatureCache.mu.Unlock()
}
