package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
)

// BP-313: an overweight request's images get fitted — oldest first, down to
// three quarters of the budget, the latest user message untouched — before
// the byte gate sees the request. Everything else stays byte-identical:
// under-budget requests, pinned requests, and any image the fit cannot
// process faithfully (EXIF orientation, ICC, animation, decode failure).

// bp313Img is one image part the driver saw.
type bp313Img struct {
	mime string
	b64  string
}

type bp313Driver struct {
	channel string
	calls   *int
	seen    *[]bp313Img
}

func (d *bp313Driver) Channel() string                          { return d.channel }
func (d *bp313Driver) Models(context.Context) ([]string, error) { return []string{"m"}, nil }

func (d *bp313Driver) Stream(ctx context.Context, call driver.ChatCall, emit driver.Emit) (*ir.ChatResponse, error) {
	*d.calls++
	for _, m := range call.Request.Messages {
		for _, p := range m.Content {
			if p.Kind == "image" {
				*d.seen = append(*d.seen, bp313Img{mime: p.ImageMIME, b64: p.ImageB64})
			}
		}
	}
	_ = emit(ir.StreamEvent{Kind: ir.EvText, Delta: "ok"})
	_ = emit(ir.StreamEvent{Kind: ir.EvDone, Stop: ir.StopEnd})
	return &ir.ChatResponse{
		Message:    ir.Message{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		StopReason: ir.StopEnd,
	}, nil
}

func newBP313Gateway(t *testing.T, budget int64) (*Gateway, *[]bp313Img, *int) {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	cfg.ImageBudgetBytes = budget
	cfg.Models = []config.ModelBinding{
		{Canonical: "m", Channel: "workbuddy", UpstreamModel: "m"},
	}
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second}, nil,
		func(a *model.Account) { _ = st.SaveAccount(a) })
	p.Upsert(&model.Account{ID: "wb-1", Channel: "workbuddy", CredsJSON: []byte(`{}`)})
	seen := &[]bp313Img{}
	calls := new(int)
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{
		"workbuddy": &bp313Driver{channel: "workbuddy", calls: calls, seen: seen},
	}, st), seen, calls
}

// bp313PNG builds an opaque (or translucent) PNG's base64 with content whose
// JPEG re-encode crushes it: a smooth gradient over blocky stripes.
func bp313PNG(w, h int, alpha byte) (string, string) {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			// screenshot-like content: large flat panels, a smooth vertical
			// ramp, and low-amplitude per-pixel antialiasing detail — PNG
			// must encode the detail exactly (big), JPEG quantizes it away
			// (small), which is the ratio that makes the fit effective.
			hz := uint32(x*73856093) ^ uint32(y*19349663)
			hz ^= hz >> 13
			hz *= 0x5bd1e995
			hz ^= hz >> 15
			d := uint8(hz) & 0x07
			img.Pix[i] = uint8(((x/96+y/96)%2)*170) + 15 + d/2
			img.Pix[i+1] = uint8((y*255)/h)/3 + 40 + d/3
			img.Pix[i+2] = 60 + d/4
			img.Pix[i+3] = alpha
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return "image/png", base64.StdEncoding.EncodeToString(buf.Bytes())
}

// bp313JPEG builds a JPEG's base64 above the resolution cap.
func bp313JPEG(w, h int, q int) (string, string) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := img.PixOffset(x, y)
			hz := uint32(x*73856093) ^ uint32(y*19349663)
			hz ^= hz >> 13
			hz *= 0x5bd1e995
			hz ^= hz >> 15
			d := uint8(hz >> 8)
			img.Pix[i] = uint8(((x/64+y/64)%2)*180) + 20 + d/3
			img.Pix[i+1] = uint8((y*255)/h)/2 + 20 + d/3
			img.Pix[i+2] = 40 + d/4
			img.Pix[i+3] = 0xFF
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		panic(err)
	}
	return "image/jpeg", base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestBP313UnderBudgetUntouched: a request under the budget must reach the
// driver byte-for-byte — the fit never even looks at it.
func TestBP313UnderBudgetUntouched(t *testing.T) {
	mimeA, b64A := bp313PNG(2200, 1200, 0xFF)
	req := &ir.ChatRequest{Model: "m", Stream: true, MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "text", Text: "hi"},
			{Kind: "image", ImageMIME: mimeA, ImageB64: b64A},
		}}},
	}
	budget := int64(ir.EstimateRequestBytes(req)) + 1024
	g, seen, calls := newBP313Gateway(t, budget)
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *calls == 0 {
		t.Fatalf("driver not called")
	}
	if len(*seen) != 1 || (*seen)[0].b64 != b64A {
		t.Fatalf("under-budget request was modified: seen=%d", len(*seen))
	}
}

