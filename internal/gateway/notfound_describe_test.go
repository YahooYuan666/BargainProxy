package gateway

import (
	"net/http"
	"testing"

	"bargainproxy/internal/uperr"
)

// TestDescribeErrorNotFoundIsHonest404: a 404 from every binding is a routing
// problem, not a rate limit or a gateway bug. Rendering it as 429 ("retry
// later") or 500 ("gateway bug") sends the client down the wrong path; 404
// tells the truth. 2026-10-19, alongside the ClassNotFound split.
func TestDescribeErrorNotFoundIsHonest404(t *testing.T) {
	ue := uperr.New(uperr.ClassNotFound, http.StatusNotFound, "workbuddy: 404 page not found")
	status, class, msg := DescribeError(ue)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
	if class != uperr.ClassNotFound {
		t.Fatalf("class = %q, want %q", class, uperr.ClassNotFound)
	}
	if msg == "" {
		t.Fatal("message must explain that no route exists")
	}
}
