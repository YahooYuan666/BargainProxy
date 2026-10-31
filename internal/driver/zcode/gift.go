package zcode

// BP-231: ZCode hands out event gift plans (Trust Build, the weekend
// ZCODE WEEKEND BUILD, …) that only earn tokens once each is CLAIMED —
// claimable from the app, and from here: the app's own preview list plus its
// banner feed, POSTed one claim per plan (magpie-community/plugins #503 +
// 0.1.35 8ebbaa58/2e580cfe, 2026-10-29/30 real captures). The claim verdict
// rides the BODY code, not the HTTP status:
//
//	code 0    → claimed (the next request bills the plan)
//	code 1003 → already claimed ("target already exists", HTTP 200)
//	code 3007 → captcha wall — the app wants its Aliyun captcha; we do not
//	            solve captchas (the BP-65 wall), so the plan is skipped and
//	            noted, never counted as claimed or failed
//	anything else, an HTTP error, or a non-JSON reply → failed (retryable
//	            later the same day; the account's other state is untouched)
//
// BP-231a (field 2026-10-31 05:21, the user's first live runs): the billing
// plane answers 400 {"code":3001,"parameter error"} behind Alibaba's ESA
// edge unless the request matches the app's shape — the plugin source pins
// it exactly: `?app_version=<ver>&platform=<os>-<arch>` QUERY params on
// preview, an X-Device-Mid (dashed UUID per account) on every call, and
// X-ZCode-App-Version / X-Platform headers; touch additionally takes
// ?seq=<n, counting up from 0> and X-Client-Language. The chat family's
// identityHeaders (anthropic-version, UA ZCode/1.0.7) is the WRONG family
// here and was the whole bug.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"bargainproxy/internal/httpx"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

const (
	giftPreviewPath = "/api/v1/zcode-plan/billing/preview"
	giftTouchPath   = "/api/v1/marketing/touch"
	giftClaimPath   = "/api/v1/zcode-plan/billing/claim"

	// giftAppVersion is the ZCode app build the billing plane expects (the
	// plugin pins the same figure; the chat lane's defaultAppVersion 1.0.7
	// predates it and reads as a different client).
	giftAppVersion = "3.16.3"
	giftPlatform   = "win32-x64"
	giftLanguage   = "zh-CN"

	// giftCallTimeout caps one billing-plane call: a stalled dial/proxy must
	// surface as a retryable failure within the task's patience (BP-231a).
	giftCallTimeout = 30 * time.Second
)

// giftTouchSeq counts marketing/touch polls the way the app does: ?seq=
// starting at 0 and counting up.
var giftTouchSeq atomic.Int64

// GiftClaimResult is one account's daily claim outcome, per-plan verdicts
// folded into counts plus a note line each for the task log.
type GiftClaimResult struct {
	Claimed int
	Already int
	Captcha int
	Failed  int
	Notes   []string
}

// giftPlan is one claimable plan id (preview row or banner button), with the
// display name when the source carried one.
type giftPlan struct {
	id   string
	name string
}

// giftDeviceMid is the account's X-Device-Mid in the dashed UUID shape the
// app persists: the JWT sub (else the account id) hashed into a stable
// v4-shaped UUID — the same id on every call, never rotated.
func giftDeviceMid(creds *Creds, accountID string) string {
	bare := deviceIDFor(creds, accountID)
	if len(bare) == 32 {
		return bare[0:8] + "-" + bare[8:12] + "-" + bare[12:16] + "-" + bare[16:20] + "-" + bare[20:32]
	}
	return bare
}

