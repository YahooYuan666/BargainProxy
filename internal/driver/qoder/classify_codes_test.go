package qoder

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// Qoder-4 red-first tests (BP-98): the business code inside the body decides
// the class, read as Qoder's own client reads it (magpie-community/plugins
// packages/qoder 0.2.30 failure()): quota codes are a usage limit, request
// codes refuse THIS request, "Error in upstream response" is the vendor
// behind Qoder failing (sent as a 400 — a 400 that ends the turn as the
// client's fault is the wrong shape).

func TestQuotaCodesAreHardCredit(t *testing.T) {
	for _, code := range []string{"110", "112", "113", "114", "115", "116", "117", "118", "119", "122"} {
		body := `{"code":` + code + `,"message":"plan usage used up"}`
		ue := classifyQoder(400, body)
		if ue.Class != uperr.ClassHardCredit {
			t.Fatalf("code %s class = %s, want hard_credit (else the pool keeps hammering an empty account)", code, ue.Class)
		}
	}
}

func TestRequestCodesStayBadRequest(t *testing.T) {
	for _, code := range []string{"406", "416", "430", "80411", "80412", "100400", "100401", "100403"} {
		body := `{"code":"` + code + `","message":"refused"}`
		ue := classifyQoder(400, body)
		if ue.Class != uperr.ClassBadRequest {
			t.Fatalf("code %s class = %s, want bad_request", code, ue.Class)
		}
	}
}

func TestUpstreamFailedIsTransport(t *testing.T) {
	body := `{"code":400,"message":"Error in upstream response"}`
	ue := classifyQoder(400, body)
	if ue.Class != uperr.ClassTransport {
		t.Fatalf("'error in upstream response' class = %s, want transport (the vendor failed, not the request)", ue.Class)
	}
}

func TestUnrelatedCodeUnchanged(t *testing.T) {
	// An unrecognized code keeps the status-based shape: 400 → bad_request.
	ue := classifyQoder(400, `{"code":7,"message":"whatever"}`)
	if ue.Class != uperr.ClassBadRequest {
		t.Fatalf("unrecognized 400 class = %s, want bad_request", ue.Class)
	}
}

func TestStreamQuotaCodeIsHardCredit(t *testing.T) {
	// The in-stream gateway wrapper carries the body as a string.
	line := `data: {"statusCodeValue":400,"body":"{\"code\":122,\"message\":\"out of credits\"}"}`
	err := (&Driver{}).streamLineError(line)
	if err == nil {
		t.Fatal("in-stream quota error must surface")
	}
	ue, ok := uperr.As(err)
	if !ok || ue.Class != uperr.ClassHardCredit {
		t.Fatalf("in-stream code 122 class = %v, want hard_credit", err)
	}
}