// TestBP313FitsOldestFirstAndStops: three over-cap images, the last one in
// the current turn. The budget is sized so fitting the OLDEST image alone
// reaches the target — so the middle image must stay untouched (the walk
// stops before it) and the current turn must stay untouched (the fit never
// touches it).
func TestBP313FitsOldestFirstAndStops(t *testing.T) {
	mimeA, b64A := bp313PNG(2600, 1800, 0xFF)
	mimeB, b64B := bp313PNG(2100, 1100, 0xFF)
	mimeC, b64C := bp313PNG(2400, 1500, 0xFF)
	req := &ir.ChatRequest{Model: "m", Stream: true, MaxTokens: 10, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "old"}, {Kind: "image", ImageMIME: mimeA, ImageB64: b64A}}},
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "mid"}, {Kind: "image", ImageMIME: mimeB, ImageB64: b64B}}},
		{Role: ir.RoleAssistant, Content: []ir.ContentPart{{Kind: "text", Text: "ok"}}},
		{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "current"}, {Kind: "image", ImageMIME: mimeC, ImageB64: b64C}}},
	}}
	// The fitted form of A, measured through the same code the walk uses.
	_, fittedA, changed := fitImage(mimeA, b64A, true)
	if !changed {
		t.Fatalf("fixture: A must be fittable")
	}
	est := int64(ir.EstimateRequestBytes(req)) // captured BEFORE Execute mutates req
	// Trigger: est > budget. Stop after A: target = 3/4·budget ≥ est - |A| + |A'|.
	budget := (est - int64(len(b64A)) + int64(len(fittedA))) * 4 / 3
	g, seen, calls := newBP313Gateway(t, budget+8)
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *calls == 0 {
		t.Fatalf("driver not called")
	}
	if est <= budget+8 {
		t.Fatalf("fixture: request must start over budget")
	}
	if len(*seen) != 3 {
		t.Fatalf("driver saw %d images, want 3", len(*seen))
	}
	a, b, c := (*seen)[0], (*seen)[1], (*seen)[2]
	if len(a.b64) >= len(b64A) {
		t.Fatalf("oldest image not fitted: %d ≥ %d", len(a.b64), len(b64A))
	}
	if b.b64 != b64B {
		t.Fatalf("middle image was touched (fit did not stop after the oldest): %d → %d", len(b64B), len(b.b64))
	}
	if c.b64 != b64C || c.mime != mimeC {
		t.Fatalf("current-turn image was touched: %d → %d", len(b64C), len(c.b64))
	}
}

// TestBP313DeterministicAndCached: the same image replayed on the next turn
// (what ZCode does with its whole history) produces byte-identical output
// from the cache — a second run must not decode or encode anything again.
func TestBP313DeterministicAndCached(t *testing.T) {
	mimeA, b64A := bp313PNG(2300, 1300, 0xFF) // unique size → unique cache key
	req := &ir.ChatRequest{Model: "m", Stream: true, MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: mimeA, ImageB64: b64A},
		}}},
	}
	g, seen, calls := newBP313Gateway(t, 1) // budget 1: always over, fit everything eligible
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute 1: %v", err)
	}
	first := (*seen)[0]
	if first.b64 == b64A {
		t.Fatalf("fixture: first run must fit the image")
	}
	before := imageFitRefits.Load()
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute 2: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("driver called %d times, want 2", *calls)
	}
	if after := imageFitRefits.Load(); after != before {
		t.Fatalf("second run re-encoded the image (cache miss): refits %d → %d", before, after)
	}
	if (*seen)[len(*seen)-1] != first {
		t.Fatalf("second run produced different bytes — the upstream prefix cache would break every turn")
	}
}

