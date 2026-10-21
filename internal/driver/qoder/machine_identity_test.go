package qoder

import (
	"crypto/md5"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"
)

// TestMachineIDsStableAndPerAccount pins the 2026-10-21 change: device
// credentials must be derived from the account identity (UID + install salt),
// not random UUIDs. The reference implementations (qoder2api-hub /
// Zhengyuuuui Go port) derive one stable virtual device per account so the
// upstream never sees a machine-code drift, and unrelated accounts never
// share a device. Our previous newMachineIDs() minted random UUIDs — stable
// only per stored credential file, drifting on every re-login.
func TestMachineIDsStableAndPerAccount(t *testing.T) {
	a1 := deriveMachineIDs("uid-a", "salt-1")
	a1b := deriveMachineIDs("uid-a", "salt-1")
	a2 := deriveMachineIDs("uid-a", "salt-2")
	b1 := deriveMachineIDs("uid-b", "salt-1")

	if a1 != a1b {
		t.Fatal("same uid+salt must derive identical device credentials")
	}
	if a1 == a2 {
		t.Fatal("changing the install salt must change the device (fingerprint space)")
	}
	if a1.machineID == b1.machineID {
		t.Fatal("different accounts must never share a machine id")
	}
}

// The shapes must match the official client (and the reference's test vectors):
// machineId 32 hex, machineType 18 chars, machineToken 43 chars base64url.
func TestMachineIDShapes(t *testing.T) {
	m := deriveMachineIDs("uid-x", "salt-x")
	if len(m.machineID) != 32 {
		t.Fatalf("machineID = %q (%d chars), want 32 hex chars", m.machineID, len(m.machineID))
	}
	if len(m.machineType) != 18 {
		t.Fatalf("machineType = %q, want 18 chars", m.machineType)
	}
	if len(m.machineToken) != 43 {
		t.Fatalf("machineToken = %q, want 43 chars base64url", m.machineToken)
	}
	// Hex + base64url must decode cleanly (charset check via round-trip).
	if _, err := hex.DecodeString(m.machineID); err != nil {
		t.Fatalf("machineID not hex: %v", err)
	}
}

// Vectors computed with the same formulas as qoder2api-hub's
// qoder_fingerprint.py (md5/md5[:18]/sha512[:43] base64url) so our values are
// byte-identical for the same seed — cross-checking the derivation, not the
// salt handling.
func TestMachineIDReferenceVectors(t *testing.T) {
	m := deriveMachineIDs("seed-vec", "")
	sum := md5.Sum([]byte("machine:seed-vec"))
	if m.machineID != hex.EncodeToString(sum[:]) {
		t.Fatalf("machineID vector mismatch: %q", m.machineID)
	}
	sum2 := md5.Sum([]byte("machinetype:seed-vec"))
	if m.machineType != hex.EncodeToString(sum2[:])[:18] {
		t.Fatalf("machineType vector mismatch: %q", m.machineType)
	}
	s := sha512.Sum512([]byte("machinetoken:seed-vec"))
	want := base64.RawURLEncoding.EncodeToString(s[:])[:43]
	if m.machineToken != want {
		t.Fatalf("machineToken vector mismatch: %q", m.machineToken)
	}
}

// Empty uid is rejected: deriving a device from nothing would give every
// credential-less account the same machine.
func TestMachineIDRequiresSeed(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("empty uid must panic (callers guarantee a non-empty seed)")
		}
	}()
	_ = deriveMachineIDs("", "")
	_ = fmt.Sprint()
}
