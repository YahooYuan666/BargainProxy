// Qoder quota detail for the panel: the raw OpenAPI document carries more
// than the single remain/total pair used by the account card — plan usage,
// add-on and org packages, the exceeded flag and the account tier. The daily
// check-in status (+100 credits) is fetched from the same openapi host.
//
// Field mapping mirrors agentpoints2 internal/qoder (quotaView):
// userQuota.remaining/total/used, addOnQuota.remaining,
// orgResourcePackage.available/remaining, isQuotaExceeded, userType.
package qoder

import (
	"context"
	"net/http"
	"time"

	"bargainproxy/internal/model"
)

// QuotaDetail is the panel payload for one Qoder account.
type QuotaDetail struct {
	Account         string        `json:"account,omitempty"`
	UserType        string        `json:"user_type,omitempty"`
	Unit            string        `json:"unit,omitempty"`
	PlanRemain      int64         `json:"plan_remaining"`
	PlanTotal       int64         `json:"plan_total"`
	PlanUsed        int64         `json:"plan_used"`
	AddonRemain     int64         `json:"addon_remaining"`
	OrgRemain       int64         `json:"org_remaining"`
	DedicatedRemain int64         `json:"dedicated_remaining,omitempty"` // 专属资源包合计 (BP-98 Qoder-7b)
	Exceeded        bool          `json:"exceeded"`
	ExpiresAtMs     int64         `json:"expires_at,omitempty"`
	Checkin         *CheckinState `json:"checkin,omitempty"`
	CheckinError    string        `json:"checkin_error,omitempty"`
}

// CheckinState is the daily check-in status without claiming.
type CheckinState struct {
	Status     string `json:"status"`
	StreakDays int64  `json:"streak_days"`
	Claimable  bool   `json:"claimable"`
	Claimed    bool   `json:"claimed"`
	Disabled   bool   `json:"disabled"`
}

// QuotaDetail fetches the quota document and the check-in status.
func (d *Driver) QuotaDetail(ctx context.Context, acc *model.Account) (*QuotaDetail, error) {
	doc, err := d.QuotaRaw(ctx, acc)
	if err != nil {
		return nil, err
	}
	out := parseQuotaDetail(doc)
	out.Account = acc.ID
	if st, serr := d.CheckinStatus(ctx, acc); serr != nil {
		out.CheckinError = serr.Error()
	} else {
		out.Checkin = st
	}
	return out, nil
}

// parseQuotaDetail maps the OpenAPI document (live nested shape, legacy flat
// shape) to the panel payload.
func parseQuotaDetail(doc map[string]any) *QuotaDetail {
	out := &QuotaDetail{}
	if uq, ok := doc["userQuota"].(map[string]any); ok {
		out.PlanRemain = toInt64(uq["remaining"])
		out.PlanTotal = toInt64(uq["total"])
		out.PlanUsed = toInt64(uq["used"])
		out.Unit = toString(uq["unit"])
	}
	if aq, ok := doc["addOnQuota"].(map[string]any); ok {
		out.AddonRemain = toInt64(aq["remaining"])
	}
	if oq, ok := doc["orgResourcePackage"].(map[string]any); ok {
		if avail, _ := oq["available"].(bool); avail {
			out.OrgRemain = toInt64(oq["remaining"])
		}
	}
	// Dedicated packages (BP-98 Qoder-7b): every package's remaining credits,
	// summed — the array used to be dropped entirely (magpie 0.2.30
	// parseUsage "Dedicated credits").
	if pkgs, ok := doc["dedicatedResourcePackages"].([]any); ok {
		for _, it := range pkgs {
			if pkg, ok := it.(map[string]any); ok {
				out.DedicatedRemain += toInt64(pkg["remaining"])
			}
		}
	}
	out.Exceeded, _ = doc["isQuotaExceeded"].(bool)
	out.UserType = toString(doc["userType"])
	out.ExpiresAtMs = toInt64(doc["expiresAt"])
	if out.PlanTotal == 0 { // legacy flat document
		out.PlanRemain = toInt64(doc["plan_remaining"])
		out.PlanTotal = toInt64(doc["plan_total"])
		out.PlanUsed = toInt64(doc["plan_used"])
		if out.AddonRemain == 0 {
			out.AddonRemain = toInt64(doc["addon_remaining"])
		}
		if out.OrgRemain == 0 {
			out.OrgRemain = toInt64(doc["org_remaining"])
		}
	}
	return out
}

// CheckinStatus reads the daily check-in status without claiming it.
//
// The legacy endpoint reports DISABLED for every account since Qoder CN moved
// the daily 100 credits into the desktop benefit centre (BP-120). Reporting
// that verbatim made the panel say the account has no check-in programme while
// an active CLAIM_BENEFIT campaign was sitting on the campaigns page. When the
// legacy status is DISABLED (or unrecognised), the campaigns page is the
// authority: it knows whether today's claim is still available.
func (d *Driver) CheckinStatus(ctx context.Context, acc *model.Account) (*CheckinState, error) {
	rt, err := d.runtimeFor(acc)
	if err != nil {
		return nil, err
	}
	bearer, err := d.openBearer(ctx, rt)
	if err != nil {
		return nil, qoderError(err)
	}
	rt.mu.Lock()
	uid := rt.uid
	rt.mu.Unlock()
	// BP-237: the device-identity helper keys on the uid; a runtime that has
	// not bootstrapped leaves it empty, and acc.UID is the authoritative value.
	if uid == "" {
		uid = acc.UID
	}
	out, err := d.checkinJSON(ctx, http.MethodGet, d.openAPIBase()+pathCheckStatus, nil, uid, bearer)
	if err != nil {
		return nil, qoderError(err)
	}
	st := &CheckinState{
		Status:     toString(out["status"]),
		StreakDays: toInt64(out["currentStreakDays"]),
	}
	switch st.Status {
	case "CLAIMABLE":
		st.Claimable = true
		return st, nil
	case "CLAIMED":
		st.Claimed = true
		return st, nil
	}
	// DISABLED / anything else: ask the campaigns page instead of repeating a
	// status that no longer describes the programme.
	if cs, cerr := d.campaignCheckinState(ctx, uid, bearer); cerr == nil && cs != nil {
		cs.StreakDays = st.StreakDays
		return cs, nil
	}
	st.Disabled = true
	return st, nil
}

// campaignCheckinState derives the check-in status from the campaigns page:
// the active CLAIM_BENEFIT campaign is the daily 100-credit benefit.
// A nil result means "no active daily campaign at all".
func (d *Driver) campaignCheckinState(ctx context.Context, uid, bearer string) (*CheckinState, error) {
	list, err := d.checkinJSON(ctx, http.MethodGet, d.openAPIBase()+campaignPath, nil, uid, bearer)
	if err != nil {
		return nil, qoderError(err)
	}
	rawList, _ := list["campaigns"].([]any)
	now := time.Now().Unix()
	for _, it := range rawList {
		c, ok := it.(map[string]any)
		if !ok || toString(c["actionType"]) != "CLAIM_BENEFIT" {
			continue
		}
		start, end := toInt64(c["startAt"]), toInt64(c["endAt"])
		if !((start == 0 || start <= now) && (end == 0 || end > now)) {
			continue
		}
		st := &CheckinState{Status: toString(c["claimStatus"])}
		switch st.Status {
		case "CLAIMABLE":
			st.Claimable = true
		case "CLAIMED":
			st.Claimed = true
		default:
			st.Disabled = true
		}
		return st, nil
	}
	return nil, nil
}
