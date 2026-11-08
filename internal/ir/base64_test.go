package ir

import (
	"encoding/base64"
	"testing"
)

// BP-297（magpie b657a21 #5697）：google-genai SDK 用 URL-safe 字母表（-_）或
// 无 padding 编码 inline 图片；只认标准字母表的上游回 400 "Failed to
// base64-decode image data"。归一化=转标准补齐；本就标准补齐的逐字节不动；
// 混字母表/非法的原样保留（让上游报真实的错）。

func mustB64(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("seed %q: %v", s, err)
	}
	return string(b)
}

func TestStdBase64Normalizes(t *testing.T) {
	raw := mustB64(t, "TWFueSBobWFuZHM=") // "Many hands"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"URL-safe 无 padding", base64.RawURLEncoding.EncodeToString([]byte(raw)), base64.StdEncoding.EncodeToString([]byte(raw))},
		{"URL-safe 有 padding", base64.URLEncoding.EncodeToString([]byte(raw)), base64.StdEncoding.EncodeToString([]byte(raw))},
		{"标准无 padding", base64.RawStdEncoding.EncodeToString([]byte(raw)), base64.StdEncoding.EncodeToString([]byte(raw))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StdBase64(c.in); got != c.want {
				t.Fatalf("StdBase64 = %q, want %q", got, c.want)
			}
		})
	}
}

func TestStdBase64LeavesAlone(t *testing.T) {
	standard := base64.StdEncoding.EncodeToString([]byte("Many hands"))
	cases := []struct {
		name string
		in   string
	}{
		{"标准补齐逐字节不动", standard},
		{"空串", ""},
		{"混两种字母表", "Ab+/C-_d"},
		{"URL-safe 但解不开", "-_~-_"},
		{"无 padding 但解不开", "abc$"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StdBase64(c.in); got != c.in {
				t.Fatalf("StdBase64(%q) = %q, want unchanged", c.in, got)
			}
		})
	}
}

// 语义保持：转换只改字母表与 padding，解码后的字节相同。
func TestStdBase64PreservesBytes(t *testing.T) {
	raw := []byte{0xfb, 0xff, 0xbf, 0x1e, 0x00, 0xff} // 会同时产生 +/ 与 -_ 的字节
	urlSafe := base64.RawURLEncoding.EncodeToString(raw)
	got := StdBase64(urlSafe)
	dec, err := base64.StdEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("converted undecodable: %v", err)
	}
	if string(dec) != string(raw) {
		t.Fatalf("bytes changed: %v vs %v", dec, raw)
	}
}
