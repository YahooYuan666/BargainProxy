package server_test

import (
	"context"
	"strings"
	"testing"

	"bargainproxy/internal/config"
	"bargainproxy/internal/driver"
	"bargainproxy/internal/driver/antigravity"
	"bargainproxy/internal/driver/openaicompat"
	"bargainproxy/internal/driver/opencodezen"
	"bargainproxy/internal/driver/trae"
	"bargainproxy/internal/driver/workbuddy"
	"bargainproxy/internal/driver/zcode"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"fmt"
	"net/http"
	"net/http/httptest"
	"time"
)

func TestBP76DriversRejectMissingNativeTerminal(t *testing.T) {
	for _, channel := range []string{model.ChannelAntigravity, model.ChannelWorkBuddy, model.ChannelOpenAICompat, model.ChannelOpenCodeZen, model.ChannelTrae, model.ChannelZCode} {
		t.Run(channel, func(t *testing.T) {
			for _, terminal := range []bool{false, true} {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					switch channel {
					case model.ChannelAntigravity:
						fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}]}}\n\n")
						if terminal {
							fmt.Fprint(w, "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n")
						}
					case model.ChannelZCode:
						fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
						if terminal {
							fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
						}
					default:
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
						if terminal {
							fmt.Fprint(w, "data: [DONE]\n\n")
						}
					}
				}))
				cfg := config.Default()
				var d driver.Driver
				acc := &model.Account{ID: "terminal-test", Channel: channel, CredsJSON: []byte(`{"access_token":"dummy","jwt":"dummy","token":"dummy","refresh_token":"dummy","project_id":"p","expiry":"2099-01-01T00:00:00Z","uid":"u"}`)}
				switch channel {
				case model.ChannelAntigravity:
					cfg.Channels.Antigravity.BaseURL = up.URL
					cfg.Channels.Antigravity.DailyURL = up.URL
					d = antigravity.New(cfg.Channels.Antigravity, nil)
				case model.ChannelWorkBuddy:
					cfg.Channels.WorkBuddy.ChatBaseCN = up.URL
					d = workbuddy.New(cfg.Channels.WorkBuddy, nil)
				case model.ChannelOpenAICompat:
					cfg.Channels.OpenAICompat.BaseURL = up.URL
					d = openaicompat.New(cfg.Channels.OpenAICompat, nil)
				case model.ChannelOpenCodeZen:
					cfg.Channels.OpenCodeZen.BaseURL = up.URL
					zen := opencodezen.New(cfg.Channels.OpenCodeZen, nil)
					zen.SetWireResolver(func(string) string { return "chat" })
					d = zen
				case model.ChannelTrae:
					cfg.Channels.Trae.AgentHost = up.URL
					d = trae.New(cfg.Channels.Trae, nil)
				case model.ChannelZCode:
					cfg.Channels.ZCode.BaseURL = up.URL
					d = zcode.New(cfg.Channels.ZCode, nil)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_, err := d.Stream(ctx, driver.ChatCall{Account: acc, UpstreamModel: "glm-5.3", Request: &ir.ChatRequest{Model: "glm-5.3", Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.ContentPart{{Kind: "text", Text: "ping"}}}}}}, func(ir.StreamEvent) error { return nil })
				cancel()
				up.Close()
				if terminal && err != nil {
					t.Fatalf("valid terminal rejected: %v", err)
				}
				if !terminal && (err == nil || !strings.Contains(err.Error(), "without")) {
					t.Fatalf("missing terminal not classified: %v", err)
				}
			}
		})
	}
}
