package gateway

// BP-313 — fit an overweight request's images. A history full of screenshots
// rides the wire as megabytes of base64 and dies on every channel's size
// ceiling (2026-11-11 22:56: 51 screenshots, 8MB, all four channels dead).
// Fitting them to what the vendors' own clients send is the fix that keeps
// every image visible (research/2026-11-12-上游图片压缩调研.md):
//
//   - openai/codex resizes BEFORE sending (codex-rs/utils/image): long edge
//     2048 for the default detail tiers, triangle filter, JPEG stays JPEG —
//     a screenshot past that size is not what the model would see from the
//     official client either;
//   - Anthropic's docs: "Images larger than either limit are downscaled
//     before processing" (1568px standard tier, 2576px high-res tier), so a
//     2048px cap sits at or above what Claude's own pipeline produces;
//   - magpie runs exactly this per Codex account (internal/gateway/
//     codex_images.go) behind a bytes-bounded LRU cache, because the client
//     replays the whole history on every turn;
//   - qoder2api-hub (qoder_images.py) adds the second lever for PNG
//     screenshots that already fit the cap: opaque PNG → JPEG at high
//     quality, only when it saves at least 20%.
//
// Guard rails (qoder_images.py's list, in spirit): an image that cannot be
// processed faithfully — EXIF orientation other than 1, an ICC profile,
// animation, a decode failure, an absurd declared size — goes as it came.
// Nothing is ever dropped or replaced with text; that is magpie's separate
// reactive layer (body_cap.go), deliberately not adopted here — BP-312's
// honest byte gate already covers the leftover case.
//
// The fit runs once per request, before the byte gate, oldest images first,
// and stops as soon as the estimate drops to three quarters of the budget:
// the headroom keeps a growing conversation from crossing the line every
// turn and breaking the upstream prefix cache each time. An image that only
// needs its resolution lowered (>2048px long edge) is fitted wherever it
// sits — including the latest user message, because that is exactly what
// the official clients do to every image they send, and the incident this
// ticket answers began with images pasted in the current turn. A PAST turn's
// image that already fits the cap may still be format-converted (opaque PNG
// → JPEG, ≥20% savings); the CURRENT turn's is never — small images stay
// byte-for-byte what the client sent. Pinned requests are never touched
// (the caller asked for the honest answer, same principle as the byte
// gate's pinned exemption).

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"bargainproxy/internal/ir"
)

const (
	// imageFitLongEdge is the resolution cap: what codex's client sends for
	// the default detail tiers, above both of Anthropic's processing caps.
	imageFitLongEdge = 2048
	// imageFitMinB64 is the smallest base64 worth touching: below it the fit
	// saves little and the image stays byte-for-byte what the client sent.
	imageFitMinB64 = 256 << 10
	// imageFitJPEGQuality sits between the official client's 85 and
	// qoder2api-hub's 95 (Go's encoder is fixed 4:2:0; screenshots keep
	// their luma — where UI text lives — at full resolution).
	imageFitJPEGQuality = 90
	// imageFitConvertFloor: a format conversion must save at least this much
	// to be applied (qoder2api-hub: "only when the result saves at least
	// 20%"). A pure re-encode that saves less is distortion for nothing.
	imageFitConvertFloor = 0.8
	// imageFitMaxPixels is the decode bomb guard (qoder_images.py: 32M
	// pixels); imageFitMaxB64 the largest base64 this will decode at all.
	imageFitMaxPixels = 32 << 20
	imageFitMaxB64    = 24 << 20

	imageFitCacheBytes = 256 << 20
	imageFitCacheItems = 4096
	imageFitCacheIdle  = time.Hour
)

// imageFitRefits counts images actually decoded and re-encoded (test hook:
// a cache hit must not move it).
var imageFitRefits atomic.Int64

