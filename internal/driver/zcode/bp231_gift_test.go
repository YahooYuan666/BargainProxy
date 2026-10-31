package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// BP-231: the daily gift-plan claim (plugins #503 + 8ebbaa58/2e580cfe). The
// verdict rides the body code: 0 claimed, 1003 already, 3007 captcha (never
// a failure), anything else a retryable failure. Plans come from BOTH the
// billing preview and the marketing banner feed.
func TestBP231GiftClaim(t *testing.T) {
	var claims []string
	var claimBodies []string
	codes := map[string]int{
		"zcode-v3-start-plan-trust-1006": 0,    // claimed now
		"zcode-v3-start-plan-1029":       1003, // claimed before
		"zcode-weekend-banner":           3007, // captcha wall
		"zcode-broken":                   -1,   // weird code → failed
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case giftPreviewPath:
			// BP-231a: the billing edge rejects the call without the app's
			// identity family (3001 parameter error behind the ESA edge).
			if r.URL.Query().Get("app_version") != "3.16.3" || r.URL.Query().Get("platform") != "win32-x64" {
				t.Errorf("preview query = %q, want app_version+platform", r.URL.RawQuery)
			}
			for k, want := range map[string]string{
				"X-Device-Mid":         dashedDeviceMid,
				"X-ZCode-App-Version": "3.16.3",
				"X-Platform":          "win32-x64",
				"Authorization":       "Bearer tok",
			} {
				if got := r.Header.Get(k); got != want {
					t.Errorf("preview %s = %q, want %q", k, got, want)
				}
			}
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"plans":[`+
				`{"plan_id":"zcode-v3-start-plan-trust-1006","name":"ZCode Trust Build","status":"active"},`+
				`{"plan_id":"zcode-v3-start-plan-1029","name":"ZCODE GLOBAL BUILD","status":"active"}]}}`)
		case giftTouchPath:
			if r.URL.Query().Get("seq") == "" {
				t.Errorf("touch query = %q, want a seq counter", r.URL.RawQuery)
			}
			if got := r.Header.Get("X-Client-Language"); got == "" {
				t.Error("touch must carry X-Client-Language")
			}
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"deliveries":[{"campaign_id":"1","banner":{"buttons":[{"text":"领取","action":{"type":"claim_zcode_plan","args":{"plan_id":"zcode-weekend-banner"}}},{"action":{"type":"close","args":{}}}]}}]}}`)
		case giftClaimPath:
			var body struct {
				PlanID string `json:"plan_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			claims = append(claims, body.PlanID)
			raw, _ := json.Marshal(body)
			claimBodies = append(claimBodies, string(raw))
			switch codes[body.PlanID] {
			case 0:
				fmt.Fprint(w, `{"code":0,"msg":""}`)
			case 1003:
				fmt.Fprint(w, `{"code":1003,"msg":"target already exists","data":{"message":"You have already claimed this plan."}}`)
			case 3007:
				fmt.Fprint(w, `{"code":3007,"msg":"captcha verify failed"}`)
			default:
				fmt.Fprint(w, `{"code":9999,"msg":"weird"}`)
			}
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer up.Close()

	d := New(config.ZCodeSettings{Enabled: true, BaseURL: up.URL}, nil)
	acc := &model.Account{ID: "zc-1", Channel: model.ChannelZCode,
		CredsJSON: []byte(`{"jwt":"tok","uid":"u1"}`)}
	res, err := d.ClaimGiftPlans(context.Background(), acc)
	if err != nil {
		t.Fatalf("ClaimGiftPlans: %v", err)
	}
	if res.Claimed != 1 || res.Already != 1 || res.Captcha != 1 || res.Failed != 0 {
		t.Fatalf("counts = %+v, want 1/1/1/0", res)
	}
	// The banner-only plan must be in the claim set (it is not in preview).
	joined := strings.Join(claims, ",")
	for _, want := range []string{"trust-1006", "1029", "weekend-banner"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("claim set missing %s: %v", want, claims)
		}
	}
	// No captcha wall may read as a failure.
	for _, n := range res.Notes {
		if strings.Contains(n, "captcha") && !strings.Contains(n, "ZCode app") {
			t.Fatalf("captcha note must point at the app: %q", n)
		}
	}
}

// A preview whose only plan was claimed before reports Already and claims
// once more on the next run only if the server stops answering 1003.
func TestBP231GiftClaimIdempotent(t *testing.T) {
	var posts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case giftPreviewPath:
			fmt.Fprint(w, `{"code":0,"data":{"plans":[{"plan_id":"p1","name":"P1"}]}}`)
		case giftClaimPath:
			posts.Add(1)
			fmt.Fprint(w, `{"code":1003,"msg":"target already exists"}`)
		}
	}))
	defer up.Close()

	d := New(config.ZCodeSettings{Enabled: true, BaseURL: up.URL}, nil)
	acc := &model.Account{ID: "zc-2", Channel: model.ChannelZCode, CredsJSON: []byte(`{"jwt":"t"}`)}
	for i := 0; i < 2; i++ {
		res, err := d.ClaimGiftPlans(context.Background(), acc)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if res.Already != 1 || res.Claimed != 0 || res.Failed != 0 {
			t.Fatalf("run %d counts = %+v, want already=1", i, res)
		}
	}
	if posts.Load() != 2 {
		t.Fatalf("claim posts = %d, want one per run", posts.Load())
	}
}

// No ZCode session (a key-imported account) is ineligible, not a crash.
func TestBP231GiftClaimIneligible(t *testing.T) {
	d := New(config.ZCodeSettings{Enabled: true, BaseURL: "http://unused"}, nil)
	acc := &model.Account{ID: "zc-3", Channel: model.ChannelZCode, CredsJSON: []byte(`{"jwt":""}`)}
	_, err := d.ClaimGiftPlans(context.Background(), acc)
	if err == nil {
		t.Fatal("a session-less account must answer an error")
	}
}

// dashedDeviceMid is what giftDeviceMid derives for the test account (the
// JWT sub hashed to a dashed v4-shaped UUID).
var dashedDeviceMid = func() string {
	c := &Creds{JWT: "tok"}
	return GiftDeviceMid(c, "zc-1")
}()
