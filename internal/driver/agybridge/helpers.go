package agybridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"bargainproxy/internal/driver"
	"bargainproxy/internal/ir"
	"bargainproxy/internal/model"
	"bargainproxy/internal/uperr"
)

// scanLines wraps a reader with the line scanner used by the login pump.
func scanLines(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 8*1024), 64*1024)
	return sc
}

// buildPrompt renders one turn for the ACP session.
//
// The session holds the conversation and the system prompt travels in ACP's
// own `_meta.antigravity.systemPrompt.append` field (see prompt()), so the
// prompt body is USER TEXT ONLY. Assistant prefills are not replayed — an ACP
// prompt is user-side only.
func buildPrompt(req *ir.ChatRequest) string {
	start := 0
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "assistant" {
			start = i + 1
			break
		}
	}
	var b strings.Builder
	for _, m := range req.Messages[start:] {
		text := textOf(m)
		if text == "" {
			continue
		}
		switch m.Role {
		case "user":
			b.WriteString(text)
			b.WriteString("\n\n")
		case "tool":
			b.WriteString("[tool result]\n")
			b.WriteString(text)
			b.WriteString("\n\n")
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		out = "(continue)"
	}
	return out
}

func textOf(m ir.Message) string {
	var b strings.Builder
	for _, p := range m.Content {
		if p.Kind == "text" && p.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// parseUpdate extracts (sessionUpdate kind, text) from one session/update
// notification. Non-text content blocks (images/diffs) are dropped — the
// bridge is a text channel.
func parseUpdate(m rpcMsg) (string, string) {
	var params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(m.Params, &params); err != nil {
		return "", ""
	}
	if params.Update.Content == nil {
		return params.Update.SessionUpdate, ""
	}
	return params.Update.SessionUpdate, params.Update.Content.Text
}

// classify maps bridge failures into the shared error taxonomy. agy-side
// auth problems surface as spawn/initialize failures; refusals come back as
// normal stopReason=refusal turns, not errors.
func (d *Driver) classify(err error, acc *model.Account) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not logged in"), strings.Contains(msg, "keyring"),
		strings.Contains(msg, "auth"):
		return uperr.New(uperr.ClassAuth, 0, "agybridge: %s", truncateStr(msg, 300))
	case strings.Contains(msg, "exec"), strings.Contains(msg, "spawn agy"),
		strings.Contains(msg, "agy exited"):
		return uperr.New(uperr.ClassUpstreamBusy, 0, "agybridge: agy process unavailable: %s", truncateStr(msg, 300))
	default:
		return uperr.New(uperr.ClassInternal, 0, "agybridge: %s", truncateStr(msg, 300))
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// approxTokens gives the response accounting a body estimate. ACP carries no
// usage numbers on the streaming path; the gateway's budget math runs off this
// proxy.
func approxTokens(s string) int { return len(s) / 4 }

// Probe implements driver.Prober with the cheapest honest signal: whether the
// account's isolated home holds a token file.
func (d *Driver) Probe(ctx context.Context, acc *model.Account) (*driver.ProbeResult, error) {
	home, err := d.accountHome(acc)
	if err != nil {
		return nil, err
	}
	if homeHasToken(home) {
		return &driver.ProbeResult{Note: "ok: 已登录（agy 管理 token）"}, nil
	}
	return &driver.ProbeResult{Note: "尚未登录 — 面板里点「开始登录」"}, nil
}

// LoginFlows satisfies driver.Interactive for the panel's onboarding UI. The
// real flow lives in StartBrowserLogin (login.go): a loopback OAuth listener in
// THIS process, matching upstream agy-acp — the terminal `agy acp --login`
// flow loses its PKCE verifier the moment the child exits, which is exactly the
// `invalid_grant "Invalid code verifier"` this repository hit on 2026-11-25.
func (d *Driver) LoginFlows() driver.LoginFlows {
	return driver.LoginFlows{
		SupportsDeviceFlow: false,
		StartLogin: func(ctx context.Context, onDone func(credsJSON []byte, name string, err error)) (string, error) {
			return "", fmt.Errorf("agybridge: use the panel's Agy-Bridge sign-in (StartBrowserLogin)")
		},
	}
}

// resolveAgyBin finds the agy executable. Order: explicit config path, then
// PATH, then the official installer's default location. The official Windows
// installer does NOT add its directory to PATH (it prints a warning and moves
// on), so relying on PATH alone made every spawn fail with "exit status 2"
// (2026-11-25 field: the panel login page reported exit status 2 while the
// same command worked from a shell).
func resolveAgyBin(configured string) string {
	if p := strings.TrimSpace(configured); p != "" {
		return p
	}
	if p, err := exec.LookPath("agy"); err == nil {
		return p
	}
	var candidates []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		candidates = append(candidates, filepath.Join(local, "agy", "bin", "agy.exe"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(home, ".local", "bin", "agy"),
			filepath.Join(home, "AppData", "Local", "agy", "bin", "agy.exe"),
		)
	}
	candidates = append(candidates, `C:\Users\`+os.Getenv("USERNAME")+`\AppData\Local\agy\bin\agy.exe`)
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return "agy" // last resort: let the spawn report the failure
}

func (d *Driver) agyBinary() string { return resolveAgyBin(d.cfg.AgyPath) }

// updateSessionID reads the sessionId out of one session/update frame ("" when
// the frame has none).
func updateSessionID(m rpcMsg) string {
	var params struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(m.Params, &params) != nil {
		return ""
	}
	return params.SessionID
}

// systemTextOf extracts the client's system messages (joined) so they can be
// delivered through ACP's dedicated systemPrompt field instead of the prompt
// body.
func systemTextOf(req *ir.ChatRequest) string {
	if req == nil {
		return ""
	}
	var parts []string
	for _, m := range req.Messages {
		if m.Role != "system" {
			continue
		}
		if t := textOf(m); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

