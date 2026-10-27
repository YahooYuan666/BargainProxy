package qoder

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// BP-163: the official client declares the model's input ceiling on the
// top-level model_config; a catalogue entry that names one must reach the
// wire (without it the server truncates at its ~200K default).
func TestBP163MaxInputTokensOnWire(t *testing.T) {
	b := newRequestBody()
	b.ModelConfig.MaxInputTokens = 1000000
	raw, err := marshalNoEscape(b)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ModelConfig struct {
			MaxInputTokens int `json:"max_input_tokens"`
		} `json:"model_config"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.ModelConfig.MaxInputTokens != 1000000 {
		t.Fatalf("max_input_tokens = %d, want 1000000", doc.ModelConfig.MaxInputTokens)
	}

	// A model with no ceiling in the catalogue keeps the field off the wire
	// (omitempty), exactly like a client that knows nothing.
	empty := newRequestBody()
	raw2, _ := marshalNoEscape(empty)
	if strings.Contains(string(raw2), "max_input_tokens") {
		t.Fatal("a catalogue without a ceiling must not invent one")
	}
}

// BP-164: the chat header set differs from the shared one — no forged
// machine token/type, plus the hostname/OS pair the official chat sends.
func TestBP164ChatHeaderSplit(t *testing.T) {
	sess := &sessionContext{
		machineID:    "mid",
		machineType:  "mt",
		machineToken: "mtn",
	}
	shared := bearerHeaders(sess, 1700000000, "Bearer x", "application/json", false)
	if shared.Get("cosy-machinetoken") == "" || shared.Get("cosy-machinetype") == "" {
		t.Fatal("the shared (listing/usage/queue) set must keep machine token/type")
	}

	chat := bearerHeaders(sess, 1700000000, "Bearer x", "text/event-stream", true)
	if chat.Get("cosy-machinetoken") != "" {
		t.Fatal("chat must not carry the forged machine token (official chat sends the native umid or nothing)")
	}
	if chat.Get("cosy-machinetype") != "" {
		t.Fatal("chat must not carry the forged machine type")
	}
	if chat.Get("cosy-machinehostname") == "" {
		t.Fatal("chat must carry Cosy-MachineHostname")
	}
	if chat.Get("cosy-machineos") == "" {
		t.Fatal("chat must carry Cosy-MachineOS")
	}
	if chat.Get("cosy-machineid") != "mid" {
		t.Fatal("machine id stays on chat")
	}
}

func TestBP164MachineHostnameSanitized(t *testing.T) {
	if os_name := qoderMachineOS(); !strings.Contains(os_name, "_") {
		t.Fatalf("machine OS = %q, want the arch_platform shape", os_name)
	}
	// The sanitizer is pure string logic; drive it via a deterministic check
	// on its outputs through the header path instead of patching os.Hostname.
	hn := bearerHeaders(&sessionContext{machineID: "m"}, 1, "b", "a", true).Get("cosy-machinehostname")
	if len(hn) > 96 {
		t.Fatalf("hostname %q exceeds 96 chars", hn)
	}
	for _, r := range hn {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			t.Fatalf("hostname %q has unsanitized character %q", hn, r)
		}
	}
	if strings.Contains(hn, "--") || strings.HasPrefix(hn, "-") || strings.HasSuffix(hn, "-") {
		t.Fatalf("hostname %q has uncollapsed or boundary dashes", hn)
	}
	_ = http.MethodPost
}

// BP-167: a usage frame naming a different model than requested is parseable,
// so the warn in streamOnce can fire.
func TestBP167BilledModelParsed(t *testing.T) {
	u := extractUsageObj(map[string]interface{}{
		"prompt_tokens":     10,
		"completion_tokens": 5,
		"total_tokens":      15,
		"credits":           1.25,
		"model":             "auto",
	})
	if u == nil || u.Model != "auto" {
		t.Fatalf("billed model = %+v, want auto", u)
	}
	// Absent stays absent.
	u2 := extractUsageObj(map[string]interface{}{"prompt_tokens": 1})
	if u2.Model != "" {
		t.Fatalf("absent billed model = %q", u2.Model)
	}
}
