package anthropic

import (
	"bytes"
	"strings"
	"testing"

	"bargainproxy/internal/ir"
)

// BP-56: the Anthropic wire counts the prompt as
// input_tokens (uncached) + cache_creation + cache_read, while ir.Usage
// carries the OpenAI-style total plus a cached subset. The stream must report
// the split so context meters (ZCode's usage bar / auto-compact baseline)
// stop reading 0.
func TestStreamUsageCarriesPromptAndCacheSplit(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "glm-5.3-flash", 0)
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvText, Delta: "hi"}); err != nil {
		t.Fatalf("send text: %v", err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{
		InputTokens: 100, OutputTokens: 7, CachedInputTokens: 80, CacheKnown: true,
	}}); err != nil {
		t.Fatalf("send usage: %v", err)
	}
	if err := sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd}); err != nil {
		t.Fatalf("send done: %v", err)
	}
	out := buf.String()
	// message_delta carries the authoritative cumulative usage: the prompt
	// total split into uncached input (100-80) + cache read (80).
	if !strings.Contains(out, `"usage":{"input_tokens":20,"cache_read_input_tokens":80,"output_tokens":7}`) {
		t.Fatalf("message_delta usage missing the split:\n%s", out)
	}
}

// A reported zero cache hit is a real reading and must stay explicit (BP-53
// semantics), while an upstream that never reported cache accounting must not
// be turned into a fabricated 0.
func TestStreamUsageCacheUnknownOmitsCacheFields(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "m", 0)
	sw.Send(ir.StreamEvent{Kind: ir.EvText, Delta: "x"})
	sw.Send(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: 50, OutputTokens: 3}})
	sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	out := buf.String()
	if !strings.Contains(out, `"usage":{"input_tokens":50,"output_tokens":3}`) {
		t.Fatalf("unknown-cache usage must carry the plain total:\n%s", out)
	}
	if strings.Contains(out, "cache_read_input_tokens") {
		t.Fatalf("cache accounting was never reported; fields must stay absent:\n%s", out)
	}

	var buf2 bytes.Buffer
	sw2 := NewStreamWriter(&buf2, nil, "msg_2", "m", 0)
	sw2.Send(ir.StreamEvent{Kind: ir.EvText, Delta: "x"})
	sw2.Send(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{
		InputTokens: 50, OutputTokens: 3, CachedInputTokens: 0, CacheKnown: true,
	}})
	sw2.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	if !strings.Contains(buf2.String(), `"cache_read_input_tokens":0`) {
		t.Fatalf("a reported 0-hit request must stay distinguishable from unknown:\n%s", buf2.String())
	}
}

// The start placeholder comes from the request-side estimate (the same number
// count_tokens exposes), so a non-empty prompt never announces 0 tokens;
// message_delta then replaces it with the upstream's numbers.
func TestStreamStartCarriesPromptEstimate(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "m", 1234)
	sw.Send(ir.StreamEvent{Kind: ir.EvText, Delta: "x"})
	sw.Send(ir.StreamEvent{Kind: ir.EvUsage, Usage: &ir.Usage{InputTokens: 40, OutputTokens: 2}})
	sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	out := buf.String()
	if !strings.Contains(out, `"usage":{"input_tokens":1234,"output_tokens":0}`) {
		t.Fatalf("message_start must carry the estimate:\n%s", out)
	}
	if !strings.Contains(out, `"usage":{"input_tokens":40,"output_tokens":2}`) {
		t.Fatalf("message_delta must overwrite with the real numbers:\n%s", out)
	}
}

// Cancelled mid-stream (no upstream usage frame): the delta still carries the
// estimate instead of dropping the prompt to 0.
func TestStreamDeltaFallsBackToEstimateWithoutUpstreamUsage(t *testing.T) {
	var buf bytes.Buffer
	sw := NewStreamWriter(&buf, nil, "msg_1", "m", 777)
	sw.Send(ir.StreamEvent{Kind: ir.EvText, Delta: "x"})
	sw.Send(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	out := buf.String()
	if !strings.Contains(out, `"usage":{"input_tokens":777,"output_tokens":0}`) {
		t.Fatalf("delta fallback missing:\n%s", out)
	}
}

// The non-streamed path must use the same split (it already forwarded the
// total but dropped the cache accounting entirely).
func TestRenderMessageUsageSplitsCache(t *testing.T) {
	resp := &ir.ChatResponse{
		StopReason: ir.StopEnd,
		Usage:      ir.Usage{InputTokens: 100, OutputTokens: 4, CachedInputTokens: 80, CacheKnown: true},
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
	}
	out := RenderMessage("msg_1", "m", resp)
	if out.Usage.InputTokens != 20 {
		t.Fatalf("input_tokens = %d, want 20 (total minus cached)", out.Usage.InputTokens)
	}
	if out.Usage.CacheReadInputTokens == nil || *out.Usage.CacheReadInputTokens != 80 {
		t.Fatalf("cache_read_input_tokens = %v, want 80", out.Usage.CacheReadInputTokens)
	}
	// Upstream noise (cached > total) must clamp instead of emitting a
	// negative prompt count.
	noisy := RenderMessage("msg_2", "m", &ir.ChatResponse{
		StopReason: ir.StopEnd,
		Usage:      ir.Usage{InputTokens: 10, OutputTokens: 1, CachedInputTokens: 99, CacheKnown: true},
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}},
	})
	if noisy.Usage.InputTokens != 0 {
		t.Fatalf("clamp failed: %d", noisy.Usage.InputTokens)
	}
}