// fitOverweightImages fits req's images when its estimated wire size crosses
// the budget. Called once per Execute, before the byte gate computes
// requestBytes, so every binding sees the fitted body.
func (g *Gateway) fitOverweightImages(req *ir.ChatRequest, pinned bool) {
	if req == nil || pinned {
		return
	}
	budget := int(g.cfg.ImageBudgetBytes)
	if budget <= 0 {
		return
	}
	total := ir.EstimateRequestBytes(req)
	if total <= budget {
		return
	}
	target := budget / 4 * 3
	keep := lastUserMessage(req)
	imageFitted.letGo()
	fitted, before := 0, total
	for mi := range req.Messages {
		if total <= target {
			break
		}
		msg := &req.Messages[mi]
		for pi := range msg.Content {
			if total <= target {
				break
			}
			part := &msg.Content[pi]
			if part.Kind != "image" || len(part.ImageB64) < imageFitMinB64 {
				continue
			}
			// The current turn's images keep their bytes when no resolution
			// change is needed; everything else may also be format-converted.
			allowConvert := mi != keep
			key := imageFitKey(part.ImageMIME, part.ImageB64, allowConvert)
			old := len(part.ImageB64)
			var nm, nb string
			if e, ok := imageFitted.get(key); ok {
				nm, nb = e.mime, e.b64
			} else {
				nm, nb, _ = fitImage(part.ImageMIME, part.ImageB64, allowConvert)
				imageFitted.put(key, nm, nb)
			}
			if nb == "" || nb == part.ImageB64 {
				continue // examined before and left as it came, or no gain now
			}
			log.Printf("gateway: bp313 image fit: msg %d part %d %d→%d bytes %s→%s", mi, pi, old, len(nb), part.ImageMIME, nm)
			part.ImageMIME, part.ImageB64 = nm, nb
			total += len(nb) - old
			fitted++
		}
	}
	if fitted > 0 {
		log.Printf("gateway: bp313 fitted %d image(s), request estimate %d→%d bytes (budget %d)", fitted, before, total, budget)
	}
}

// lastUserMessage is the index of the newest plain user message — the turn
// the user is looking at right now. Its images may be RESIZED (what the
// official clients do to every image) but never format-converted: without a
// resolution change there is no fidelity argument for touching them.
func lastUserMessage(req *ir.ChatRequest) int {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == ir.RoleUser {
			return i
		}
	}
	return -1
}

// fitImage is one image's base64 refitted, or the original back (changed
// false) when nothing faithful could improve it. allowConvert gates the
// convert-only lever (PNG at or under the cap → JPEG). Pure function of its
// input, so the cache below makes every turn after the first a lookup.
func fitImage(mime, b64 string, allowConvert bool) (string, string, bool) {
	if len(b64) > imageFitMaxB64 {
		return mime, b64, false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return mime, b64, false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return mime, b64, false // unknown format (webp, gif, heic…) goes as it came
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > imageFitMaxPixels {
		return mime, b64, false
	}
	switch format {
	case "png":
		if pngFidelityRails(raw) {
			return mime, b64, false
		}
	case "jpeg":
		if jpegFidelityRails(raw) {
			return mime, b64, false
		}
		if max(cfg.Width, cfg.Height) <= imageFitLongEdge {
			return mime, b64, false // no resize needed; a JPEG re-encode would be pure generation loss
		}
	default:
		return mime, b64, false
	}
	var src image.Image
	switch format {
	case "png":
		src, err = png.Decode(bytes.NewReader(raw))
	case "jpeg":
		src, err = jpeg.Decode(bytes.NewReader(raw))
	}
	if err != nil {
		return mime, b64, false
	}
	imageFitRefits.Add(1)
	opaque := imageOpaque(src)
	w, h, resized := fitDims(cfg.Width, cfg.Height)
	type enc struct {
		data []byte
		mime string
	}
	var cands []enc
	if resized {
		dst := triangleResize(src, w, h)
		if p, err := pngEncode(dst); err == nil {
			cands = append(cands, enc{p, "image/png"})
		}
		if opaque {
			if j, err := jpegEncode(dst); err == nil {
				cands = append(cands, enc{j, "image/jpeg"})
			}
		}
	} else if format == "png" && opaque && allowConvert {
		// convert-only (qoder2api-hub's lever): an opaque PNG at or under the
		// cap may become JPEG when it saves at least a fifth of itself
		if j, err := jpegEncode(src); err == nil && float64(len(j)) <= float64(len(raw))*imageFitConvertFloor {
			cands = append(cands, enc{j, "image/jpeg"})
		}
	}
	best := enc{}
	for _, c := range cands {
		if len(c.data) < len(raw) && (best.data == nil || len(c.data) < len(best.data)) {
			best = c
		}
	}
	if best.data == nil {
		return mime, b64, false
	}
	return best.mime, base64.StdEncoding.EncodeToString(best.data), true
}

// fitDims scales w×h down to the long-edge cap, proportionally, never up.
func fitDims(w, h int) (int, int, bool) {
	long := max(w, h)
	if long <= imageFitLongEdge {
		return w, h, false
	}
	scale := float64(imageFitLongEdge) / float64(long)
	nw := max(int(math.Round(float64(w)*scale)), 1)
	nh := max(int(math.Round(float64(h)*scale)), 1)
	return nw, nh, true
}

func pngEncode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func jpegEncode(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: imageFitJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// imageOpaque reports whether every pixel is fully opaque. Models that
// cannot carry alpha skip the scan; the rest are checked channel by channel
// (at most once per image — the cache makes it once ever).
func imageOpaque(src image.Image) bool {
	switch src.ColorModel() {
	case color.GrayModel, color.Gray16Model, color.YCbCrModel, color.CMYKModel:
		return true
	}
	if img, ok := src.(*image.RGBA); ok && alphaStrided(img.Pix, img.Stride, img.Rect.Dx(), img.Rect.Dy()) {
		return true
	}
	if img, ok := src.(*image.NRGBA); ok && alphaStrided(img.Pix, img.Stride, img.Rect.Dx(), img.Rect.Dy()) {
		return true
	}
	b := src.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := src.At(x, y).RGBA(); a != 0xFFFF {
				return false
			}
		}
	}
	return true
}

