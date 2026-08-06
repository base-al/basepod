package api

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// trustedProxyContextKey marks a request as having arrived through a
// trusted listener: the dashboard's unix-socket route, which only
// bp-caddy's container can ever reach (see internal/server's
// prepareDashboardListener doc comment), fronted by Caddy's own
// reverse_proxy. Every other request — anything that reached this
// process over its public loopback listener — carries no such marker,
// even if it forges an X-Forwarded-For header, since that header is only
// ever consulted when this marker is present.
type trustedProxyContextKey struct{}

// TrustedProxyMiddleware marks every request that reaches it as arriving
// through a trusted reverse proxy. internal/server wraps ONLY the
// unix-socket dashboard listener's handler with this — the public
// loopback listener's handler is left unwrapped — so a request that
// reaches this process directly (bypassing Caddy) can never spoof its
// way into trusted status: there is no request header or other
// client-controlled input that flips this marker, only which listener
// physically accepted the connection.
func TrustedProxyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), trustedProxyContextKey{}, true)))
	})
}

// isTrustedProxy reports whether ctx carries the trusted-listener marker
// set by TrustedProxyMiddleware.
func isTrustedProxy(ctx context.Context) bool {
	v, _ := ctx.Value(trustedProxyContextKey{}).(bool)
	return v
}

// clientIP extracts the address rate-limit bucketing should key on.
//
// On a trusted request (see isTrustedProxy) it takes the LAST hop of
// X-Forwarded-For: Caddy's reverse_proxy appends the real client address
// to that header on every request it forwards, so the last entry is the
// one Caddy itself set — closer hops (if any) would only exist if
// something upstream of Caddy also proxied the request, which BasePod's
// deployment model doesn't do. If the header is absent or its last entry
// doesn't parse as an IP, this falls back to RemoteAddr like the
// untrusted path.
//
// On an untrusted request, X-Forwarded-For is never read at all — a
// direct client hitting the public loopback listener could set that
// header to anything, and honoring it there would let a single attacker
// spread a login flood across fabricated keys, defeating the per-IP
// limit entirely.
func clientIP(r *http.Request) string {
	if isTrustedProxy(r.Context()) {
		if ip, ok := lastForwardedFor(r.Header.Get("X-Forwarded-For")); ok {
			return ip
		}
	}
	return remoteAddrHost(r)
}

// remoteAddrHost extracts the host portion of r.RemoteAddr, falling back
// to the raw value if it isn't a host:port pair (e.g. in atypical test
// transports).
func remoteAddrHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// lastForwardedFor parses the last comma-separated hop out of an
// X-Forwarded-For header value and reports whether it looks like a real
// IP address. Caddy sets this to a bare IP (no port), but a stray
// "host:port" or bracketed IPv6 form is tolerated defensively. An empty
// header, an empty last hop, or a last hop that doesn't parse as an IP at
// all (garbage, or a hostname) reports ok=false so the caller falls back
// to RemoteAddr instead of bucketing on nonsense.
func lastForwardedFor(xff string) (ip string, ok bool) {
	if xff == "" {
		return "", false
	}
	parts := strings.Split(xff, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == "" {
		return "", false
	}
	if host, _, err := net.SplitHostPort(last); err == nil {
		last = host
	}
	if net.ParseIP(last) == nil {
		return "", false
	}
	return last, true
}
