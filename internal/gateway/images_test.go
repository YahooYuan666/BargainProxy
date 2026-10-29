package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

// imageDriver fakes the antigravity image capability. failSoft/failBad count
// how many of the first calls fail with a rotatable / non-rotatable error.
type imageDriver struct {
	calls    int
	failSoft int
	failBad  int
}

func (d *imageDriver) Channel() string { return model.ChannelAntigravity }
func (d *imageDriver) Models(context.Context) ([]string, error) {
	return []string{model.ImageGenModel}, nil
}
func (d *imageDriver) Stream(context.Context, driver.ChatCall, driver.Emit) (*ir.ChatResponse, error) {
	return nil, uperr.New(uperr.ClassBadRequest, 400, "images do not stream")
}
func (d *imageDriver) GenerateImage(context.Context, *model.Account, string, string) (*driver.ImageResult, error) {
	d.calls++
	if d.failBad >= d.calls {
		return nil, uperr.New(uperr.ClassBadRequest, 400, "antigravity: image generation rejected: no")
	}
	if d.failSoft >= d.calls {
		return nil, uperr.New(uperr.ClassSoftRate, 429, "slow down")
	}
	return &driver.ImageResult{Images: []driver.ImagePart{{MIME: "image/jpeg", B64: "aW1n"}}}, nil
}

func newImageGateway(t *testing.T, drv driver.Driver, accounts int) *Gateway {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second},
		map[string]int{model.ChannelAntigravity: 3},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	for i := 0; i < accounts; i++ {
		p.Upsert(&model.Account{ID: string(rune('a'+i)) + "-ag", Channel: model.ChannelAntigravity})
	}
	return New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelAntigravity: drv}, st)
}

// BP-186: one upstream call makes one image; a rotatable failure moves to the
// next account, and the failed account carries the cooldown.
func TestGenerateImagesRotatesOnRotatableFailure(t *testing.T) {
	drv := &imageDriver{failSoft: 1}
	g := newImageGateway(t, drv, 2)

	out, err := g.GenerateImages(context.Background(), model.ImageGenModel, "a cat", 1)
	if err != nil {
		t.Fatalf("GenerateImages: %v", err)
	}
	if len(out.Images) != 1 || out.Images[0].B64 != "aW1n" {
		t.Fatalf("images = %+v", out.Images)
	}
	if drv.calls != 2 {
		t.Fatalf("driver calls = %d, want 2 (rotate after soft rate)", drv.calls)
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", out.Attempts)
	}
	// The failed account must be cooled for the image model — that is what
	// makes the NEXT request skip it instead of re-paying the failure.
	p := g.Pool()
	for _, acc := range p.Accounts(model.ChannelAntigravity) {
		if acc.ID == "a-ag" {
			if _, ok := acc.ModelCooldowns[model.ImageGenModel]; !ok {
				t.Fatalf("failed account %s carries no model cooldown", acc.ID)
			}
		}
	}
}

// A bad request (refusal, malformed prompt) is the same verdict for every
// account — rotation would only spread cooldowns, so it stops immediately.
func TestGenerateImagesStopsOnBadRequest(t *testing.T) {
	drv := &imageDriver{failBad: 1}
	g := newImageGateway(t, drv, 3)

	out, err := g.GenerateImages(context.Background(), model.ImageGenModel, "x", 1)
	if err == nil {
		t.Fatal("want the refusal error")
	}
	if !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("error = %v", err)
	}
	if drv.calls != 1 || out.Attempts != 1 {
		t.Fatalf("calls/attempts = %d/%d, want 1/1 (bad_request does not rotate)", drv.calls, out.Attempts)
	}
}

// n images = n serial upstream calls; attempts accumulate across them.
func TestGenerateImagesSerialPerImage(t *testing.T) {
	drv := &imageDriver{}
	g := newImageGateway(t, drv, 1)

	out, err := g.GenerateImages(context.Background(), model.ImageGenModel, "x", 3)
	if err != nil {
		t.Fatalf("GenerateImages: %v", err)
	}
	if len(out.Images) != 3 || drv.calls != 3 || out.Attempts != 3 {
		t.Fatalf("images/calls/attempts = %d/%d/%d, want 3/3/3", len(out.Images), drv.calls, out.Attempts)
	}
}

// No accounts → the honest 429 instead of an obscure driver failure.
func TestGenerateImagesWithoutAccounts(t *testing.T) {
	drv := &imageDriver{}
	g := newImageGateway(t, drv, 0)

	_, err := g.GenerateImages(context.Background(), model.ImageGenModel, "x", 1)
	if err == nil {
		t.Fatal("want no-account error")
	}
	if !strings.Contains(err.Error(), "no account") {
		t.Fatalf("error = %v", err)
	}
	if drv.calls != 0 {
		t.Fatalf("driver called %d times with an empty pool", drv.calls)
	}
}

// Chat requests naming the image model get pointed at the images endpoint
// instead of a bare "model not found" (the chat path cannot carry images).
func TestImageModelGateBlocksChat(t *testing.T) {
	drv := &imageDriver{}
	g := newImageGateway(t, drv, 1)

	req := &ir.ChatRequest{
		Model: model.ImageGenModel, Stream: true,
		Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}},
	}
	_, err := g.Execute(context.Background(), req, func(ir.StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("chat on the image model must be gated")
	}
	if !strings.Contains(err.Error(), "/v1/images/generations") {
		t.Fatalf("error = %v, want the endpoint hint", err)
	}
	if drv.calls != 0 {
		t.Fatalf("image driver invoked %d times from the chat path", drv.calls)
	}
}

// The image model appears in /v1/models only while the channel can serve it.
func TestImageModelExposedWhileServable(t *testing.T) {
	drv := &imageDriver{}
	g := newImageGateway(t, drv, 1)
	found := false
	for _, mi := range g.ListModels() {
		if mi.Model == model.ImageGenModel {
			found = true
		}
	}
	if !found {
		t.Fatal("image model missing from ListModels with a servable channel")
	}

	empty := newImageGateway(t, drv, 0)
	for _, mi := range empty.ListModels() {
		if mi.Model == model.ImageGenModel {
			t.Fatal("image model listed with an empty pool")
		}
	}
}