// TestBP313RealPNGShrinksUnderCap: the fitted image the driver sees decodes
// to at most 2048px on the long edge and is smaller than what came in.
func TestBP313RealPNGShrinksUnderCap(t *testing.T) {
	mimeA, b64A := bp313PNG(2800, 1600, 0xFF)
	req := &ir.ChatRequest{Model: "m", Stream: true, MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: mimeA, ImageB64: b64A},
		}}},
	}
	g, seen, _ := newBP313Gateway(t, 1)
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := (*seen)[0]
	if len(got.b64) >= len(b64A) {
		t.Fatalf("fitted image is not smaller: %d ≥ %d", len(got.b64), len(b64A))
	}
	raw, err := base64.StdEncoding.DecodeString(got.b64)
	if err != nil {
		t.Fatalf("driver saw invalid base64: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("driver saw undecodable image: %v", err)
	}
	if max(cfg.Width, cfg.Height) > 2048 {
		t.Fatalf("fitted image is %dx%d, long edge must be ≤2048", cfg.Width, cfg.Height)
	}
}

// TestBP313AlphaPNGStaysPNG: transparency must survive — an alpha image is
// never handed to the JPEG encoder.
func TestBP313AlphaPNGStaysPNG(t *testing.T) {
	mimeA, b64A := bp313PNG(2200, 1200, 0x80)
	req := &ir.ChatRequest{Model: "m", Stream: true, MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: mimeA, ImageB64: b64A},
		}}},
	}
	g, seen, _ := newBP313Gateway(t, 1)
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := (*seen)[0]
	if got.mime != "image/png" {
		t.Fatalf("alpha image converted to %s", got.mime)
	}
	if len(got.b64) >= len(b64A) {
		t.Fatalf("alpha image not downsampled: %d ≥ %d", len(got.b64), len(b64A))
	}
}

// TestBP313SmallJPEGUntouched: a JPEG already at or under the cap is never
// re-encoded — that would be generation loss for nothing.
func TestBP313SmallJPEGUntouched(t *testing.T) {
	mimeA, b64A := bp313JPEG(2048, 1152, 100) // at the cap, big enough to be eligible
	if len(b64A) < 256<<10 {
		t.Fatalf("fixture: JPEG too small to be eligible (%d bytes)", len(b64A))
	}
	req := &ir.ChatRequest{Model: "m", Stream: true, MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: mimeA, ImageB64: b64A},
		}}},
	}
	g, seen, _ := newBP313Gateway(t, 1)
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*seen)[0].b64 != b64A {
		t.Fatalf("JPEG at the cap was re-encoded (generation loss)")
	}
}

// TestBP313PinnedUntouched: a pinned request keeps its honest answer — the
// fit never runs on it, whatever it weighs.
func TestBP313PinnedUntouched(t *testing.T) {
	mimeA, b64A := bp313PNG(2600, 1800, 0xFF)
	req := &ir.ChatRequest{Model: "channel:workbuddy:m", Stream: true, MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: mimeA, ImageB64: b64A},
		}}},
	}
	g, seen, calls := newBP313Gateway(t, 1)
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if *calls == 0 {
		t.Fatalf("driver not called")
	}
	if (*seen)[0].b64 != b64A {
		t.Fatalf("pinned request was modified")
	}
}

// TestBP313ExifJPEGUntouched: a JPEG whose EXIF says "rotate me" goes as it
// came even when over the cap — a plain re-encode would bake the wrong
// rotation in and then drop the tag.
func TestBP313ExifJPEGUntouched(t *testing.T) {
	mimeA, b64A := bp313JPEG(2600, 1500, 90)
	raw, err := base64.StdEncoding.DecodeString(b64A)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	// APP1 segment: "Exif\0\0" + little-endian TIFF with IFD0 orientation=6.
	tiff := []byte{
		'I', 'I', 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, // header, IFD0 at 8
		0x01, 0x00, // one entry
		0x12, 0x01, 0x03, 0x00, 0x01, 0x00, 0x00, 0x00, 0x06, 0x00, 0x00, 0x00, // orientation 6
		0x00, 0x00, 0x00, 0x00, // no next IFD
	}
	exif := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xFF, 0xE1, byte((len(exif) + 2) >> 8), byte(len(exif) + 2)}
	withExif := append(append(append([]byte{}, raw[:2]...), app1...), exif...)
	withExif = append(withExif, raw[2:]...)
	b64 := base64.StdEncoding.EncodeToString(withExif)
	req := &ir.ChatRequest{Model: "m", Stream: true, MaxTokens: 10,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{
			{Kind: "image", ImageMIME: mimeA, ImageB64: b64},
		}}},
	}
	g, seen, _ := newBP313Gateway(t, 1)
	if _, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if (*seen)[0].b64 != b64 {
		t.Fatalf("JPEG with EXIF orientation was re-encoded — rotation would be lost")
	}
}
