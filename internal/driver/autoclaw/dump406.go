// 406 prompt-gate dump for the AutoClaw CN managed gateway (BP-325).
//
// # WHY
//
// The gate answers 406 with an EMPTY body, so a wording the refusedIdentities
// list does not know leaves no trace of what it objected to. On 2026-11-14 the
// gate started refusing every request (62/62) and the only visible evidence
// was "resending with a neutral system prompt" — the trigger text itself was
// gone. This dump makes the next occurrence self-documenting: the exact system
// messages that were refused land on disk, and refusedIdentities gets extended
// from evidence instead of guesses.
//
// # SHAPE
//
// Parity with the community plugin's dump406 (packages/autoclaw
// AUTOCLAW_406_DUMP_DIR) and with our own antigravity dump (BP-115): one file
// per refusal, best effort, every failure path is a log line. Privacy: ONLY
// system messages are written — the user's conversation never leaves the
// request path. The directory is fixed (no config knob to forget); pruning
// keeps the newest 40 files.
package autoclaw

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// dump406Dir is where refused system prompts land, relative to the gateway's
// working directory (the same convention as logs/gateway.log). A var so tests
// can point it at a scratch directory.
var dump406Dir = "logs/ac406"

// dump406Keep bounds the directory: the newest N dumps survive.
const dump406Keep = 40

// dump406 writes the system messages of a refused wire body. Best effort.
func (d *Driver) dump406(wireBody []byte) {
	var doc struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(wireBody, &doc); err != nil {
		log.Printf("autoclaw: 406 dump: parse refused body: %v (dump skipped)", err)
		return
	}
	var b strings.Builder
	b.WriteString("=== meta ===\n")
	b.WriteString("time: " + time.Now().Format(time.RFC3339) + "\n")
	b.WriteString("model: " + doc.Model + "\n")
	b.WriteString("note: system messages exactly as sent on the refused attempt (managedLead merged at send time is NOT included here)\n")
	n := 0
	for _, m := range doc.Messages {
		if m.Role != "system" {
			continue
		}
		n++
		b.WriteString("\n=== system[" + strconv.Itoa(n) + "] ===\n")
		b.WriteString(systemTextForDump(m.Content))
		b.WriteString("\n")
	}
	if n == 0 {
		b.WriteString("\n(no system messages in the refused body)\n")
	}
	if err := os.MkdirAll(dump406Dir, 0o700); err != nil {
		log.Printf("autoclaw: 406 dump dir: %v (dump skipped)", err)
		return
	}
	prune406Dumps()
	name := time.Now().Format("20060102-150405.000") + "_406.dump"
	if err := os.WriteFile(filepath.Join(dump406Dir, name), []byte(b.String()), 0o600); err != nil {
		log.Printf("autoclaw: 406 dump write: %v", err)
		return
	}
	log.Printf("autoclaw: dumped refused system prompt to %s", filepath.Join(dump406Dir, name))
}

// systemTextForDump renders one content field (plain string or parts array) as
// text. A part shape it does not understand is kept as its raw JSON so the dump
// never lies by omission.
func systemTextForDump(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil && len(parts) > 0 {
		var out strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				out.WriteString(p.Text)
			}
		}
		return out.String()
	}
	return "(unrecognised content shape) " + string(raw)
}

// prune406Dumps keeps the newest dump406Keep files. Best effort.
func prune406Dumps() {
	entries, err := os.ReadDir(dump406Dir)
	if err != nil || len(entries) <= dump406Keep {
		return
	}
	type filed struct {
		name string
		mod  time.Time
	}
	var files []filed
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".dump") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, filed{e.Name(), info.ModTime()})
	}
	if len(files) <= dump406Keep {
		return
	}
	for i := 0; i < len(files)-1; i++ {
		for j := i + 1; j < len(files); j++ {
			if files[j].mod.Before(files[i].mod) {
				files[i], files[j] = files[j], files[i]
			}
		}
	}
	for _, f := range files[:len(files)-dump406Keep] {
		_ = os.Remove(filepath.Join(dump406Dir, f.name))
	}
}
