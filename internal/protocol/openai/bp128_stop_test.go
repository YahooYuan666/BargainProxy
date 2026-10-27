package openai

import (
	"testing"
)

// BP-128: a strongly-typed []string Stop used to be dropped silently — the
// switch only knew string and []any (new-api bc795cc).
func TestStopTypedSliceSurvives(t *testing.T) {
	wire := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["END","STOP"]}`)
	req, err := ParseChatRequest(wire)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Stop) != 2 || req.Stop[0] != "END" || req.Stop[1] != "STOP" {
		t.Fatalf("stop = %v, want [END STOP]", req.Stop)
	}
}
