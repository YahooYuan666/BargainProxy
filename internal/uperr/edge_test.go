package uperr

import "testing"

// TestIsEdgeBlock pins the shapes that mean "the CDN refused it", as opposed to
// the vendor's own 403. Charging an account for an edge block is what turned
// one Cloudflare rule into a dead pool upstream (sub2api e0fa4b94f, BP-94).
func TestIsEdgeBlock(t *testing.T) {
	yes := []string{
		`<html><title>Access denied</title>error code: 1010</html>`,
		`{"error":{"message":"error code: 1010","type":"cf_error"}}`,
		`{"error":{"cloudflare_error":true}}`,
		`{"cloudflare_error": true}`,
		`<title>Attention Required! | Cloudflare</title>`,
		`<div id="cf-error-details">blocked</div>`,
	}
	for _, body := range yes {
		if !IsEdgeBlock([]byte(body)) {
			t.Fatalf("edge block not recognized: %s", body)
		}
	}
	no := []string{
		``,
		`{"code":11140,"msg":"request illegal"}`,
		`{"error":{"message":"insufficient quota"}}`,
		`{"error":{"type":"permission_error","message":"model not available"}}`,
	}
	for _, body := range no {
		if IsEdgeBlock([]byte(body)) {
			t.Fatalf("vendor error misread as an edge block: %s", body)
		}
	}
}

// TestEdgeBlockIsNotRotatable pins that an edge block does not walk the pool:
// every account behind the same egress hits it, so rotating spends each
// account's budget on the same refusal.
func TestEdgeBlockIsNotRotatable(t *testing.T) {
	if Rotatable(ClassEdgeBlock) {
		t.Fatal("an edge block must not rotate accounts")
	}
	if !Rotatable(ClassWAF) {
		t.Fatal("a plain 403 keeps rotating (its cause may be the credential)")
	}
}
