package uperr

// RequestTimeout builds the verdict for an HTTP 408 from the upstream.
//
// BP-335 (sub2api #8968): 408 was not in ANY driver's classifier, so it fell to
// the generic default — ClassInternal, which the pool charges a strike for. A
// request timeout is the upstream saying "I took too long with this one", which
// is exactly the ClassUpstreamTimeout story: not the account's fault, and
// another account hits the same slow upstream. No penalty, and the client's
// remedy is to retry rather than to re-route.
func RequestTimeout(status int, format string, args ...any) *Error {
	return New(ClassUpstreamTimeout, status, format, args...)
}
