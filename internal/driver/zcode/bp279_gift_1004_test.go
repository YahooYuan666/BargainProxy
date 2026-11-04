package zcode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

// BP-279 (plugins zcode 0.1.50, #729): claim body code 1004 (HTTP 400
// "your account is not eligible for this plan yet") means ZCode is not
// offering the account that plan AT THAT MOMENT — a plan can be dropped
// into the feed later in the day (11-04: a GLOBAL BUILD banner showed 28
// minutes after sign-in). It is an ineligible, not a failure: the day's
// later task runs retry it naturally, and reporting it as failed turned a
// "come back later" into a red task row.
func TestBP279GiftClaim1004IsIneligible(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case giftPreviewPath:
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"plans":[{"plan_id":"zcode-v3-start-plan-trust-1006","name":"ZCode Trust Build","status":"active"}]}}`)
		case giftTouchPath:
			fmt.Fprint(w, `{"code":0,"msg":"","data":{"deliveries":[]}}`)
		case giftClaimPath:
			var body struct {
				PlanID string `json:"plan_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fmt.Fprint(w, `{"code":1004,"msg":"your account is not eligible for this plan yet"}`)
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
	if res.Ineligible != 1 || res.Failed != 0 {
		t.Fatalf("counts = %+v, want ineligible=1 failed=0 (1004 is a come-back-later, not a failure)", res)
	}
}

// The version constant must track the build the billing plane currently
// expects: upstream measured SIX accounts answering 1004 for everything
// under 3.16.3's header and four of them claiming with code 0 the same
// night under 3.17.2's (plugins 0.1.50 README, 11-04).
func TestBP279GiftAppVersionCurrent(t *testing.T) {
	if giftAppVersion != "3.17.2" {
		t.Fatalf("giftAppVersion = %q, want 3.17.2 (six accounts all answered 1004 under 3.16.3; four claimed under 3.17.2)", giftAppVersion)
	}
}
