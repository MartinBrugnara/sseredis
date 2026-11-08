package main

import (
	"net"
	"net/http"
	"strings"
)

// clientIP returns the IP address of the client that made r.
//
// Behind nginx it is the first (leftmost) address of X-Forwarded-For. This
// is only safe because the public-facing nginx overwrites the header
// instead of appending to it, and because sseredis is only reachable
// through nginx: see "Client IP" in README.md.
//
// Without the header (or with a malformed one) it is the address of the
// peer that connected to us.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		first = strings.TrimSpace(first)
		if ip := net.ParseIP(first); ip != nil {
			return ip.String()
		}
		Warn("Ignoring malformed X-Forwarded-For %q from %s", xff, r.RemoteAddr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
