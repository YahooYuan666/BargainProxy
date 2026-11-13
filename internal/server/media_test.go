package server_test

// BP-306: the images endpoint serves every catalog channel, clamps numbers,
// and answers capability gaps with models_that_can (switch-model advice).
// The gateway video endpoints return 202 + a pollable task id.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bargainproxy/internal/media"
)

func TestImagesCapabilityNotSupported(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"model":"qwen-image-3","prompt":"x","aspect_ratio":"16:9"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
	e, _ := out["error"].(map[string]any)
	if e == nil || e["code"] != "capability_not_supported" {
		t.Fatalf("error = %v, want capability_not_supported", out)
	}
	list, _ := e["models_that_can"].([]any)
	if len(list) == 0 {
		t.Fatalf("models_that_can empty: %v", e)
	}
	for _, id := range list {
		if id == "qwen-image-3" {
			t.Fatalf("offending model listed as capable: %v", list)
		}
	}
}

func TestImagesEnumClampAndValue(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	// n overflow clamps to the model max and reports it.
	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"model":"nano-banana-2","prompt":"x","n":99}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	applied, _ := out["applied"].(map[string]any)
	if applied == nil || applied["n"] != float64(99) {
		t.Fatalf("applied = %v, want original n recorded", out["applied"])
	}
	// size enum violation on a model that HAS the param: invalid_parameter_value.
	resp2, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"model":"qwen-image-3","prompt":"x","size":"1K"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 map[string]any
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	e, _ := out2["error"].(map[string]any)
	if e == nil || e["code"] != "invalid_parameter_value" {
		t.Fatalf("error = %v, want invalid_parameter_value", out2)
	}
	// qwen's size is a free-form param validated by Pattern (BP-316 era), so
	// the answer carries the machine-readable rule — enum params carry allowed.
	if _, ok := e["rule"].(map[string]any); !ok {
		if _, ok2 := e["allowed"].([]any); !ok2 {
			t.Fatalf("neither rule nor allowed present: %v", e)
		}
	}
}

