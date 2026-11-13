// Package media is the single source of truth for the gateway's media
// generation surface: which canonical models exist, which channel serves each,
// what parameters each accepts, and how caller-facing requests map onto the
// per-channel wire shapes. Parameter facts come from the vendors' own machine
// readable specs (lobster /api/media/*/models parameterSpec) and the WorkBuddy
// desktop client's request builder — see
// research/2026-11-10-媒体参数规格-唯一事实源.md.
package media

import "strings"

// Param is one caller-facing parameter of a media model. Supported=false marks
// a capability the model does NOT have: the caller must not send it, and the
// gateway answers capability_not_supported with models_that_can instead of
// silently dropping it (BP-306 parameter discipline).
type Param struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"` // integer | string | enum | boolean | array
	Min       *int     `json:"min,omitempty"`
	Max       *int     `json:"max,omitempty"`
	Values    []string `json:"values,omitempty"`
	Default   any      `json:"default,omitempty"`
	Supported bool     `json:"supported"`
	Note      string   `json:"note,omitempty"`
	// Pattern marks a free-form string param the gateway validates itself:
	// the regex must match (with two digit groups = W and H), Min/Max bound
	// total pixels, RatioMin/RatioMax bound W/H. Vendor specs list UI enums
	// but accept custom W*H inside these bounds (qwen 1:8~8:1, minimax 1:3~3:1).
	Pattern  string   `json:"pattern,omitempty"`
	RatioMin *float64 `json:"ratio_min,omitempty"`
	RatioMax *float64 `json:"ratio_max,omitempty"`
}

func fptr(v float64) *float64 { return &v }

// Model is one canonical media model. Upstream maps channel → native model id;
// a model reachable from several channels lists them all and the gateway picks
// by availability (BP-306: never drop a channel from the catalog).
type Model struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"` // image | video
	DisplayName string            `json:"display_name"`
	Upstream    map[string]string `json:"upstream"` // channel -> upstream model
	Realm       map[string]string `json:"realm,omitempty"`
	Operations  []string          `json:"operations"`
	Params      []Param           `json:"params"`
	Elo         int               `json:"elo,omitempty"`
	EloRank     int               `json:"elo_rank,omitempty"`
	EloSource   string            `json:"elo_source,omitempty"`
	Pricing     string            `json:"pricing"`
	Browser     bool              `json:"requires_browser,omitempty"`
	Note        string            `json:"note,omitempty"`
}

func iptr(v int) *int { return &v }

// watermarkParam declares the vendor-side watermark every workbuddy/lobster
// image model stamps on ("AI生成" corner mark). The hidden upstream param
// defaults to true and false is unverified (a probe costs credits), so it is
// declared unsupported — callers must know the mark is coming (sess_4041).
var watermarkParam = Param{Name: "watermark", Type: "boolean", Supported: false,
	Note: "上游强制叠加「AI生成」角标，不可关闭（2026-11-13 实测 watermark:false 被忽略仍带角标）"}

// imageParams assembles the common image parameter set with per-model tweaks.
func imageParams(nMin, nMax int, size *Param, ref *Param, extra ...Param) []Param {
	ps := []Param{
		{Name: "prompt", Type: "string", Supported: true},
		{Name: "n", Type: "integer", Min: iptr(nMin), Max: iptr(nMax), Default: 1, Supported: true},
	}
	if size != nil {
		ps = append(ps, *size)
	} else {
		ps = append(ps, Param{Name: "size", Type: "string", Supported: false,
			Note: "模型不支持指定尺寸；比例/尺寸写进提示词"})
	}
	ps = append(ps, Param{Name: "aspect_ratio", Type: "enum", Supported: false,
		Note: "模型不支持指定宽高比；写进提示词"})
	if ref != nil {
		ps = append(ps, *ref)
	} else {
		ps = append(ps, Param{Name: "reference_images", Type: "array", Supported: false,
			Note: "模型不支持参考图"})
	}
	for _, e := range extra {
		// An extra entry REPLACES the base entry of the same name instead of
		// coexisting: seedream-5-pro's aspect_ratio(true) must override the
		// default unsupported one, or get() and CapabilityModels disagree and
		// the preflight rejects requests the vendor spec allows (sess_4041).
		replaced := false
		for i := range ps {
			if ps[i].Name == e.Name {
				ps[i] = e
				replaced = true
				break
			}
		}
		if !replaced {
			ps = append(ps, e)
		}
	}
	return ps
}

