package executor

import (
	"encoding/json"
	"testing"
)

// A conversation that switched providers replays ids we synthesized for it.
// Upstream rejects the whole request over one unrecognized id, and the id lives
// in the client's history — so without this the session is stranded for good,
// failing identically on every retry.
func TestStripForeignInputIDsDropsIDsUpstreamWouldReject(t *testing.T) {
	var reqMap map[string]interface{}
	raw := `{
		"input": [
			{"type":"message","role":"assistant","id":"item_e04a9233-516f-43bc-9b1d-330d9","content":[{"type":"output_text","text":"kept"}]},
			{"type":"message","role":"user","id":"msg_0123456789","content":[{"type":"input_text","text":"native"}]},
			{"type":"function_call","id":"fc_abc","call_id":"call_1","name":"ls","arguments":"{}"},
			{"type":"reasoning","id":"rs_xyz"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"no id at all"}]}
		]
	}`
	if err := json.Unmarshal([]byte(raw), &reqMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	stripForeignInputIDs(reqMap)

	input := reqMap["input"].([]interface{})
	first := input[0].(map[string]interface{})
	if _, present := first["id"]; present {
		t.Errorf("foreign id survived: %v", first["id"])
	}
	// Dropping the id must not drop the turn — upstream still reads its content.
	if first["content"] == nil {
		t.Error("stripping the id removed the item's content")
	}

	for i, wantID := range map[int]string{1: "msg_0123456789", 2: "fc_abc", 3: "rs_xyz"} {
		item := input[i].(map[string]interface{})
		if item["id"] != wantID {
			t.Errorf("input[%d] id = %v, want %q preserved", i, item["id"], wantID)
		}
	}
}

// Malformed or absent input must not panic the request path.
func TestStripForeignInputIDsToleratesUnexpectedShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"no input":       `{"model":"gpt-5.5"}`,
		"input a string": `{"input":"plain text prompt"}`,
		"scalar items":   `{"input":["a", 3, null]}`,
		"id not string":  `{"input":[{"id":42}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var reqMap map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &reqMap); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			stripForeignInputIDs(reqMap)
		})
	}
}
