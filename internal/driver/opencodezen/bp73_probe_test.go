package opencodezen

import (
	"net/http"
	"strings"
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-73: a probe failure that says nothing about the model must not be recorded
// as "fail" — the anonymous free lane answers 429 whenever its shared quota is
// spent, and a scheduled probe used to turn that into a page of dead models.
func TestTransientProbeErrClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"free-lane quota", classifyHTTP(http.StatusTooManyRequests, []byte(`{"type":"error","error":{"type":"FreeUsageLimitError"}}`)), true},
		{"timeout", uperr.New(uperr.ClassUpstreamTimeout, 0, "slow"), true},
		{"canceled", uperr.New(uperr.ClassCanceled, 0, "canceled"), true},
		{"transport", uperr.New(uperr.ClassTransport, 502, "bad gateway"), true},
		{"waf", uperr.New(uperr.ClassWAF, 403, "edge block"), true},
		// The gate's 403 is answered network-by-network (the official client
		// draws it too), so it must not retire a model.
		{"gate rejected", classifyHTTP(http.StatusForbidden, []byte(`{"error":{"type":"FreeTierError"}}`)), true},
		// A plain 403 is an edge/WAF block (ClassWAF) — IP-scoped, transient.
		{"other 403", classifyHTTP(http.StatusForbidden, []byte(`forbidden by policy`)), true},
		{"model gone", classifyHTTP(http.StatusNotFound, []byte(`model not found`)), false},
		{"bad request", classifyHTTP(http.StatusBadRequest, []byte(`bad`)), false},
		{"plain error", errValue("boom"), false},
	}
	for _, tc := range cases {
		if got := transientProbeErr(tc.err); got != tc.want {
			t.Fatalf("%s: transientProbeErr = %v, want %v", tc.name, got, tc.want)
		}
	}
}

type errValue string

func (e errValue) Error() string { return string(e) }

// A transient verdict keeps the model usable; only a definitive failure
// downgrades it. A blip must also not resurrect a model whose last real probe
// failed.
func TestRecordProbeTransientKeepsVerdict(t *testing.T) {
	c := NewCatalog(nil, nil, "")
	c.mu.Lock()
	c.snap = Snapshot{Models: []ModelEntry{
		{ID: "a", Candidate: true, ProbeStatus: ProbeOK},
		{ID: "b", Candidate: true, ProbeStatus: ProbeFail, ProbeError: "opencodezen: model endpoint gone: 404"},
		{ID: "c", Candidate: true},
		// A gate-403 recorded before the transient split: it must be cleared,
		// not left as a permanent "failed" verdict.
		{ID: "d", Candidate: true, ProbeStatus: ProbeFail, ProbeError: "opencodezen: free-tier gate rejected the request"},
	}}
	c.mu.Unlock()

	c.RecordProbeTransient("a", 120, "429 rate limit")
	c.RecordProbeTransient("b", 130, "429 rate limit")
	c.RecordProbeTransient("c", 140, "timeout")
	c.RecordProbeTransient("d", 150, "429 rate limit")

	got := map[string]ModelEntry{}
	for _, m := range c.Snapshot().Models {
		got[m.ID] = m
	}
	if got["a"].ProbeStatus != ProbeLimited || got["a"].ProbeError != "429 rate limit" || got["a"].ProbeMs != 120 {
		t.Fatalf("ok model lost its usable verdict: %+v", got["a"])
	}
	if got["b"].ProbeStatus != ProbeFail || !strings.Contains(got["b"].ProbeError, "404") {
		t.Fatalf("transient blip resurrected a definitively failed model: %+v", got["b"])
	}
	if got["c"].ProbeStatus != ProbeLimited {
		t.Fatalf("never-probed model should read limited, got %+v", got["c"])
	}
	if got["d"].ProbeStatus != ProbeLimited {
		t.Fatalf("misclassified gate-403 must clear to limited: %+v", got["d"])
	}

	// A later real success upgrades limited → ok and clears the diagnostic.
	c.RecordProbe("a", true, 90, "")
	for _, m := range c.Snapshot().Models {
		if m.ID == "a" && (m.ProbeStatus != ProbeOK || m.ProbeError != "") {
			t.Fatalf("recovery did not clear the verdict: %+v", m)
		}
	}
}
