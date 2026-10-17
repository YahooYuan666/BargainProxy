package router

import (
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/model"
)

func testCfg() *config.Config {
	cfg := config.Default()
	cfg.Models = []config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelWorkBuddy, UpstreamModel: "glm-5.3", Priority: 0},
		{Canonical: "glm-5.3", Channel: model.ChannelTrae, UpstreamModel: "glm-5.2", Priority: 1},
	}
	return cfg
}

func channelsOf(bs []Binding) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Channel)
	}
	return out
}

func TestReplaceHotReloadsResolve(t *testing.T) {
	cfg := testCfg()
	r := New(cfg)

	if got := channelsOf(r.Resolve("glm-5.3")); strings.Join(got, ",") != "workbuddy,trae" {
		t.Fatalf("initial resolve = %v", got)
	}

	// Swap the table: glm-5.3 now only via zcode.
	err := r.Replace([]config.ModelBinding{
		{Canonical: "glm-5.3", Channel: model.ChannelZCode, UpstreamModel: "GLM-5.3", Priority: 0},
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := channelsOf(r.Resolve("glm-5.3")); strings.Join(got, ",") != "zcode" {
		t.Fatalf("resolve after replace = %v", got)
	}
	if rows := r.Rows(); len(rows) != 1 || rows[0].Channel != model.ChannelZCode {
		t.Fatalf("rows after replace = %+v", rows)
	}

	// Invalid submissions are rejected and the live table is untouched.
	if err := r.Replace([]config.ModelBinding{{Canonical: "x", Channel: "bogus", UpstreamModel: "y"}}); err == nil {
		t.Fatal("unknown channel must be rejected")
	}
	if err := r.Replace([]config.ModelBinding{{Canonical: "", Channel: model.ChannelZCode, UpstreamModel: "y"}}); err == nil {
		t.Fatal("empty model name must be rejected")
	}
	if err := r.Replace(nil); err == nil {
		t.Fatal("empty table must be rejected")
	}
	if got := channelsOf(r.Resolve("glm-5.3")); strings.Join(got, ",") != "zcode" {
		t.Fatalf("rejected replace must not touch the table: %v", got)
	}
}

func TestChannelPrefixEscapeHatch(t *testing.T) {
	r := New(testCfg())

	// Prefix pins the request to one channel: no cross-channel fallback.
	got := r.Resolve("channel:trae:glm-5.3")
	if strings.Join(channelsOf(got), ",") != "trae" || got[0].UpstreamModel != "glm-5.2" {
		t.Fatalf("forced trae resolve = %+v", got)
	}
	got = r.Resolve("channel:workbuddy:glm-5.3")
	if strings.Join(channelsOf(got), ",") != "workbuddy" {
		t.Fatalf("forced workbuddy resolve = %+v", got)
	}

	// Unknown channel in the prefix: unresolvable (404 semantics), never a
	// fallback to normal resolution.
	if got := r.Resolve("channel:bogus:glm-5.3"); len(got) != 0 {
		t.Fatalf("unknown prefix channel must resolve empty, got %+v", got)
	}
	// Malformed prefixes are treated the same way.
	for _, bad := range []string{"channel:trae:", "channel::glm-5.3", "channel:trae"} {
		if got := r.Resolve(bad); len(got) != 0 {
			t.Fatalf("malformed prefix %q must resolve empty, got %+v", bad, got)
		}
	}
	// Unknown model behind a valid channel prefix: still empty.
	if got := r.Resolve("channel:trae:no-such-model"); len(got) != 0 {
		t.Fatalf("unknown model behind prefix must resolve empty, got %+v", got)
	}
	// A channel that is known but disabled resolves empty too.
	cfg := testCfg()
	cfg.Channels.Trae.Enabled = false
	r2 := New(cfg)
	if got := r2.Resolve("channel:trae:glm-5.3"); len(got) != 0 {
		t.Fatalf("disabled channel behind prefix must resolve empty, got %+v", got)
	}

	// The plain name is unaffected by the escape-hatch parsing.
	if got := channelsOf(r.Resolve("glm-5.3")); strings.Join(got, ",") != "workbuddy,trae" {
		t.Fatalf("plain resolve = %v", got)
	}

	// ParseChannelPrefix contract.
	if bare, ch, has := ParseChannelPrefix("channel:trae:glm-5.3"); !has || bare != "glm-5.3" || ch != "trae" {
		t.Fatalf("parse = %q %q %v", bare, ch, has)
	}
	if _, _, has := ParseChannelPrefix("glm-5.3"); has {
		t.Fatal("plain name must not report a prefix")
	}
}
