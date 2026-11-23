// BP-335: the Responses surface dropped the `stop` field entirely. An upstream
// that honours stop sequences then returned the marker AND everything after
// it — output the client explicitly asked not to receive (sub2api #8963).
//
// The chat ingress has parsed both shapes for a long time (chat.go:165); this
// surface simply never read the field.
package responses

import (
	"testing"
)

func TestStopStringIsCarried(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","input":"hi","stop":"<END>"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "<END>" {
		t.Fatalf("stop = %v, want [<END>]", req.Stop)
	}
}

func TestStopArrayIsCarried(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"m","input":"hi","stop":["<END>","STOP"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Stop) != 2 || req.Stop[0] != "<END>" || req.Stop[1] != "STOP" {
		t.Fatalf("stop = %v, want [<END> STOP]", req.Stop)
	}
}

// Absent or empty stop means "no sequences", not an error and not a
// single empty string (which some upstreams reject).
func TestStopAbsentOrEmpty(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":"hi"}`,
		`{"model":"m","input":"hi","stop":""}`,
		`{"model":"m","input":"hi","stop":[]}`,
		`{"model":"m","input":"hi","stop":[""]}`,
	} {
		req, err := ParseRequest([]byte(body))
		if err != nil {
			t.Fatalf("body %s: %v", body, err)
		}
		if len(req.Stop) != 0 {
			t.Fatalf("body %s: stop = %v, want none", body, req.Stop)
		}
	}
}

// A malformed stop is an error, not a silent drop: forwarding without the
// sequences produces output the client asked not to see.
func TestStopMalformedIsAnError(t *testing.T) {
	if _, err := ParseRequest([]byte(`{"model":"m","input":"hi","stop":123}`)); err == nil {
		t.Fatal("a numeric stop must be rejected, not silently dropped")
	}
}
