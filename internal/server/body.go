package server

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxInboundBody is the request-body ceiling shared by every ingress.
const maxInboundBody = 32 << 20

// readRequestBody reads the request body under a size ceiling, decodes any
// Content-Encoding the client applied, and reports an honest 413 when the
// ceiling was hit.
//
// BP-335 (Coding2API 3e5e8aed): the old io.ReadAll(io.LimitReader(...)) SILENTLY
// TRUNCATED an oversized body. json.Unmarshal then failed on the cut-off bytes
// and the client got 400 "invalid_request" — neither the right status nor any
// hint about size. Reading the ceiling+1st byte distinguishes "the body ended"
// from "we stopped reading", so the answer can be 413 with both numbers.
//
// BP-335 (CLIProxyAPI 6e7a1b29): pi and Codex CLI send zstd-compressed request
// bodies. Without decoding, every such request was a 400 that was very hard to
// diagnose. gzip/x-gzip/deflate are decoded here; zstd needs a dependency this
// module does not carry (see decodeBody); an unknown encoding passes through.
//
// limit<=0 means maxInboundBody.
func readRequestBody(r *http.Request, limit int64) ([]byte, int, error) {
	if limit <= 0 {
		limit = maxInboundBody
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, 0, fmt.Errorf("read body: %w", err)
	}
	if int64(len(raw)) > limit {
		// The status and both numbers, so the operator knows the actual size.
		return nil, http.StatusRequestEntityTooLarge,
			fmt.Errorf("request body exceeds the %d-byte limit (read at least %d bytes)", limit, len(raw))
	}
	decoded, err := decodeBody(raw, r.Header.Get("Content-Encoding"))
	if err != nil {
		return nil, 0, err
	}
	return decoded, 0, nil
}

// statusOr returns status when it is a real HTTP status, else fallback.
func statusOr(status, fallback int) int {
	if status >= 400 && status <= 599 {
		return status
	}
	return fallback
}

// decodeBody applies a Content-Encoding to a body that is already in memory.
func decodeBody(raw []byte, encoding string) ([]byte, error) {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	switch enc {
	case "", "identity":
		return raw, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("gzip body: %w", err)
		}
		defer zr.Close()
		out, err := io.ReadAll(io.LimitReader(zr, maxInboundBody+1))
		if err != nil {
			return nil, fmt.Errorf("gzip body: %w", err)
		}
		if len(out) > maxInboundBody {
			return nil, fmt.Errorf("decompressed body exceeds the %d-byte limit", maxInboundBody)
		}
		return out, nil
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("deflate body: %w", err)
		}
		defer zr.Close()
		out, err := io.ReadAll(io.LimitReader(zr, maxInboundBody+1))
		if err != nil {
			return nil, fmt.Errorf("deflate body: %w", err)
		}
		if len(out) > maxInboundBody {
			return nil, fmt.Errorf("decompressed body exceeds the %d-byte limit", maxInboundBody)
		}
		return out, nil
	case "zstd":
		// zstd is what pi and Codex CLI actually send (CLIProxyAPI 6e7a1b29).
		// Decoding it needs github.com/klauspost/compress, which is NOT a
		// dependency of this module — adding one is a deliberate decision, not
		// something to slip in with a fix. Until then the body passes through
		// and the parser rejects it; the WARN below is what makes that
		// diagnosable instead of a mystery 400.
		return nil, fmt.Errorf("zstd-compressed request bodies are not supported yet (add github.com/klauspost/compress to enable)")
	default:
		// An encoding we do not know: hand the bytes through. The parser will
		// reject them with a clear message, which beats pretending we decoded.
		return raw, nil
	}
}
