package qoder

import (
	"testing"

	"bargainproxy/internal/uperr"
)

// BP-125: Qoder wraps a business envelope inside the 403's message field
// (Zhengyuuuui/qoder2api internal/bridge/errors.go). When the inner doc says
// NOT queued, NO retryAfterSeconds, subCode RESOURCE_LIMIT, the account has
// no entitlement for this model — a per-model wall, not an auth failure and
// not an account-wide rate event.
func TestNestedResourceLimitIsModelScoped(t *testing.T) {
	body := `{"code":"403","message":"{\"code\":\"10605\",\"isQueued\":false,\"serviceAvailable\":false,\"retryAfterSeconds\":0,\"subCode\":\"RESOURCE_LIMIT\",\"message\":\"当前账号无此模型权限\"}"}`
	e := classifyQoder(403, body)
	if e.Class != uperr.ClassModelRate {
		t.Fatalf("class = %s, want model_rate", e.Class)
	}
}

// A genuinely queued 10605 keeps the queue path (SoftRate carrying busyCode
// in the message — the queue-wait machinery matches on it).
func TestQueued10605StaysQueuePath(t *testing.T) {
	body := `{"code":"403","message":"{\"code\":\"10605\",\"isQueued\":true,\"serviceAvailable\":false,\"retryAfterSeconds\":12,\"subCode\":\"RESOURCE_LIMIT\"}"}`
	e := classifyQoder(403, body)
	if e.Class != uperr.ClassSoftRate {
		t.Fatalf("class = %s, want soft_rate (queued)", e.Class)
	}
}

// A 403 without the nested RESOURCE_LIMIT marker still classifies as auth.
func TestPlain403StaysAuth(t *testing.T) {
	e := classifyQoder(403, `{"code":"403","message":"invalid token"}`)
	if e.Class != uperr.ClassAuth {
		t.Fatalf("class = %s, want auth", e.Class)
	}
}

// The raw helper: doubly-escaped and object-shaped messages both parse.
func TestQoderNestedPermissionShapes(t *testing.T) {
	if !qoderNestedPermission(`{"message":"{\"subCode\":\"RESOURCE_LIMIT\",\"isQueued\":false}"}`) {
		t.Fatal("escaped string message should parse")
	}
	if qoderNestedPermission(`{"message":"{\"subCode\":\"RESOURCE_LIMIT\",\"isQueued\":true}"}`) {
		t.Fatal("queued inner doc is not a permission wall")
	}
	if qoderNestedPermission(`{"message":"{\"subCode\":\"OTHER\"}"}`) {
		t.Fatal("other subCodes are not permission walls")
	}
	if qoderNestedPermission(`{"message":"invalid token"}`) {
		t.Fatal("non-JSON inner doc is not a permission wall")
	}
}