// giftRequest builds one billing-plane request with the app's header family.
// The JWT rides Authorization; Content-Type is set when a body rides along.
func (d *Driver) giftRequest(ctx context.Context, creds *Creds, accountID, method, pathWithQuery string, body []byte, extra map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.base()+pathWithQuery, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+giftAppVersion)
	req.Header.Set("Authorization", "Bearer "+creds.JWT)
	req.Header.Set("X-Device-Mid", giftDeviceMid(creds, accountID))
	req.Header.Set("X-ZCode-App-Version", giftAppVersion)
	req.Header.Set("X-Platform", giftPlatform)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	start := time.Now()
	log.Printf("zcode: gift dispatch %s %s (device=%s)", method, pathWithQuery, req.Header.Get("X-Device-Mid"))
	type giftResult struct {
		r   *http.Response
		err error
	}
	done := make(chan giftResult, 1)
	go func() {
		r, e := d.giftClient.Do(req)
		done <- giftResult{r, e}
	}()
	watch := time.AfterFunc(10*time.Second, func() {
		log.Printf("zcode: gift dispatch STILL in flight after 10s (%s %s) — dial/proxy/upstream stall", method, pathWithQuery)
	})
	gr := <-done
	watch.Stop()
	if gr.err != nil {
		log.Printf("zcode: gift dispatch failed after %s: %v", time.Since(start).Round(time.Millisecond), gr.err)
		return nil, gr.err
	}
	log.Printf("zcode: gift dispatch done in %s", time.Since(start).Round(time.Millisecond))
	return gr.r, nil
}

// ClaimGiftPlans claims every gift plan the account's preview list and
// banner feed offer. Accounts without a ZCode session (imported by GLM
// Coding Plan key) have nothing to claim with and answer ClassAuth.
func (d *Driver) ClaimGiftPlans(ctx context.Context, acc *model.Account) (*GiftClaimResult, error) {
	creds, err := ParseCreds(acc.CredsJSON)
	if err != nil {
		return nil, uperr.New(uperr.ClassAuth, 0, "zcode: %v", err)
	}
	if creds.JWT == "" {
		return nil, uperr.New(uperr.ClassAuth, 401, "zcode: no zcode session on this account (gift plans need one)")
	}

	plans, err := d.collectGiftPlans(ctx, creds, acc.ID)
	if err != nil {
		return nil, err
	}
	res := &GiftClaimResult{}
	if len(plans) == 0 {
		res.Notes = append(res.Notes, "no claimable plans listed")
		return res, nil
	}
	for _, p := range plans {
		label := p.name
		if label == "" {
			label = p.id
		}
		code, msg, err := d.claimGiftPlan(ctx, creds, acc.ID, p.id)
		switch {
		case err != nil:
			res.Failed++
			res.Notes = append(res.Notes, fmt.Sprintf("failed %s: %v", label, err))
		case code == 0:
			res.Claimed++
			res.Notes = append(res.Notes, fmt.Sprintf("claimed %s", label))
		case code == 1003:
			res.Already++
			res.Notes = append(res.Notes, fmt.Sprintf("already claimed %s", label))
		case code == 3007:
			res.Captcha++
			res.Notes = append(res.Notes, fmt.Sprintf("captcha required for %s (claim it in the ZCode app)", label))
		default:
			res.Failed++
			res.Notes = append(res.Notes, fmt.Sprintf("failed %s: code %d %s", label, code, msg))
		}
	}
	log.Printf("zcode: gift plans claimed=%d already=%d captcha=%d failed=%d (account=%s)",
		res.Claimed, res.Already, res.Captcha, res.Failed, acc.ID)
	return res, nil
}