// alphaStrided is the fast path for 8-bit-per-channel images: scan the alpha
// byte of every pixel along the rows.
func alphaStrided(pix []uint8, stride, w, h int) bool {
	for y := 0; y < h; y++ {
		row := pix[y*stride:]
		for x := 0; x < w; x++ {
			if row[x*4+3] != 0xFF {
				return false
			}
		}
	}
	return true
}

// pngFidelityRails reports whether a PNG carries anything a re-encode would
// drop: APNG animation (acTL), an embedded ICC profile (iCCP) or EXIF (eXIf).
func pngFidelityRails(raw []byte) bool {
	at := 8 // past the signature
	for at+8 <= len(raw) {
		n := int(binary.BigEndian.Uint32(raw[at : at+4]))
		switch string(raw[at+4 : at+8]) {
		case "acTL", "iCCP", "eXIf":
			return true
		}
		if n < 0 || at+12+n > len(raw) {
			return true // malformed chunk: do not risk it
		}
		at += 12 + n
	}
	return false
}

// jpegFidelityRails reports whether a JPEG carries anything a re-encode
// would drop or misapply: an EXIF orientation other than 1 (Go's decoder
// ignores it, so resizing would bake the wrong rotation in, and the encoder
// drops the tag) or an ICC profile (the colors would shift).
func jpegFidelityRails(raw []byte) bool {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return true
	}
	at := 2
	for at+4 <= len(raw) && raw[at] == 0xFF {
		marker := raw[at+1]
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) { // stand-alone, no length
			at += 2
			continue
		}
		segLen := int(binary.BigEndian.Uint16(raw[at+2 : at+4]))
		if segLen < 2 || at+2+segLen > len(raw) {
			return false // malformed: leave the rest to the decoder
		}
		seg := raw[at+4 : at+2+segLen]
		switch marker {
		case 0xE1: // APP1: Exif
			if bytes.HasPrefix(seg, []byte("Exif\x00\x00")) && exifOrientationRail(seg[6:]) {
				return true
			}
		case 0xE2: // APP2: ICC profile
			if bytes.HasPrefix(seg, []byte("ICC_PROFILE")) {
				return true
			}
		}
		at += 2 + segLen
	}
	return false
}

// exifOrientationRail reads just the IFD0 orientation tag. true = the image
// carries an orientation a plain re-encode would discard.
func exifOrientationRail(tiff []byte) bool {
	if len(tiff) < 8 {
		return false
	}
	var bo binary.ByteOrder = binary.LittleEndian
	switch {
	case tiff[0] == 'M' && tiff[1] == 'M':
		bo = binary.BigEndian
	case tiff[0] != 'I' || tiff[1] != 'I':
		return false
	}
	off := int(bo.Uint32(tiff[4:8]))
	if off <= 0 || off+2 > len(tiff) {
		return false
	}
	n := int(bo.Uint16(tiff[off : off+2]))
	for i := 0; i < n; i++ {
		e := off + 2 + i*12
		if e+12 > len(tiff) {
			return false
		}
		if bo.Uint16(tiff[e:e+2]) != 0x0112 { // 274 = orientation
			continue
		}
		return bo.Uint16(tiff[e+8:e+10]) != 1
	}
	return false
}

// imageFitted remembers what an image is sent as, keyed by sha256 of its
// base64 and mime: the client replays the whole history every turn, so a
// past screenshot is decoded, resized and encoded once, not per turn, and
// every turn after the first sees byte-identical output — the upstream
// prefix cache stays intact. Bytes-bounded LRU with an idle eviction, the
// shape of magpie's fitted cache.
type imageFitCache struct {
	mu    sync.Mutex
	m     map[[32]byte]*list.Element
	order list.List // of *imageFitItem, most recently used first
	bytes int
}

type imageFitEntry struct {
	mime string
	b64  string // "" = examined and left as it came
}

type imageFitItem struct {
	key  [32]byte
	e    imageFitEntry
	used time.Time
}

var imageFitted = &imageFitCache{m: map[[32]byte]*list.Element{}}