func videoBase(res []string, resDefault string, ar []string, arDefault string, dMin, dMax, dDefault int, audio string) []Param {
	return []Param{
		{Name: "prompt", Type: "string", Supported: true},
		{Name: "resolution", Type: "enum", Values: res, Default: resDefault, Supported: true},
		{Name: "aspect_ratio", Type: "enum", Values: ar, Default: arDefault, Supported: true},
		{Name: "duration", Type: "integer", Min: iptr(dMin), Max: iptr(dMax), Default: dDefault, Supported: true},
		{Name: "enable_audio", Type: "boolean", Supported: audio == "switchable",
			Note: map[string]string{"always-on": "该模型强制生成音频，不可关闭", "switchable": ""}[audio]},
		{Name: "first_frame", Type: "string", Supported: true, Note: "首帧图片 URL 或 data:URI"},
		{Name: "last_frame", Type: "string", Supported: true, Note: "尾帧；必须与首帧同传"},
	}
}

// Catalog is the full caller-visible model list (BP-306 P1).
func Catalog() []Model {
	aa := "artificialanalysis.ai AA-Image-T2I v2.1 / AA-Video-T2V v2.1 (2026-11-10)"
	return []Model{
		{
			ID: "nano-banana-2", Kind: "image", DisplayName: "Nano Banana 2 (Gemini 3.1 Flash Image)",
			Upstream:   map[string]string{"antigravity": "gemini-3.1-flash-image", "gemini-web": "gemini-3.1-flash-image"},
			Operations: []string{"text_to_image"},
			Params:     imageParams(1, 4, nil, nil),
			Elo:        1142, EloRank: 9, EloSource: aa,
			Pricing: "免费额度（antigravity 池）",
			Browser: false,
			Note:    "antigravity 通道强制 4K JPEG（实测 5632×3072，40-50s）；gemini-web 通道需浏览器、暂未接线",
		},
		{
			ID: "hy-image-3.5", Kind: "image", DisplayName: "HY-Image-3.5（腾讯混元，Global）",
			Upstream: map[string]string{"workbuddy": "hy-image-3.5"}, Realm: map[string]string{"workbuddy": "global"},
			Operations: []string{"text_to_image", "image_to_image"},
			Params:     imageParams(1, 4, nil, &Param{Name: "reference_images", Type: "array", Supported: true, Note: "图生图输入图"}, watermarkParam),
			Elo:        973, EloRank: 67, EloSource: aa,
			Pricing: "2.12 credit/张（实测）",
			Note:    "上游零参数校验：size/n 等全部静默忽略，网关按白名单钳制",
		},
		{
			ID: "hunyuan-image-alpha", Kind: "image", DisplayName: "Hunyuan Image Alpha（腾讯混元，CN）",
			Upstream: map[string]string{"workbuddy": "hunyuan-image-alpha"}, Realm: map[string]string{"workbuddy": "cn"},
			Operations: []string{"text_to_image"},
			Params:     imageParams(1, 4, nil, nil, watermarkParam),
			Pricing:    "约 5.7 credit/张",
			Note:       "上游零参数校验，网关按白名单钳制；Elo 榜无此模型条目",
		},
		{
			ID: "hunyuan-image-alpha-edit", Kind: "image", DisplayName: "Hunyuan Image Alpha Edit（腾讯混元，CN）",
			Upstream: map[string]string{"workbuddy": "hunyuan-image-alpha-edit"}, Realm: map[string]string{"workbuddy": "cn"},
			Operations: []string{"image_to_image"},
			Params:     imageParams(1, 4, nil, &Param{Name: "reference_images", Type: "array", Supported: true, Max: iptr(1), Note: "待编辑原图"}, watermarkParam),
			Pricing:    "约 5.7 credit/张",
		},
		{
			ID: "qwen-image-3", Kind: "image", DisplayName: "Qwen-Image-3.0",
			Upstream:   map[string]string{"lobster": "qwen-image-3.0"},
			Operations: []string{"text_to_image", "image_to_image"},
			Params: imageParams(1, 6,
				&Param{Name: "size", Type: "string", Pattern: `^\d{3,4}\*\d{3,4}$`, Default: "2048*2048", Supported: true,
					Min: iptr(262144), Max: iptr(4194304), RatioMin: fptr(0.125), RatioMax: fptr(8),
					Note: "自定义 W*H（星号分隔）：总像素 512²~2048²、比例 1:8~8:1；推荐档 2048*2048 / 2688*1536(7:4) / 2368*1728(11:8) / 1024*1024；不支持 1K/2K/auto"},
				&Param{Name: "reference_images", Type: "array", Max: iptr(3), Supported: true, Note: "单张 ≤10MB；每张输入图单独计费"}, watermarkParam),
			Elo: 1073, EloRank: 20, EloSource: aa,
			Pricing: "18 积分/张",
		},
		{
			ID: "qwen-image-3-pro", Kind: "image", DisplayName: "Qwen-Image-3.0-Pro",
			Upstream:   map[string]string{"lobster": "qwen-image-3.0-pro"},
			Operations: []string{"text_to_image", "image_to_image"},
			Params: imageParams(1, 6, &Param{Name: "size", Type: "string", Pattern: `^\d{3,4}\*\d{3,4}$`, Default: "2048*2048", Supported: true,
				Min: iptr(262144), Max: iptr(4194304), RatioMin: fptr(0.125), RatioMax: fptr(8),
				Note: "自定义 W*H（星号分隔）：总像素 512²~2048²、比例 1:8~8:1；推荐档 2048*2048 / 2688*1536(7:4) / 2368*1728(11:8) / 1024*1024"}, &Param{Name: "reference_images", Type: "array", Max: iptr(3), Supported: true, Note: "单张 ≤10MB"}, watermarkParam),
			Elo: 1096, EloRank: 19, EloSource: aa,
			Pricing: "25 积分/张（≤225 万像素）；50（2K）",
		},
		{
			ID: "seedream-5", Kind: "image", DisplayName: "Doubao-Seedream-5.0-lite",
			Upstream:   map[string]string{"lobster": "doubao-seedream-5-0-260128"},
			Operations: []string{"text_to_image", "image_to_image"},
			Params: imageParams(1, 1,
				&Param{Name: "size", Type: "enum", Values: []string{"2K", "3K", "4K"}, Default: "2K", Supported: true, Note: "比例写进提示词"},
				&Param{Name: "reference_images", Type: "array", Max: iptr(14), Supported: true}, watermarkParam),
			Elo: 1019, EloRank: 39, EloSource: aa,
			Pricing: "22 积分/张",
		},
		{
			ID: "seedream-5-pro", Kind: "image", DisplayName: "Doubao-Seedream-5.0-pro",
			Upstream:   map[string]string{"lobster": "doubao-seedream-5-0-pro-260628"},
			Operations: []string{"text_to_image", "image_to_image"},
			Params: imageParams(1, 15,
				&Param{Name: "size", Type: "enum", Values: []string{"2K", "2048x2048", "2304x1728", "1728x2304", "2848x1600", "1600x2848"}, Default: "2K", Supported: true, Note: "档位比例（精确）：2048x2048=1:1、2304x1728=4:3、1728x2304=3:4(竖)、2848x1600=16:9、1600x2848=9:16(竖)；≤236 万像素 30 积分、其余 60"},
				&Param{Name: "reference_images", Type: "array", Max: iptr(10), Supported: true},
				Param{Name: "aspect_ratio", Type: "enum", Supported: false,
					Note: "上游参数规格声明但实测忽略（2026-11-13 探针：传 3:2 出 2048×2048）；比例写进提示词"}, watermarkParam),
			Elo: 1101, EloRank: 18, EloSource: aa,
			Pricing: "30 积分/张（≤236 万像素）；60（更大）",
		},
		{
			ID: "minimax-h3-image", Kind: "image", DisplayName: "MiniMax-H3-Image",
			Upstream:   map[string]string{"lobster": "MiniMax-H3-Image"},
			Operations: []string{"text_to_image", "image_to_image"},
			Params: imageParams(1, 10,
				&Param{Name: "size", Type: "string", Pattern: `^\d{2,5}[xX]\d{2,5}$`, Supported: true,
					Min: iptr(655360), Max: iptr(20000000), RatioMin: fptr(0.3333), RatioMax: fptr(3),
					Note: "自定义 WxH（x 分隔）：面积 655360~20000000 像素、比例 1:3~3:1；传入后忽略 area/aspect_ratio"},
				&Param{Name: "reference_images", Type: "array", Max: iptr(16), Supported: true, Note: "JPG/PNG/WEBP，128-8192px"},
				Param{Name: "area", Type: "enum", Values: []string{"1K", "2K", "4K"}, Default: "2K", Supported: true},
				Param{Name: "aspect_ratio", Type: "enum", Values: []string{"auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "21:9"}, Default: "auto", Supported: true,
					Note: "也可传 1:3~3:1 的任意 X:Y"}, watermarkParam),
			Pricing: "13 积分/张",
			Note:    "未进 AA T2I 榜（无 Elo 数据）",
		},
		{
			ID: "minimax-video-h3", Kind: "video", DisplayName: "MiniMax H3 视频（768p）",
			Upstream:   map[string]string{"workbuddy": "minimax-video-h3", "lobster": "MiniMax-H3"},
			Operations: []string{"text_to_video", "image_to_video"},
			Params:     videoBase([]string{"768P", "2K"}, "768P", []string{"adaptive", "21:9", "16:9", "4:3", "1:1", "3:4", "9:16"}, "adaptive", 4, 15, 5, "always-on"),
			Elo:        1161, EloRank: 3, EloSource: "AA-Video-T2V v2.1 " + aa,
			Pricing: "workbuddy 实测 4s/768P≈57 credit；lobster 50 积分/秒",
		},
		{
			ID: "kling-video-v3", Kind: "video", DisplayName: "Kling Video V3",
			Upstream:   map[string]string{"workbuddy": "kling-video-v3"},
			Operations: []string{"text_to_video", "image_to_video"},
			Params:     videoBase([]string{"720P", "1080P", "4K"}, "720P", []string{"16:9", "9:16", "1:1"}, "16:9", 3, 15, 5, "switchable"),
			Elo:        1000, EloRank: 21, EloSource: "AA-Video-T2V v2.1 " + aa,
			Pricing: "未实测；官方档 720P/1080P/4K",
			Note:    "唯一支持 4K 与 3 秒片段；不支持任何参考素材（图/视频/音频上限全 0）",
		},
		{
			ID: "seedance-2-5", Kind: "video", DisplayName: "Doubao-Seedance-2.5",
			Upstream:   map[string]string{"lobster": "doubao-seedance-2-5-260628"},
			Operations: []string{"text_to_video", "image_to_video"},
			Params:     videoBase([]string{"480p", "720p", "1080p"}, "720p", []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"}, "adaptive", 4, 30, 5, "switchable"),
			Elo:        1155, EloRank: 4, EloSource: "AA-Video-T2V v2.1 " + aa,
			Pricing: "7000 积分/百万 token（480p/720p）",
			Note:    "图生视频榜第 1（1161）；唯一支持 4-30s 与 30 张参考图；比例字段上游规格名 ratio（网关双发，未实弹）",
		},
		{
			ID: "seedance-2", Kind: "video", DisplayName: "Doubao-Seedance-2.0",
			Upstream:   map[string]string{"lobster": "doubao-seedance-2-0-260128"},
			Operations: []string{"text_to_video", "image_to_video"},
			Params:     videoBase([]string{"480p", "720p", "1080p"}, "720p", []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"}, "adaptive", 4, 15, 5, "switchable"),
			Elo:        1137, EloRank: 10, EloSource: "AA-Video-T2V v2.1 " + aa,
			Pricing: "4600 积分/百万 token",
			Note:    "比例字段上游规格名 ratio（网关双发，未实弹）",
		},
		{
			ID: "seedance-2-fast", Kind: "video", DisplayName: "Doubao-Seedance-2.0-fast",
			Upstream:   map[string]string{"lobster": "doubao-seedance-2-0-fast-260128"},
			Operations: []string{"text_to_video", "image_to_video"},
			Params:     videoBase([]string{"480p", "720p"}, "720p", []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"}, "adaptive", 4, 15, 5, "switchable"),
			Pricing:    "3700 积分/百万 token",
			Note:       "比例字段上游规格名 ratio（网关双发，未实弹）",
		},
		{
			ID: "veo-3-1", Kind: "video", DisplayName: "Veo 3.1（gemini-web 网页端）",
			Upstream:   map[string]string{"gemini-web": "veo"},
			Operations: []string{"text_to_video"},
			Params:     videoBase([]string{"720p"}, "720p", []string{"16:9", "9:16"}, "16:9", 8, 9, 8, "switchable"),
			Elo:        965, EloRank: 24, EloSource: "AA-Video-T2V v2.1 " + aa,
			Pricing: "Pro 订阅内；每账号每日条数上限",
			Browser: true,
			Note:    "浏览器通道未接线：available=false；且 9:16 实测仍回 1280×720",
		},
	}
}

// ByID returns the catalog entry for a canonical model id (case-insensitive).
func ByID(id string) (*Model, bool) {
	want := strings.ToLower(strings.TrimSpace(id))
	for i := range Catalog() {
		if strings.ToLower(Catalog()[i].ID) == want {
			return &Catalog()[i], true
		}
	}
	return nil, false
}

// Route resolves a canonical model to one channel. preferred (may be empty)
// names a channel the caller wants; when the model is not served there the
// first listed channel wins. gemini-web always loses to a non-browser channel
// while it is unwired.
func Route(m *Model, preferred string) (channel, upstream string, ok bool) {
	if m == nil || len(m.Upstream) == 0 {
		return "", "", false
	}
	if preferred != "" {
		if up, hit := m.Upstream[preferred]; hit && preferred != "gemini-web" {
			return preferred, up, true
		}
	}
	for _, ch := range []string{"antigravity", "workbuddy", "lobster"} {
		if up, hit := m.Upstream[ch]; hit {
			return ch, up, true
		}
	}
	return "", "", false
}

// CapabilityModels lists canonical models that support a caller-facing
// parameter (capability_not_supported's models_that_can, BP-306 §4.1).
func CapabilityModels(kind, param string) []string {
	var out []string
	for _, m := range Catalog() {
		if m.Kind != kind {
			continue
		}
		for _, p := range m.Params {
			if p.Name == param && p.Supported {
				out = append(out, m.ID)
				break
			}
		}
	}
	return out
}
