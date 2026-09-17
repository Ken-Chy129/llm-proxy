package compaction

import (
	"encoding/base64"
	"strings"
)

// Prefix distinguishes portable summaries produced by this proxy from opaque
// account-bound encrypted_content issued by Codex upstream.
const Prefix = "ken-compact-v1:"

func Encode(summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(Prefix + summary))
}

func Decode(encrypted string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encrypted))
	if err != nil {
		return "", false
	}
	text := string(raw)
	if !strings.HasPrefix(text, Prefix) {
		return "", false
	}
	summary := strings.TrimSpace(strings.TrimPrefix(text, Prefix))
	return summary, summary != ""
}