func TestVideoSubmitValidationAndTask(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	// Unknown model → 404 pointing at the catalog.
	resp, err := http.Post(srv.URL+"/v1/videos/generations", "application/json",
		strings.NewReader(`{"model":"sora-9","prompt":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown video model status = %d", resp.StatusCode)
	}
	// Browser-gated model → 501 with the reason.
	resp2, _ := http.Post(srv.URL+"/v1/videos/generations", "application/json",
		strings.NewReader(`{"model":"veo-3-1","prompt":"x"}`))
	vbody := new(strings.Builder)
	_, _ = io.Copy(vbody, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode < 400 || !strings.Contains(vbody.String(), "browser") {
		t.Fatalf("veo = %d %s, want >=400 naming the browser gate", resp2.StatusCode, vbody.String())
	}
	// Capability gap on a wired model → 400 naming models that can.
	resp3, err := http.Post(srv.URL+"/v1/videos/generations", "application/json",
		strings.NewReader(`{"model":"kling-video-v3","prompt":"x","aspect_ratio":"21:9"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("kling 21:9 status = %d, want 400", resp3.StatusCode)
	}
	// Tail frame without a first frame → 400 (vendor lastFrameRequiresFirstFrame).
	resp4, err := http.Post(srv.URL+"/v1/videos/generations", "application/json",
		strings.NewReader(`{"model":"minimax-video-h3","prompt":"x","last_frame":"data:image/png;base64,AA"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp4.Body.Close()
	if resp4.StatusCode != http.StatusBadRequest {
		t.Fatalf("last_frame without first_frame status = %d, want 400", resp4.StatusCode)
	}
	// Unknown task id → 404.
	miss, err := http.Get(srv.URL + "/v1/videos/generations/vtask-nope")
	if err != nil {
		t.Fatal(err)
	}
	defer miss.Body.Close()
	if miss.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown task status = %d", miss.StatusCode)
	}
}

// The rejection must never list the rejected model itself, and every listed
// model must genuinely declare the capability (sess_4041: seedream-5-pro was
// refused for aspect_ratio while appearing in its own models_that_can).
func TestModelsThatCanExcludeRejected(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"model":"qwen-image-3","prompt":"x","aspect_ratio":"16:9"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	e, _ := out["error"].(map[string]any)
	list, _ := e["models_that_can"].([]any)
	if len(list) == 0 {
		t.Fatal("models_that_can empty")
	}
	for _, id := range list {
		name, _ := id.(string)
		if name == "qwen-image-3" {
			t.Fatalf("rejected model listed in its own models_that_can: %v", list)
		}
		m, ok := media.ByID(name)
		if !ok {
			t.Fatalf("models_that_can entry %q is not in the catalog", name)
		}
		supported := false
		for _, p := range m.Params {
			if p.Name == "aspect_ratio" && p.Supported {
				supported = true
			}
		}
		if !supported {
			t.Fatalf("models_that_can entry %q does not declare aspect_ratio", name)
		}
	}
}

// seedream-5-pro's vendor spec DECLARES aspectRatio but measurably ignores it
// (2026-11-13 probe: 3:2 in → 2048×2048 out), so the catalog marks it
// unsupported and the preflight must reject with an honest models_that_can —
// switch-model advice naming only models that truly take it, never the
// rejected model itself.
func TestSeedreamPro3x2RejectedWithHonestList(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json",
		strings.NewReader(`{"model":"seedream-5-pro","prompt":"x","size":"2848x1600","aspect_ratio":"3:2"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	e, _ := out["error"].(map[string]any)
	code, _ := e["code"].(string)
	if code != "capability_not_supported" {
		t.Fatalf("code = %q, want capability_not_supported", code)
	}
	list, _ := e["models_that_can"].([]any)
	if len(list) != 1 || list[0] != "minimax-h3-image" {
		t.Fatalf("models_that_can = %v, want only minimax-h3-image", list)
	}
}

// qwen/minimax accept custom W*H inside vendor pixel/ratio bounds — the enum
// the spec's UI shows is narrower than the wire contract. sess_0999c885: the
// card kept steering to 2368*1728 because the enum had no true 3:2/16:9.
func postImage(t *testing.T, srv *httptest.Server, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/images/generations", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func errCode(t *testing.T, out map[string]any) string {
	t.Helper()
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func TestQwenCustomSizePassesPreflight(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	code, out := postImage(t, srv, `{"model":"qwen-image-3-pro","prompt":"x","size":"2048*1365"}`)
	if c := errCode(t, out); c == "invalid_parameter_value" || c == "capability_not_supported" {
		t.Fatalf("custom in-bounds size rejected: %d %v", code, out)
	}
}

func TestQwenCustomSizeBoundsAndFormat(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	// pixels over the 2048^2 ceiling
	code, out := postImage(t, srv, `{"model":"qwen-image-3-pro","prompt":"x","size":"4000*4000"}`)
	if code != http.StatusBadRequest || errCode(t, out) != "invalid_parameter_value" {
		t.Fatalf("over-pixels size: %d %v", code, out)
	}
	// ratio beyond 1:8 (pixels fine)
	code, out = postImage(t, srv, `{"model":"qwen-image-3-pro","prompt":"x","size":"2048*200"}`)
	if code != http.StatusBadRequest || errCode(t, out) != "invalid_parameter_value" {
		t.Fatalf("over-ratio size: %d %v", code, out)
	}
	// wrong separator for this model (x instead of *)
	code, out = postImage(t, srv, `{"model":"qwen-image-3-pro","prompt":"x","size":"2048x1365"}`)
	if code != http.StatusBadRequest || errCode(t, out) != "invalid_parameter_value" {
		t.Fatalf("wrong separator: %d %v", code, out)
	}
	if _, ok := out["error"].(map[string]any)["rule"]; !ok {
		t.Fatal("rule object missing from custom-size rejection")
	}
}

func TestMinimaxCustomSizePassesPreflight(t *testing.T) {
	drv := &imagesDriver{}
	srv := buildImagesServer(t, drv, 1)
	code, out := postImage(t, srv, `{"model":"minimax-h3-image","prompt":"x","size":"3000x2000"}`)
	if c := errCode(t, out); c == "invalid_parameter_value" || c == "capability_not_supported" {
		t.Fatalf("minimax custom size rejected: %d %v", code, out)
	}
}
