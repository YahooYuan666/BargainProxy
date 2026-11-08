package httpx

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// BP-298：终态后有界 drain——读尽剩余尾巴，且不超过上限。
func TestDrainReadsTail(t *testing.T) {
	src := io.NopCloser(strings.NewReader("\r\n" + "tail-bytes"))
	Drain(src)
	// 读完后再读应得到 EOF。
	if _, err := src.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("after drain: %v, want EOF", err)
	}
}

func TestDrainBounded(t *testing.T) {
	big := strings.Repeat("x", drainCap+1000)
	src := io.NopCloser(strings.NewReader(big))
	Drain(src)
	rest, err := io.ReadAll(src)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rest) != 1000 {
		t.Fatalf("drain consumed %d over the cap, want exactly %d left", drainCap, len(rest))
	}
}

func TestDrainNilSafe(t *testing.T) {
	Drain(nil) // 不许 panic
}
