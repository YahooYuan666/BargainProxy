package openai

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"strings"
	"testing"
)

// BP-277 (traework2api-panel a45bd4e): blocks the upstreams do not understand
// (input_audio etc. — astrbot stuffs QQ voice straight into content) used to
// vanish without a word, so a voice-only message reached Trae as content:null
// and died as 4001 "the param is invalid" while the client believed the model
// had heard it. The ingress must SAY what it dropped.
func TestParseContentWarnsOnUnsupportedBlock(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	raw := json.RawMessage(`[
		{"type":"text","text":"这段语音说了什么"},
		{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}
	]`)
	parts, err := parseContent("user", "glm-5.3-flash", raw)
	if err != nil {
		t.Fatalf("parseContent: %v", err)
	}
	for _, p := range parts {
		if p.Kind != "text" {
			t.Fatalf("only text should survive, got kind %q", p.Kind)
		}
	}
	if n := len(parts); n != 1 {
		t.Fatalf("text part should survive the audio drop, got %d parts", n)
	}
	if out := buf.String(); !strings.Contains(out, "input_audio") {
		t.Fatalf("the dropped block must be logged with its type, log was: %s", out)
	}
}

// A voice-only message parses to ZERO parts — the trae egress placeholder
// (buildSOLOBody) is what keeps that from going on the wire as content:null.
func TestParseContentAudioOnlyYieldsNoParts(t *testing.T) {
	raw := json.RawMessage(`[{"type":"input_audio","input_audio":{"data":"QUJD","format":"wav"}}]`)
	parts, err := parseContent("user", "glm-5.3-flash", raw)
	if err != nil {
		t.Fatalf("parseContent: %v", err)
	}
	if len(parts) != 0 {
		t.Fatalf("audio-only message should parse to zero parts, got %v", parts)
	}
}
