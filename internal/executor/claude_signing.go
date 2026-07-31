package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	claudeCodeVersion     = "2.1.220"
	claudeAttributionSeed = "59cf53e54c78"
)

func computeFingerprint(messageText, version string) string {
	units := utf16.Encode([]rune(messageText))
	var selected strings.Builder
	for _, index := range []int{4, 7, 20} {
		if index >= len(units) {
			selected.WriteByte('0')
			continue
		}
		unit := units[index]
		if unit >= 0xD800 && unit <= 0xDFFF {
			selected.WriteRune(utf8.RuneError)
			continue
		}
		selected.WriteRune(rune(unit))
	}
	h := sha256.Sum256([]byte(claudeAttributionSeed + selected.String() + version))
	return hex.EncodeToString(h[:])[:3]
}

func firstUserText(payload []byte) string {
	var request struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(payload, &request) != nil {
		return ""
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			return text
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(message.Content, &blocks) == nil {
			for _, block := range blocks {
				if block.Type == "text" {
					return block.Text
				}
			}
		}
		return ""
	}
	return ""
}

func generateBillingHeader(payload []byte, version string) string {
	fingerprint := computeFingerprint(firstUserText(payload), version)
	return fmt.Sprintf("x-anthropic-billing-header: cc_version=%s.%s; cc_entrypoint=sdk-cli;", version, fingerprint)
}

// injectClaudeCodeSystemBlocks transforms a simple Anthropic request body
// into one that looks like an official Claude Code request by injecting
// billing header and system prompt blocks.
func injectClaudeCodeSystemBlocks(body []byte) []byte {
	// Check if already injected
	firstText := gjson.GetBytes(body, "system.0.text").String()
	if strings.HasPrefix(firstText, "x-anthropic-billing-header:") {
		return body
	}

	// Build system array: [billing, agent, ...whatever the caller sent]
	billingBlock := map[string]interface{}{
		"type": "text",
		"text": generateBillingHeader(body, claudeCodeVersion),
	}
	agentBlock := map[string]interface{}{
		"type": "text",
		"text": "You are Claude Code, Anthropic's official CLI for Claude.",
	}

	systemBlocks := []interface{}{billingBlock, agentBlock}

	// The caller's system is appended block by block, verbatim.
	//
	// This used to be gjson.Get(body, "system").String() dropped into the text of
	// a single block — and gjson returns *raw JSON* for an array, so a client that
	// sent proper system blocks got the whole array stringified into one block.
	// Two consequences, both silent: every cache_control the client had placed on
	// its system prompt was destroyed (so the upstream could not cache that
	// prefix), and the prompt itself arrived as escaped JSON, paying extra tokens
	// to say the same thing less clearly.
	existing := gjson.GetBytes(body, "system")
	switch {
	case existing.IsArray():
		for _, block := range existing.Array() {
			systemBlocks = append(systemBlocks, json.RawMessage(block.Raw))
		}
	case existing.Type == gjson.String && existing.String() != "":
		systemBlocks = append(systemBlocks, map[string]interface{}{
			"type": "text",
			"text": existing.String(),
		})
	}

	systemJSON, _ := json.Marshal(systemBlocks)
	body, _ = sjson.SetRawBytes(body, "system", systemJSON)

	return body
}
