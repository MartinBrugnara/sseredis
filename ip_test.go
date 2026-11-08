package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		xff  []string // X-Forwarded-For headers, nil for none
		want string
	}{
		{nil, "192.0.2.1"},
		{[]string{"203.0.113.7"}, "203.0.113.7"},
		{[]string{" 203.0.113.7 , 10.0.0.2, 10.0.0.3"}, "203.0.113.7"},
		{[]string{"2001:db8::1, 10.0.0.2"}, "2001:db8::1"},
		{[]string{"203.0.113.7", "198.51.100.9"}, "203.0.113.7"}, // first header
		{[]string{"unknown"}, "192.0.2.1"},
		{[]string{""}, "192.0.2.1"},
	} {
		r := httptest.NewRequest("GET", "/", nil) // RemoteAddr 192.0.2.1:1234
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(r); got != tc.want {
			t.Errorf("X-Forwarded-For %q: clientIP = %q, want %q", tc.xff, got, tc.want)
		}
	}

	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[2001:db8::2]:443"
	if got := clientIP(r); got != "2001:db8::2" {
		t.Errorf("IPv6 RemoteAddr: clientIP = %q", got)
	}
}

// The IP sent to Django is the one from X-Forwarded-For, not the proxy's.
func TestServerSendsClientIPToDjango(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	srv, h := newTestServerWithAuth(t, newTestDjangoAuth(d))

	req := getRequest(t, srv.URL+"/?subscribe=x")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.2")
	req.AddCookie(&http.Cookie{Name: "sessionid", Value: "s3cr3t"})
	connect(t, req)
	waitClients(t, h, 1)

	if got := d.lastQuery(t).Get("clientip"); got != "203.0.113.7" {
		t.Errorf("clientip = %q, want 203.0.113.7", got)
	}
}
