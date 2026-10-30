// BP-220: the OpenCode free lane reads the TLS client fingerprint, and Go's
// crypto/tls handshake is refused where Bun's passes (2026-10-31: the exact
// same body+headers 200 from bun, 429 from Go across casing and HTTP/1.1).
// This sidecar is the final HTTP hop: the gateway sends the request to it
// over loopback, and the sidecar re-sends it to opencode.ai with Bun's own
// fetch, streaming the SSE response straight back.
//
// Contract: POST /proxy with header `x-bp-target` naming the absolute URL.
// Every other request header is forwarded as-is; the request body is
// forwarded as-is; the upstream status + body (streamed) come straight back.
// GET /health answers "ok".
const port = Number(process.argv[2] ?? 13141);
if (!Number.isFinite(port) || port <= 0 || port > 65535) {
  console.error("zen-sidecar: bad port", process.argv[2]);
  process.exit(2);
}

Bun.serve({
  port,
  hostname: "127.0.0.1",
  async fetch(req) {
    const url = new URL(req.url);
    if (url.pathname === "/health") return new Response("ok");
    if (url.pathname !== "/proxy") return new Response("not found", { status: 404 });
    const target = req.headers.get("x-bp-target");
    if (!target || !/^https:\/\//.test(target)) {
      return new Response("missing x-bp-target", { status: 400 });
    }
    const fwd = new Headers();
    req.headers.forEach((v, k) => {
      const name = k.toLowerCase();
      if (name === "x-bp-target" || name === "host" || name === "content-length" || name === "accept-encoding") return;
      fwd.set(name, v);
    });
    let body: BodyInit | undefined;
    if (req.method !== "GET" && req.method !== "HEAD") {
      body = await req.text();
    }
    let upstream: Response;
    try {
      upstream = await fetch(target, {
        method: req.method,
        headers: fwd,
        body,
        signal: req.signal,
        redirect: "manual",
      });
    } catch (err) {
      if (req.signal.aborted) return new Response(null, { status: 499 });
      return new Response("sidecar fetch failed: " + String(err), { status: 502 });
    }
    const out = new Headers();
    upstream.headers.forEach((v, k) => {
      const name = k.toLowerCase();
      if (name === "content-length" || name === "transfer-encoding" || name === "content-encoding") return;
      out.set(name, v);
    });
    return new Response(upstream.body, { status: upstream.status, headers: out });
  },
});
console.log(`zen-sidecar listening on 127.0.0.1:${port}`);
