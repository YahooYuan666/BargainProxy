package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// TRAE-1 red-first tests (BP-98): the ladder gate must judge the upstream
// business *code*, not the error wording. magpie-community/plugins
// packages/trae 0.2.19: wrongFunction = ["4001","4023","1005"] by code; the
// quota family is [4008, 1005] on CN (+4011 on Global).

// TestLadderWalksOn4023And1005: a model served on solo_agent only answers
// 4023 on the first rung and 1005 on the second; both mean "this function
// will not take it", so the walk must reach the third rung and succeed.
// Before the fix 4023 landed in ClassInternal (gate fails on class) and the
// walk stopped at rung one — a servable request recorded as a 12h cooldown.
func TestLadderWalksOn4023And1005(t *testing.T) {
	var tried []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fn, _ := body["function"].(string)
		tried = append(tried, fn)
		switch fn {
		case functionsTrae[0]:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":4023,"message":"model not available on this function"}`)
		case functionsTrae[1]:
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"code":1005,"message":"plan entitlement exhausted"}`)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event:output\ndata: {\"response\":\"ok\"}\n\n")
			fmt.Fprint(w, "event:done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		}
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{
			Model:    "deepseek-v4.1-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	resp, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("ladder must reach rung three (tried %v): %v", tried, err)
	}
	if resp == nil || len(tried) != 3 {
		t.Fatalf("expected 3 rungs tried, got %v", tried)
	}
}

// Test1005CarriesBusinessCode: the final propagated 1005 (every rung refused)
// must keep its business code on the error so the panel shows 1005, not a
// bare message — and so the pool's SoftRate handling keys on the code.
func Test1005CarriesBusinessCode(t *testing.T) {
	e := classifyBusiness(1005, "plan entitlement exhausted")
	if e.Code != "1005" {
		t.Fatalf("1005 must carry Code=1005, got %q", e.Code)
	}
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("1005 class = %s, want soft_rate", e.Class)
	}
	if time.Until(e.ResetAt) < 11*time.Hour {
		t.Fatalf("1005 reset too soon: %s", e.ResetAt)
	}
}

// Test4023IsBadRequestWithCode: 4023 is a function refusal, not an internal
// error — it must be ClassBadRequest (no penalty, ladder-eligible) and carry
// its code.
func Test4023IsBadRequestWithCode(t *testing.T) {
	e := classifyBusiness(4023, "model not available on this function")
	if e.Class != uperr.ClassBadRequest {
		t.Fatalf("4023 class = %s, want bad_request", e.Class)
	}
	if e.Code != "4023" {
		t.Fatalf("4023 must carry Code=4023, got %q", e.Code)
	}
}

// TestQuotaCodes4008And4011: the quota family beyond 1005 (CN 4008, Global
// 4011) must read as plan-entitlement soft-rate with the code attached —
// today they fall into ClassInternal "account keeps being retried without a
// cooldown".
func TestQuotaCodes4008And4011(t *testing.T) {
	for _, code := range []int{4008, 4011} {
		e := classifyBusiness(code, "quota")
		if e.Class != uperr.ClassSoftRate {
			t.Fatalf("code %d class = %s, want soft_rate", code, e.Class)
		}
		if e.Code != fmt.Sprint(code) {
			t.Fatalf("code %d must carry its business code, got %q", code, e.Code)
		}
	}
}

// TestTraeWrongFunctionByCode: the gate itself must accept the code-carried
// errors and reject look-alikes (a 400 with an unrelated code, a plain 429).
func TestTraeWrongFunctionByCode(t *testing.T) {
	badReq := uperr.New(uperr.ClassBadRequest, 400, "trae: refused")
	badReq.Code = "4001"
	softRate := uperr.New(uperr.ClassSoftRate, 200, "trae: entitlement")
	softRate.Code = "1005"
	internal := uperr.New(uperr.ClassInternal, 200, "trae: code=4023 look-alike in text")
	internalNoCode := uperr.New(uperr.ClassBadRequest, 400, "trae: code=4001 in text but no code field")
	plain429 := uperr.New(uperr.ClassSoftRate, 429, "trae: too many requests")

	cases := []struct {
		err  error
		want bool
	}{
		{badReq, true},
		{softRate, true},
		{internal, false},       // ClassInternal: gate fails on class even if the text looks like a code
		{plain429, false},       // a real rate limit is not a function refusal
		{internalNoCode, false}, // no Code field: only the "param is invalid" fallback may rescue it
	}
	for i, tc := range cases {
		if got := traeWrongFunction(tc.err); got != tc.want {
			t.Fatalf("case %d (%v): traeWrongFunction = %v, want %v", i, tc.err, got, tc.want)
		}
	}
	// The shape fallback stays: a bare "param is invalid" 400 is treated as a
	// function refusal (one cheap walk, bounded).
	shape := uperr.New(uperr.ClassBadRequest, 400, "trae: the param is invalid")
	if !traeWrongFunction(shape) {
		t.Fatal("param-is-invalid fallback must still walk the ladder")
	}
}

// TestLadderExhaustedPropagates1005Cooldown: when every rung answers 1005 the
// final error must still be the SoftRate/12h entitlement error (walking the
// ladder must not soften a real quota exhaustion into a bad request).
func TestLadderExhaustedPropagates1005Cooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"code":1005,"message":"plan entitlement exhausted"}`)
	}))
	defer srv.Close()

	creds, _ := json.Marshal(map[string]any{"access_token": "jwt", "uid": "u1"})
	d := New(config.TraeSettings{AgentHost: srv.URL, UgHost: srv.URL}, nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "trae:u1", CredsJSON: creds},
		UpstreamModel: "deepseek-v4.1-flash",
		Request: &ir.ChatRequest{
			Model:    "deepseek-v4.1-flash",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
			Stream:   true,
		},
	}
	_, err := d.Stream(context.Background(), call, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("expected the final 1005 to propagate")
	}
	ue, ok := uperr.As(err)
	if !ok {
		t.Fatalf("expected an uperr.Error, got %T: %v", err, err)
	}
	if ue.Class != uperr.ClassSoftRate || ue.Code != "1005" {
		t.Fatalf("final error = %s/%s, want soft_rate/1005", ue.Class, ue.Code)
	}
	if strings.Count(ue.Message, "1005") > 0 && !strings.Contains(ue.Message, "plan entitlement") {
		t.Fatalf("message lost the upstream reason: %q", ue.Message)
	}
}
