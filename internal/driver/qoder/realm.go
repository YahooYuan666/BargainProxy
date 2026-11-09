package qoder

import "bargainproxy/internal/uperr"

// Realm config (BP-305): everything that differs between Qoder CN and the
// international site. The COSY envelope (RSA/AES/signature) is ONE scheme on
// both realms (hub qoder_sign.py carries a single SERVER_PUB_PEM), the token
// family is identical (/api/v1/jobToken/{exchange,refresh}, jt-/jrt- prefixes)
// — the split lives in the hosts, the UMID helper, and the campaigns origin.
//
// Reference (MIT, two implementations cross-checked):
//   - shuishuipingan/qoder2api-hub REALM_CONFIGS (qoder_accounts.py): intl
//     openapi=openapi.qoder.sh, inference=api3.qoder.sh primary with
//     api2/api1 as the official fallbacks (client 0.4.8 splits
//     inference/securityInference; api1 is no longer first), legacy
//     daily-check-in endpoints 404 on intl (the daily benefit lives on the
//     campaigns platform there).
//   - EchoPing07/Qoder-2API-Go: intl PAT onboarding, per-account region.
//
// v1 scope: PAT onboarding only (same exchange path both realms); gateway
// fallback hosts are recorded but not wired (api3 is the official primary).

type realmConfig struct {
	// Name is the canonical realm id: "cn" | "intl".
	Name string
	// OpenAPI hosts the token family, quota and the campaigns platform.
	OpenAPI string
	// Gateway hosts the inference calls (chat, model list, status).
	Gateway string
	// UMDir is the official client's config dir under $HOME, where the CLI
	// caches the UMID helper.
	UMDir string
	// UmidEnv is the runtime-info environment argument: 0 = CN, 3 = intl
	// (UMID ONLINE / US_ONLINE).
	UmidEnv string
	// Website is the campaigns Origin/Referer host.
	Website string
}

// NormalizeRealm is the exported form for the panel's account-creation path
// (BP-305): unknown values fall back to "cn".
func NormalizeRealm(realm string) string { return normalizeRealm(realm) }

func normalizeRealm(realm string) string {
	if realm == "intl" {
		return "intl"
	}
	// "" and anything unknown stay CN: the existing pool predates realms and
	// every current account is a CN one.
	return "cn"
}

func realmCfg(realm string) realmConfig {
	if normalizeRealm(realm) == "intl" {
		return realmConfig{
			Name:    "intl",
			OpenAPI: "https://openapi.qoder.sh",
			Gateway: "https://api3.qoder.sh",
			UMDir:   ".qoder",
			UmidEnv: "3",
			Website: "https://qoder.com",
		}
	}
	return realmConfig{
		Name:    "cn",
		OpenAPI: openBase,
		Gateway: authBase,
		UMDir:   ".qoder-cn",
		UmidEnv: "0",
		Website: "https://qoder.com.cn",
	}
}

// guardRealmModel rejects a binding key the account's own LIVE realm listing
// has never served (BP-305): a clean cross-region miss (ClassNotFound
// cascades to the next binding) instead of the upstream's generic
// illegal-model 400. Disabled keys (Off) ride on — the upstream's own 400
// feeds the BP-304 credits recast. The static fallback catalog never gates
// (an intl account falling back to the CN static list would wrongly reject
// intl-exclusive keys).
func guardRealmModel(cat *ModelCatalog, key string, rc realmConfig) error {
	if cat == nil || !cat.Live || key == "" {
		return nil
	}
	if _, served := cat.Caps[key]; served || cat.Off[key] {
		return nil
	}
	return uperr.New(uperr.ClassNotFound, 404,
		"qoder: model %q is not on this %s account's listing (wrong region binding?)",
		key, rc.Name)
}
