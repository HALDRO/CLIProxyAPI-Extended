package to_ir

import "strings"

// EncodeToolIDWithSignature packs thoughtSignature into a tool call ID.
// This is a best-effort round-trip helper: older clients may strip custom fields.
//
// Format: <id>|sig:<signature>
// If signature is empty, the original id is returned.
func EncodeToolIDWithSignature(id, signature string) string {
	id = strings.TrimSpace(id)
	signature = strings.TrimSpace(signature)
	if signature == "" {
		return id
	}
	if id == "" {
		id = "tool"
	}
	return id + "|sig:" + signature
}

// DecodeToolIDAndSignature unpacks a tool call ID produced by EncodeToolIDWithSignature.
// If the id does not contain a signature marker, returns (id, "").
func DecodeToolIDAndSignature(encoded string) (id, signature string) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return "", ""
	}
	const marker = "|sig:"
	idx := strings.Index(encoded, marker)
	if idx < 0 {
		return encoded, ""
	}
	id = strings.TrimSpace(encoded[:idx])
	signature = strings.TrimSpace(encoded[idx+len(marker):])
	return id, signature
}
