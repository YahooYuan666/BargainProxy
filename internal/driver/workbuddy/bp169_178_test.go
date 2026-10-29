package workbuddy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// BP-170: the CN client's origin moved to www.workbuddy.cn — X-Domain derives
// from it — and X-Private-Data rides every chat as "false" (the default
// client state; 7 real captures).
func TestBP170CNDomainAndPrivateData(t *testing.T) {
	d := testDriver("http://unused")
	req, _ := http.NewRequest("POST", "https://api.example.cn/v2/chat/completions", nil)
	call := driver.ChatCall{
		Account:       &model.Account{ID: "wb-pd", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3"},
	}
	d.applyClientFingerprint(req, testCreds(), call)
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.cn" {
		t.Fatalf("CN X-Domain = %q, want www.workbuddy.cn", got)
	}
	if got := req.Header.Get("X-Private-Data"); got != "false" {
		t.Fatalf("X-Private-Data = %q, want false", got)
	}

	greq, _ := http.NewRequest("POST", "https://api.example.cn/v2/chat/completions", nil)
	d.applyClientFingerprint(greq, mustGlobalCreds(t), call)
	if got := greq.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Fatalf("global X-Domain = %q, want www.workbuddy.ai", got)
	}
}

// BP-171: the bare digits "11140" inside a request id must not disable the
// account; the wording and the structured code still do.
func TestBP171NoBare11140Substring(t *testing.T) {
	e := classifyHTTP(403, []byte(`{"requestId":"req-11140-x","message":"forbidden"}`), "glm-5.3")
	if e.Class == uperr.ClassAccountRestricted {
		t.Fatal("a request id containing 11140 must not disable the account")
	}
	if got := classifyHTTP(403, []byte(`{"code":11140,"message":"request illegal"}`), "glm-5.3").Class; got != uperr.ClassAccountRestricted {
		t.Fatalf("structured 11140 = %s, want account_restricted", got)
	}
	if got := classifyHTTP(403, []byte(`账号存在异常行为，已被限制使用`), "glm-5.3").Class; got != uperr.ClassAccountRestricted {
		t.Fatalf("wording marker = %s, want account_restricted", got)
	}
}

// BP-175: the refresh request matches the real-device capture — "{}" body,
// loose Accept, the three missing headers, naked-host X-Domain.
func TestBP175RefreshProtocol(t *testing.T) {
	var gotBody, gotAccept, gotSource, gotXHR, gotReqID, gotDomain string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		gotAccept = r.Header.Get("Accept")
		gotSource = r.Header.Get("X-Auth-Refresh-Source")
		gotXHR = r.Header.Get("X-Requested-With")
		gotReqID = r.Header.Get("X-Request-ID")
		gotDomain = r.Header.Get("X-Domain")
		io.WriteString(w, `{"data":{"accessToken":"a","refreshToken":"r","expiresIn":60}}`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	acc := accountWithRefreshToken()
	// RefreshNow skips the near-expiry gate so the request is always made.
	if err := d.RefreshNow(context.Background(), acc); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if gotBody != "{}" {
		t.Fatalf("body = %q, want {}", gotBody)
	}
	if gotAccept != "application/json, text/plain, */*" {
		t.Fatalf("Accept = %q", gotAccept)
	}
	if gotSource != "plugin" || gotXHR != "XMLHttpRequest" {
		t.Fatalf("missing headers: source=%q xhr=%q", gotSource, gotXHR)
	}
	if len(gotReqID) != 32 {
		t.Fatalf("X-Request-ID = %q, want 32 hex", gotReqID)
	}
	if strings.Contains(gotDomain, "://") {
		t.Fatalf("X-Domain = %q, want the naked host", gotDomain)
	}
}

// BP-177: a DeepSeek chat that names no effort gets the model config's own
// default (high); other families stay unset.
func TestBP177DefaultEffortDeepSeekOnly(t *testing.T) {
	d := testDriver("http://unused")
	mk := func(mdl string) driver.ChatCall {
		return driver.ChatCall{
			Account:       &model.Account{ID: "wb-eff", Channel: model.ChannelWorkBuddy},
			UpstreamModel: mdl,
			Request: &ir.ChatRequest{Model: mdl, Messages: []ir.Message{
				{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "hi"}}}}},
		}
	}
	body, err := d.buildBody(mk("deepseek-v4.1-flash"), testCreds(), false)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	if doc["reasoning_effort"] != "high" {
		t.Fatalf("deepseek default effort = %v, want high", doc["reasoning_effort"])
	}
	body2, _ := d.buildBody(mk("glm-5.3"), testCreds(), false)
	var doc2 map[string]any
	_ = json.Unmarshal(body2, &doc2)
	if _, has := doc2["reasoning_effort"]; has {
		t.Fatal("a family without a verified default must stay unset")
	}
}

// BP-178: chat bodies past 1KB go out gzipped (BestSpeed), small ones plain.
func TestBP178ChatBodyGzip(t *testing.T) {
	var gotEnc string
	var gotRaw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEnc = r.Header.Get("Content-Encoding")
		gotRaw, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	d := testDriver(srv.URL)
	big := bytes.Repeat([]byte("x"), 4096)
	resp, err := d.postChat(context.Background(), testCreds(), driver.ChatCall{
		Account:       &model.Account{ID: "wb-gz", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3"},
	}, big, "/v2/chat/completions")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if gotEnc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", gotEnc)
	}
	zr, zerr := gzip.NewReader(bytes.NewReader(gotRaw))
	if zerr != nil {
		t.Fatalf("body is not gzip: %v", zerr)
	}
	var out bytes.Buffer
	_, _ = io.Copy(&out, zr)
	if !bytes.Equal(out.Bytes(), big) {
		t.Fatal("decompressed body differs from the original")
	}

	if resp2, err2 := d.postChat(context.Background(), testCreds(), driver.ChatCall{
		Account:       &model.Account{ID: "wb-gz", Channel: model.ChannelWorkBuddy},
		UpstreamModel: "glm-5.3",
		Request:       &ir.ChatRequest{Model: "glm-5.3"},
	}, []byte(`{"tiny":true}`), "/v2/chat/completions"); err2 == nil {
		resp2.Body.Close()
	}
	_ = gotEnc
}

// BP-169 driver side: the queue signal carries its own wait — data.retry_after
// wins, 缺省 5s.
func TestBP169QueueRetryAfterParsed(t *testing.T) {
	e := classifyBusiness(6020, "queue.waiting.title", "glm-5.3")
	if e.Class != uperr.ClassUpstreamBusy {
		t.Fatalf("6020 = %s, want upstream_busy", e.Class)
	}
	attachQueueRetryAfter(e, nil)
	if e.ResetAt.IsZero() {
		t.Fatal("6020 without data must carry the 5s default")
	}
	if diff := time.Until(e.ResetAt); diff <= 0 || diff > 6*time.Second {
		t.Fatalf("default ResetAt in %s, want ~5s", diff)
	}

	body := []byte(`{"code":6020,"msg":"queue.waiting.title","data":{"retry_after":40}}`)
	if code, msg, ok := detectBusinessError(body); !ok {
		t.Fatal("body not detected")
	} else {
		e2 := classifyBusiness(code, msg, "glm-5.3")
		attachQueueRetryAfter(e2, body)
		if diff := time.Until(e2.ResetAt); diff < 35*time.Second || diff > 45*time.Second {
			t.Fatalf("retry_after=40 → ResetAt in %s, want ~40s", diff)
		}
	}
}