func imageFitKey(mime, b64 string, allowConvert bool) [32]byte {
	h := sha256.New()
	h.Write([]byte(mime))
	h.Write([]byte{0})
	if allowConvert {
		h.Write([]byte{1})
	}
	h.Write([]byte(b64))
	var k [32]byte
	h.Sum(k[:0])
	return k
}

// letGo drops what has gone unsent for imageFitCacheIdle.
func (c *imageFitCache) letGo() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for back := c.order.Back(); back != nil && now.Sub(back.Value.(*imageFitItem).used) > imageFitCacheIdle; back = c.order.Back() {
		c.drop(back)
	}
}

func (c *imageFitCache) get(k [32]byte) (imageFitEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.m[k]
	if !ok {
		return imageFitEntry{}, false
	}
	it := el.Value.(*imageFitItem)
	it.used = time.Now()
	c.order.MoveToFront(el)
	return it.e, true
}

func (c *imageFitCache) put(k [32]byte, mime, b64 string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[k]; ok {
		return
	}
	e := imageFitEntry{mime: mime, b64: b64}
	if len(e.b64) > imageFitCacheBytes {
		return
	}
	for c.bytes+len(e.b64) > imageFitCacheBytes || len(c.m) >= imageFitCacheItems {
		c.drop(c.order.Back())
	}
	c.m[k] = c.order.PushFront(&imageFitItem{key: k, e: e, used: time.Now()})
	c.bytes += len(e.b64)
}

func (c *imageFitCache) drop(el *list.Element) {
	it := c.order.Remove(el).(*imageFitItem)
	delete(c.m, it.key)
	c.bytes -= len(it.e.b64)
}

// triangleResize scales src to w×h with a two-pass triangle (linear) filter,
// the kernel codex's client and magpie both use for prompt images (the
// image crate's FilterType::Triangle; the separable two-axis port follows
// magpie's MIT-licensed implementation). The kernel widens with the shrink
// factor so no source pixel is skipped on the way down.
type imageFitTap struct {
	from int
	w    []float32
}

func triangleWeights(src, n int) []imageFitTap {
	ratio := float64(src) / float64(n)
	scale := math.Max(ratio, 1)
	taps := make([]imageFitTap, n)
	for i := range taps {
		center := (float64(i) + 0.5) * ratio
		from := max(int(math.Floor(center-scale)), 0)
		to := min(int(math.Ceil(center+scale)), src)
		var ws []float32
		var sum float64
		for j := from; j < to; j++ {
			wt := math.Max(1-math.Abs((float64(j)+0.5-center)/scale), 0)
			ws = append(ws, float32(wt))
			sum += wt
		}
		if sum == 0 {
			j := min(max(int(center), 0), src-1)
			taps[i] = imageFitTap{from: j, w: []float32{1}}
			continue
		}
		for k := range ws {
			ws[k] = float32(float64(ws[k]) / sum)
		}
		taps[i] = imageFitTap{from: from, w: ws}
	}
	return taps
}

func triangleResize(src image.Image, w, h int) *image.RGBA {
	b := src.Bounds()
	rgba, ok := src.(*image.RGBA)
	if !ok || rgba.Rect.Min != (image.Point{}) {
		rgba = image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(rgba, rgba.Rect, src, b.Min, draw.Src)
	}
	sw, sh := b.Dx(), b.Dy()
	// across: sw×sh → w×sh
	mid := make([]float32, w*sh*4)
	for x, k := range triangleWeights(sw, w) {
		for y := 0; y < sh; y++ {
			row := rgba.Pix[y*rgba.Stride:]
			var r, g, bl, a float32
			for i, wt := range k.w {
				p := row[(k.from+i)*4:]
				r += wt * float32(p[0])
				g += wt * float32(p[1])
				bl += wt * float32(p[2])
				a += wt * float32(p[3])
			}
			o := (y*w + x) * 4
			mid[o], mid[o+1], mid[o+2], mid[o+3] = r, g, bl, a
		}
	}
	// down: w×sh → w×h
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y, k := range triangleWeights(sh, h) {
		for x := 0; x < w; x++ {
			var c [4]float32
			for i, wt := range k.w {
				o := ((k.from+i)*w + x) * 4
				c[0] += wt * mid[o]
				c[1] += wt * mid[o+1]
				c[2] += wt * mid[o+2]
				c[3] += wt * mid[o+3]
			}
			o := y*dst.Stride + x*4
			for ch := range c {
				dst.Pix[o+ch] = uint8(min(max(math.Round(float64(c[ch])), 0), 255))
			}
			// premultiplied: no channel past its alpha
			for ch := 0; ch < 3; ch++ {
				dst.Pix[o+ch] = min(dst.Pix[o+ch], dst.Pix[o+3])
			}
		}
	}
	return dst
}