// collectGiftPlans reads both sources the app offers: the billing preview
// list and the marketing banner feed — a weekend banner plan sits in the
// feed ALONE, so skipping it misses gifts.
func (d *Driver) collectGiftPlans(ctx context.Context, creds *Creds, accountID string) ([]giftPlan, error) {
	previewPath := giftPreviewPath + "?app_version=" + giftAppVersion + "&platform=" + giftPlatform
	preview, err := d.giftGet(ctx, creds, accountID, previewPath, nil)
	if err != nil {
		return nil, err
	}
	var pl struct {
		Code int `json:"code"`
		Data struct {
			Plans []struct {
				PlanID string `json:"plan_id"`
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"plans"`
		} `json:"data"`
	}
	if json.Unmarshal(preview, &pl) != nil {
		return nil, uperr.New(uperr.ClassInternal, 0, "zcode: gift preview is not JSON: %.200s", preview)
	}
	seen := map[string]bool{}
	var out []giftPlan
	for _, p := range pl.Data.Plans {
		if p.PlanID == "" || seen[p.PlanID] {
			continue
		}
		seen[p.PlanID] = true
		out = append(out, giftPlan{id: p.PlanID, name: p.Name})
	}

	touchPath := giftTouchPath + "?seq=" + strconv.FormatInt(giftTouchSeq.Add(1)-1, 10)
	touch, err := d.giftGet(ctx, creds, accountID, touchPath, map[string]string{"X-Client-Language": giftLanguage})
	if err != nil {
		// A broken banner feed must not sink the preview plans already
		// collected — log and continue with what we have.
		log.Printf("zcode: marketing/touch read failed (account-side banners skipped): %v", err)
		return out, nil
	}
	var tl struct {
		Code int `json:"code"`
		Data struct {
			Deliveries []struct {
				Banner struct {
					Buttons []struct {
						Action struct {
							Type string `json:"type"`
							Args struct {
								PlanID string `json:"plan_id"`
							} `json:"args"`
						} `json:"action"`
					} `json:"buttons"`
				} `json:"banner"`
			} `json:"deliveries"`
		} `json:"data"`
	}
	if json.Unmarshal(touch, &tl) != nil {
		return out, nil
	}
	for _, del := range tl.Data.Deliveries {
		for _, b := range del.Banner.Buttons {
			if b.Action.Type != "claim_zcode_plan" || b.Action.Args.PlanID == "" || seen[b.Action.Args.PlanID] {
				continue
			}
			seen[b.Action.Args.PlanID] = true
			out = append(out, giftPlan{id: b.Action.Args.PlanID})
		}
	}
	return out, nil
}

// claimGiftPlan posts one claim and returns the body's code (with its msg)
// — the HTTP status alone says nothing (1003 rides a 200).
func (d *Driver) claimGiftPlan(ctx context.Context, creds *Creds, accountID, planID string) (int, string, error) {
	body, _ := json.Marshal(map[string]string{"plan_id": planID})
	resp, err := d.giftRequest(ctx, creds, accountID, http.MethodPost, giftClaimPath, body, nil)
	if err != nil {
		return 0, "", giftTransportErr(ctx, err)
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return 0, "", fmt.Errorf("read body: %w", rerr)
	}
	var doc struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return 0, "", fmt.Errorf("non-JSON reply (status %d): %.120s", resp.StatusCode, raw)
	}
	return doc.Code, doc.Msg, nil
}

// giftGet fetches one billing-plane JSON document. A non-200 carries the
// body in the error — the edge's 3001/429 verdicts are unreadable otherwise.
func (d *Driver) giftGet(ctx context.Context, creds *Creds, accountID, pathWithQuery string, extra map[string]string) ([]byte, error) {
	start := time.Now()
	resp, err := d.giftRequest(ctx, creds, accountID, http.MethodGet, pathWithQuery, nil, extra)
	if err != nil {
		log.Printf("zcode: gift GET %s failed after %s: %v", pathWithQuery, time.Since(start).Round(time.Millisecond), err)
		return nil, giftTransportErr(ctx, err)
	}
	log.Printf("zcode: gift GET %s -> %d in %s", pathWithQuery, resp.StatusCode, time.Since(start).Round(time.Millisecond))
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return nil, fmt.Errorf("read %s: %w", pathWithQuery, rerr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, uperr.New(uperr.ClassTransport, resp.StatusCode, "zcode: gift GET %s: status %d: %.200s", pathWithQuery, resp.StatusCode, raw)
	}
	return raw, nil
}

// giftTransportErr wraps a transport failure with the shared classifier.
func giftTransportErr(ctx context.Context, err error) error {
	return uperr.New(httpx.ClassifyTransportErr(ctx, err), 0, "zcode gift call: %v", err)
}

// AttachIdentity exposes the zcode request header family for diagnostic
// probes (scripts/zgift): identical to the driver's own outbound shape.
func AttachIdentity(req *http.Request, c *Creds, appVersion, sessionID string) {
	identityHeaders(req, c, appVersion, sessionID)
}

// GiftDeviceMid exposes the billing plane's X-Device-Mid derivation for
// diagnostic probes (scripts/zgift).
func GiftDeviceMid(c *Creds, accountID string) string {
	return giftDeviceMid(c, accountID)
}
