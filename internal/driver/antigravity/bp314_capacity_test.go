package antigravity

// BP-314: "no capacity for THIS MODEL" is not a sick account, not a quota
// wall, and above all not a transport failure.
//
// Field 2026-11-12 16:40, three antigravity accounts probed within one second:
//
//	gemini-3.8-flash-high  503  MODEL_CAPACITY_EXHAUSTED  ("No capacity available
//	                                 for model … on the server")
//	gemini-3.8-flash-low   503  same
//	gemini-3.8-flash-tiered 503 same
//	gemini-3-flash         200  in 1.4s      ← SAME account, SAME host, SAME second
//	gemini-2.5-flash       200  in 1.1s
//	claude-sonnet-4-6      200  in 1.1s
//	gemini-3.1-flash-image 200  in 1.2s
//
// and retrieveUserQuotaSummary said five_hour=1.0, weekly=0.956. The account
// had quota; Google had no server for that one model.
//
// The classifier put the 503 in ClassTransport, which is ROTATABLE: the gateway
// walked all three accounts onto the same wall (three upstream hits per
// request) and told the client "upstream transport failure" — a gateway bug —
// instead of "wait, don't switch".

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/uperr"
)

const capacityBody = `{
  "error": {
    "code": 503,
    "message": "No capacity available for model gemini-3.8-flash-high on the server",
    "status": "UNAVAILABLE",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "MODEL_CAPACITY_EXHAUSTED",
        "domain": "cloudcode-pa.googleapis.com"
      }
    ]
  }
}`

// The classification itself: capacity 503 must be the capacity class, not
// transport (rotatable) and not soft-rate (account quota wall).
func TestCapacityExhaustedClassifiesAsUpstreamBusy(t *testing.T) {
	e := classifyHTTP(503, []byte(capacityBody), "gemini-3.8-flash-high")
	if e.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("class = %q, want %q — a capacity wall is not rotatable and not the account's quota",
			e.Class, uperr.ClassUpstreamBusy)
	}
	if e.Model != "gemini-3.8-flash-high" {
		t.Fatalf("Model = %q, want the model that had no capacity", e.Model)
	}
	if e.Status != 503 {
		t.Fatalf("Status = %d, want 503", e.Status)
	}
}

// The rotatability contract the gateway actually acts on: switching accounts
// cannot fix a fleet-capacity wall, so the class must not be rotatable.
func TestCapacityWallIsNotRotatable(t *testing.T) {
	if uperr.Rotatable(uperr.ClassUpstreamBusy) {
		t.Fatal("ClassUpstreamBusy must not be rotatable")
	}
	if uperr.Rotatable(uperr.ClassTransport) == false {
		t.Fatal("control: ClassTransport is rotatable, which is why the old classification walked the pool")
	}
}

// Wording-only shape (no details array): the message alone must still classify.
func TestCapacityWordingWithoutDetails(t *testing.T) {
	body := `{"error":{"code":503,"message":"No capacity available for model gemini-3.8-flash-low on the server","status":"UNAVAILABLE"}}`
	if e := classifyHTTP(503, []byte(body), "gemini-3.8-flash-low"); e.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("class = %q, want upstream_busy for the wording-only shape", e.Class)
	}
}

// Controls: the neighbouring verdicts must not be swallowed by the new branch.
// A RESOURCE_EXHAUSTED 429 stays a quota wall (account ledger), and a plain
// 500 with no capacity wording stays transport.
func TestCapacityBranchDoesNotSwallowOtherVerdicts(t *testing.T) {
	quota := `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`
	if e := classifyHTTP(429, []byte(quota), "gemini-3.8-flash-high"); e.Class != uperr.ClassSoftRate {
		t.Fatalf("429 RESOURCE_EXHAUSTED class = %q, want soft_rate (quota wall)", e.Class)
	}
	// A 429 carrying the capacity wording still speaks about quota (Google
	// sends 429 for RESOURCE_EXHAUSTED only); keep the quota verdict there.
	if e := classifyHTTP(500, []byte(`{"error":{"code":500,"message":"internal"}}`), "m"); e.Class != uperr.ClassTransport {
		t.Fatalf("plain 500 class = %q, want transport", e.Class)
	}
}

// End-to-end through the Stream entry (not just the helper): the driver must
// surface the capacity class. This is the shape the gateway consumes, and
// testing only classifyHTTP is how a class can be right in isolation and wrong
// on the wire.
func TestStreamSurfacesCapacityClass(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, capacityBody)
	}))
	defer upstream.Close()

	d := testDriver(upstream.URL, upstream.URL+"/token")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.Stream(ctx, driver.ChatCall{
		Account: testAccount(t), UpstreamModel: "gemini-3.8-flash-high",
		Request: &ir.ChatRequest{Model: "gemini-3.8-flash-high", Messages: []ir.Message{
			{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
		},
	}, func(ev ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected an error from the 503")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("error is not a uperr: %v", err)
	}
	if ue.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("class through Stream = %q, want upstream_busy", ue.Class)
	}
	if !strings.Contains(ue.Message, "capacity") {
		t.Fatalf("message must keep the upstream's own words, got %q", ue.Message)
	}
}
