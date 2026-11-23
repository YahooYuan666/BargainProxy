package config

import "testing"

// BP-336: the Hy4 family advertises a 300K default window on both realms
// (/v3/config contextWindow.defaultLength = 300000; supported lengths step up
// to 960000 but the gateway never negotiates that extension, so 300K is the
// honest number to publish).
func TestContextLengthHy4Family(t *testing.T) {
	for _, id := range []string{"hy4std", "hy4flash", "hy4flash-free"} {
		n, ok := ContextLength(id)
		if !ok {
			t.Fatalf("%s missing from the context-length table", id)
		}
		if n != 300000 {
			t.Errorf("%s context length = %d, want 300000", id, n)
		}
	}
	if n, ok := ContextLength("hy4free"); ok {
		t.Errorf("hy4free must not be advertised (alias only): got %d", n)
	}
}
