// BP-64: the pre-flight gate skips structurally blocked bindings immediately
// and honours short cooldowns that end inside the first-byte window.
package gateway

import (
	"testing"
	"time"
)

func TestSkipUnavailable(t *testing.T) {
	now := time.Now()
	window := 8 * time.Second
	cases := []struct {
		name     string
		ok       bool
		earliest time.Time
		want     bool
	}{
		{"available", true, time.Time{}, false},
		{"structural block", false, time.Time{}, true},
		{"long cooldown", false, now.Add(2 * time.Hour), true},
		{"cooldown inside window", false, now.Add(2 * time.Second), false},
	}
	for _, c := range cases {
		if got := skipUnavailable(c.ok, c.earliest, now, window); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
