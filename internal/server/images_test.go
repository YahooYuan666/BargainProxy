package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/gateway"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/pool"
	"bargainproxy/internal/router"
	"bargainproxy/internal/server"
	"bargainproxy/internal/store"
	"bargainproxy/internal/uperr"
)

// imagesDriver fakes the antigravity channel both as a (never-used) chat
// driver and as an image generator that counts its calls.
type imagesDriver struct {
	calls   int
	prompts []string
}

func (d *imagesDriver) Channel() string { return model.ChannelAntigravity }
func (d *imagesDriver) Models(context.Context) ([]string, error) {
	return []string{model.ImageGenModel}, nil
}
func (d *imagesDriver) Stream(context.Context, driver.ChatCall, driver.Emit) (*ir.ChatResponse, error) {
	return nil, uperr.New(uperr.ClassBadRequest, 400, "images do not stream")
}
func (d *imagesDriver) GenerateImage(_ context.Context, _ *model.Account, _, prompt string) (*driver.ImageResult, error) {
	d.calls++
	d.prompts = append(d.prompts, prompt)
	return &driver.ImageResult{Images: []driver.ImagePart{{MIME: "image/jpeg", B64: "aW1nLWJ5dGVz"}}}, nil
}

func buildImagesServer(t *testing.T, drv *imagesDriver, accounts int) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	cfg.Store = "memory"
	cfg.DataDir = t.TempDir()
	st := store.NewMemory()
	p := pool.New(pool.Params{MaxQueueWait: 2 * time.Second},
		map[string]int{model.ChannelAntigravity: 3},
		func(acc *model.Account) { _ = st.SaveAccount(acc) })
	for i := 0; i < accounts; i++ {
		p.Upsert(&model.Account{ID: fmt.Sprintf("ag-%d", i), Channel: model.ChannelAntigravity})
	}
	gw := gateway.New(cfg, p, router.New(cfg), map[string]driver.Driver{model.ChannelAntigravity: drv}, st)
	srv := httptest.NewServer(server.New(cfg, gw).Handler())
	t.Cleanup(srv.Close)
	return srv
}

// The happy path: OpenAI images shape, one upstream call per image, defaults
// applied (model + b64_json), url format rendered as an inline data: URI.
func TestImagesHappyPathShapes(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)

	post := func(body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	code, out := post(`{"model":"gemini-3.1-flash-image","prompt":"a cat","n":2}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, out)
	}
	if drv.calls != 2 {
		t.Fatalf("driver calls = %d, want 2 (n=2)", drv.calls)
	}
	data, _ := out["data"].([]any)
	if len(data) != 2 {
		t.Fatalf("data = %v, want 2 entries", out["data"])
	}
	first, _ := data[0].(map[string]any)
	if first["b64_json"] != "aW1nLWJ5dGVz" {
		t.Fatalf("b64_json = %v", first["b64_json"])
	}
	if _, ok := out["created"].(float64); !ok {
		t.Fatalf("created missing: %v", out)
	}

	code, out = post(`{"prompt":"a cat","response_format":"url"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d: %v", code, out)
	}
	if drv.calls != 3 {
		t.Fatalf("driver calls = %d, want 3 (model defaulted, n defaulted to 1)", drv.calls)
	}
	data, _ = out["data"].([]any)
	first, _ = data[0].(map[string]any)
	if !strings.HasPrefix(first["url"].(string), "data:image/jpeg;base64,") {
		t.Fatalf("url = %v", first["url"])
	}
}

// The narrow contract is enforced client-side with the reference's messages.
func TestImagesValidation(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)

	post := func(body string) (int, string) {
		resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw := new(strings.Builder)
		_, _ = io.Copy(raw, resp.Body)
		return resp.StatusCode, raw.String()
	}

	code, body := post(`{"model":"dall-e-3","prompt":"x"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "gemini-3.1-flash-image") {
		t.Fatalf("unknown model: %d %s", code, body)
	}
	code, body = post(`{"model":"gemini-3.1-flash-image"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "prompt is required") {
		t.Fatalf("missing prompt: %d %s", code, body)
	}
	code, body = post(`{"prompt":"x","response_format":"png"}`)
	if code != http.StatusBadRequest || !strings.Contains(body, "b64_json") {
		t.Fatalf("bad response_format: %d %s", code, body)
	}
	// n clamps to the cap: 99 must not hit the upstream 99 times.
	code, _ = post(`{"prompt":"x","n":99}`)
	if code != http.StatusOK || drv.calls != 4 {
		t.Fatalf("n clamp: status=%d calls=%d, want 200/4", code, drv.calls)
	}
}

// Chat requests naming the image model are gated with the endpoint hint, and
// the model shows up in /v1/models while the channel has accounts.
func TestImagesModelGateAndExposure(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gemini-3.1-flash-image","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := new(strings.Builder)
	_, _ = io.Copy(raw, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(raw.String(), "/v1/images/generations") {
		t.Fatalf("chat gate: %d %s", resp.StatusCode, raw.String())
	}
	if drv.calls != 0 {
		t.Fatalf("image driver used by the chat path %d times", drv.calls)
	}

	list, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	_ = json.NewDecoder(list.Body).Decode(&doc)
	list.Body.Close()
	found := false
	for _, m := range doc.Data {
		if m.ID == model.ImageGenModel {
			found = true
			if m.OwnedBy != model.ChannelAntigravity {
				t.Fatalf("owned_by = %q", m.OwnedBy)
			}
		}
	}
	if !found {
		t.Fatal("image model not listed in /v1/models")
	}
}

// An empty pool answers 429 with the honest reason, and the model disappears
// from /v1/models.
func TestImagesWithoutAccounts(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 0)

	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw := new(strings.Builder)
	_, _ = io.Copy(raw, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(raw.String(), "antigravity") {
		// DescribeError renders the pool-wide wording for quota_exhausted
		// ("all accounts are cooling down or exhausted on <channel>"); the
		// specific "no account" reason is pinned at the gateway layer.
		t.Fatalf("empty pool: %d %s", resp.StatusCode, raw.String())
	}
}
