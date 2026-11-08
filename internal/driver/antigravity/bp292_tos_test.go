package antigravity

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-292: when Google disables an account for a Terms-of-Service violation it
// answers 403 with a google.rpc.ErrorInfo detail carrying
//
//	"reason": "TOS_VIOLATION", "appeal_url": "https://forms.gle/…"
//
// Field case 2026-11-06 03:19 (xchu2693@gmail.com): the account never answered
// again, on either host, for the rest of the day. The old 403 branch sent every
// non-Cloudflare 403 to ClassWAF, whose pool handling is a 60-second rest — so
// the banned account came back into rotation once a minute and was re-selected
// on every turn (6 fresh 403s upstream on 2026-11-07 alone) while the client was
// told "upstream edge blocked the request".
func TestBP292TOSViolationIsAccountRestricted(t *testing.T) {
	body := []byte(`{"error":{"code":403,` +
		`"message":"This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.",` +
		`"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo",` +
		`"reason":"TOS_VIOLATION","domain":"cloudcode-pa.googleapis.com",` +
		`"metadata":{"uiMessage":"true","appeal_url":"https://forms.gle/hGzM9MEUv2azZsrb9","appeal_url_link_text":"Submit Appeal"}}]}}`)
	ue := classifyHTTP(403, body, "gemini-3.8-flash-high")
	if ue.Class != uperr.ClassAccountRestricted {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassAccountRestricted)
	}
	if ue.Status != 403 {
		t.Fatalf("status = %d, want 403", ue.Status)
	}
	if uperr.Rotatable(ue.Class) {
		t.Fatal("a banned account must not be rotatable — another account of the same channel is a different Google identity")
	}
}

// The ban also arrives as a 200 stream frame (detectFrameError derives 403 from
// the PERMISSION_DENIED status string), so the same body must classify the same
// way on that path.
func TestBP292TOSViolationInStreamFrame(t *testing.T) {
	frame := `{"error":{"code":403,"message":"This service has been disabled in this account for violation of Terms of Service.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`
	ue := detectFrameError(frame, "gemini-3.8-flash-high")
	if ue == nil {
		t.Fatal("frame error not detected")
	}
	if ue.Class != uperr.ClassAccountRestricted {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassAccountRestricted)
	}
}

// A plain 403 without the ToS markers stays WAF: an edge/Cloudflare refusal is
// request-scoped and must NOT take a healthy account out of routing for a week
// (BP-94 drew the same line for ClassEdgeBlock).
func TestBP292Generic403StaysWAF(t *testing.T) {
	ue := classifyHTTP(403, []byte(`{"error":{"code":403,"message":"forbidden by policy"}}`), "gemini-3.8-flash-high")
	if ue.Class != uperr.ClassWAF {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassWAF)
	}
}

// The wording-only shape (no details array) is the same refusal, so the message
// text is a fallback marker.
func TestBP292TOSWordingWithoutDetails(t *testing.T) {
	body := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"This service has been disabled in this account for violation of Terms of Service."}}`)
	ue := classifyHTTP(403, body, "")
	if ue.Class != uperr.ClassAccountRestricted {
		t.Fatalf("class = %s, want %s", ue.Class, uperr.ClassAccountRestricted)
	}
}
