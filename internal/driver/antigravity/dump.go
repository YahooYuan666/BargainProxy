// Upstream dump for the Antigravity channel (BP-115).
//
// # WHY
//
// On 2026-10-25 23:21 the upstream answered 400 "Request contains an invalid
// argument" three times on a conversation that had just answered 200 eight
// times. That message names no field. Rebuilding the request by hand through
// this very driver — same account pool, same model id, with a tool
// declaration, with 500 tool round-trips (~1500 messages), and with the
// signature replay cache on — answered 200 every time. So the difference is
// somewhere in the REAL request, and without the outbound bytes there is
// nothing left to bisect.
//
// # SHAPE
//
// Same contract as the reference project's dump (misakano internal/upstream/
// dump.go): off unless a directory is configured (bodies carry the user's
// prompts), one file per FAILED request, outbound body + upstream answer,
// capped, and any write error is a log line that never disturbs the request.
//
// Only failures are dumped. A successful request needs no explanation, and
// dumping every request would fill the disk with prompts for no reason.
package antigravity

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultDumpMax is the outbound slice cap when the config names none.
const defaultDumpMax = 1 << 18 // 256 KB

// dumpFailure writes one self-contained file describing a request.
// Successful ones are skipped unless cfg.DumpOK is set (diagnosis only).
// Best effort: every failure path here is a log line, never an error.
func (d *Driver) dumpFailure(accountID, model string, status int, sentBody, upstreamBody []byte) {
	if status == 200 && !d.cfg.DumpOK {
		return
	}
	dir := strings.TrimSpace(d.cfg.DumpDir)
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("antigravity: dump dir %s: %v (dump skipped)", dir, err)
		return
	}
	max := d.cfg.DumpMaxBytes
	if max <= 0 {
		max = defaultDumpMax
	}
	sent := sentBody
	truncated := false
	if len(sent) > max {
		sent = sent[:max]
		truncated = true
	}
	var b strings.Builder
	b.WriteString("=== meta ===\n")
	b.WriteString("time: " + time.Now().Format(time.RFC3339) + "\n")
	b.WriteString("account: " + accountID + "\n")
	b.WriteString("model: " + model + "\n")
	b.WriteString("http_status: " + itoa(status) + "\n")
	b.WriteString("sent_bytes: " + itoa(len(sentBody)) + "\n")
	if truncated {
		b.WriteString("sent_truncated: true (cap " + itoa(max) + ")\n")
	}
	b.WriteString("\n=== request ===\n")
	b.Write(sent)
	b.WriteString("\n\n=== response ===\n")
	b.Write(upstreamBody)
	b.WriteString("\n")

	name := strings.Join([]string{
		time.Now().Format("20060102-150405.000"),
		sanitizeFilePart(accountID),
		itoa(status),
	}, "_") + ".dump"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
		log.Printf("antigravity: dump write %s: %v", name, err)
		return
	}
	log.Printf("antigravity: dumped failed request to %s", filepath.Join(dir, name))
}

// sanitizeFilePart keeps a filename safe: account ids contain ':' and '@'.
func sanitizeFilePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() > 40 {
		return b.String()[:40]
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
