package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	xxHash64 "github.com/pierrec/xxHash/xxHash64"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeCCHSeed    uint64 = 0x6E52736AC806831E
	claudeCodeVersion       = "2.1.181"
)

var claudeBillingCCHPattern = regexp.MustCompile(`\bcch=([0-9a-f]{5});`)

func computeFingerprint(messageText, version string) string {
	h := sha256.Sum256([]byte(messageText + version))
	return hex.EncodeToString(h[:])[:3]
}

func generateBillingHeader(payload []byte, version string) string {
	messageText := gjson.GetBytes(payload, "system.0.text").String()
	buildHash := computeFingerprint(messageText, version)
	h := sha256.Sum256(payload)
	cch := hex.EncodeToString(h[:])[:5]
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=cli; cch=%s;", version, buildHash, cch)
}

func signBody(body []byte) []byte {
	billingHeader := gjson.GetBytes(body, "system.0.text").String()
	if !strings.HasPrefix(billingHeader, "x-anthropic-billing-header:") {
		return body
	}
	if !claudeBillingCCHPattern.MatchString(billingHeader) {
		return body
	}

	unsignedBillingHeader := claudeBillingCCHPattern.ReplaceAllString(billingHeader, "cch=00000;")
	unsignedBody, err := sjson.SetBytes(body, "system.0.text", unsignedBillingHeader)
	if err != nil {
		return body
	}

	cch := fmt.Sprintf("%05x", xxHash64.Checksum(unsignedBody, claudeCCHSeed)&0xFFFFF)
	signedBillingHeader := claudeBillingCCHPattern.ReplaceAllString(unsignedBillingHeader, "cch="+cch+";")
	signedBody, err := sjson.SetBytes(unsignedBody, "system.0.text", signedBillingHeader)
	if err != nil {
		return unsignedBody
	}
	return signedBody
}

// injectClaudeCodeSystemBlocks transforms a simple Anthropic request body
// into one that looks like an official Claude Code request by injecting
// billing header and system prompt blocks.
func injectClaudeCodeSystemBlocks(body []byte) []byte {
	// Check if already injected
	firstText := gjson.GetBytes(body, "system.0.text").String()
	if strings.HasPrefix(firstText, "x-anthropic-billing-header:") {
		return signBody(body)
	}

	// Collect existing system text
	existingSystem := gjson.GetBytes(body, "system").String()

	// Build system array: [billing, agent, existing_system_as_user_context]
	billingBlock := map[string]interface{}{
		"type": "text",
		"text": "x-anthropic-billing-header: cc_version=2.1.181.000; cc_entrypoint=cli; cch=00000;",
	}
	agentBlock := map[string]interface{}{
		"type": "text",
		"text": "You are Claude Code, Anthropic's official CLI for Claude.",
	}

	systemBlocks := []interface{}{billingBlock, agentBlock}

	if existingSystem != "" {
		systemBlocks = append(systemBlocks, map[string]interface{}{
			"type": "text",
			"text": existingSystem,
		})
	}

	systemJSON, _ := json.Marshal(systemBlocks)
	body, _ = sjson.SetRawBytes(body, "system", systemJSON)

	return signBody(body)
}
