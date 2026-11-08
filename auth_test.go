package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeDjango is a stand-in for the dmbcau authenticator API.
type fakeDjango struct {
	*httptest.Server
	mu      sync.Mutex
	queries []url.Values
	conns   int // connections opened to the server
}

func newFakeDjango(t *testing.T, status int) *fakeDjango {
	return newFakeDjangoFunc(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte("some body"))
	})
}

func newFakeDjangoFunc(t *testing.T, answer http.HandlerFunc) *fakeDjango {
	t.Helper()
	d := &fakeDjango{}
	d.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.queries = append(d.queries, r.URL.Query())
		d.mu.Unlock()
		answer(w, r)
	}))
	d.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			d.mu.Lock()
			d.conns++
			d.mu.Unlock()
		}
	}
	d.Start()
	t.Cleanup(d.Close)
	return d
}

func newTestDjangoAuth(d *fakeDjango) DjangoAuth {
	return NewDjangoAuth(d.URL+"/authenticator/", "", time.Second)
}

func (d *fakeDjango) lastQuery(t *testing.T) url.Values {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queries) == 0 {
		t.Fatal("django was not called")
	}
	return d.queries[len(d.queries)-1]
}

func authRequest(cookie string) *http.Request {
	r := httptest.NewRequest("GET", "/?subscribe=x", nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "sessionid", Value: cookie})
	}
	return r
}

func authStatus(err error) int {
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return authErr.Status
	}
	return 0
}

func TestDjangoAuthAccepts(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	auth := newTestDjangoAuth(d)

	if err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"y", "x"})); err != nil {
		t.Fatalf("Auth: %s", err)
	}
	q := d.lastQuery(t)
	if got := q["channel"]; !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("channel = %q, want [x y]", got)
	}
	if got := q.Get("sessionid"); got != "s3cr3t" {
		t.Errorf("sessionid = %q", got)
	}
	if got := q.Get("clientip"); got != "1.2.3.4" {
		t.Errorf("clientip = %q", got)
	}
}

func TestDjangoAuthRefuses(t *testing.T) {
	d := newFakeDjango(t, http.StatusForbidden)
	auth := newTestDjangoAuth(d)

	err := auth.Auth(authRequest("expired"), newClientFor("1.2.3.4", []string{"x"}))
	if got := authStatus(err); got != http.StatusForbidden {
		t.Errorf("err = %v, want status 403", err)
	}
}

func TestDjangoAuthNeedsCookie(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	auth := newTestDjangoAuth(d)

	err := auth.Auth(authRequest(""), newClientFor("1.2.3.4", []string{"x"}))
	if got := authStatus(err); got != http.StatusUnprocessableEntity {
		t.Errorf("err = %v, want status 422", err)
	}
	if len(d.queries) != 0 {
		t.Error("django was called without a session")
	}
}

func TestDjangoAuthDown(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	d.Close()
	auth := newTestDjangoAuth(d)

	err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"}))
	if err == nil || authStatus(err) != 0 {
		t.Errorf("err = %v, want a plain error", err)
	}
}

// refuseAll refuses every request with status.
type refuseAll struct{ status int }

func (a refuseAll) Auth(*http.Request, *Client) error {
	return &AuthError{a.status, "test"}
}

type failAll struct{}

func (failAll) Auth(*http.Request, *Client) error { return errors.New("boom") }

func TestServerAnswersAuthFailures(t *testing.T) {
	for _, tc := range []struct {
		auth Authenticator
		want int
	}{
		{refuseAll{http.StatusForbidden}, http.StatusForbidden},
		{failAll{}, http.StatusInternalServerError},
	} {
		srv, h := newTestServerWithAuth(t, tc.auth)
		resp, err := http.Get(srv.URL + "/?subscribe=x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%T: status %d, want %d", tc.auth, resp.StatusCode, tc.want)
		}
		if n := h.clientCount(); n != 0 {
			t.Errorf("%T: refused client was subscribed", tc.auth)
		}
	}
}

func TestDjangoAuthReusesConnections(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	auth := newTestDjangoAuth(d)
	for i := 0; i < 5; i++ {
		if err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"})); err != nil {
			t.Fatal(err)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conns != 1 {
		t.Errorf("opened %d connections for 5 calls, want 1 (response bodies not closed?)", d.conns)
	}
}

func TestDjangoAuthTimeout(t *testing.T) {
	release := make(chan struct{})
	d := newFakeDjangoFunc(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	defer close(release) // before d.Close, which waits for the handler
	auth := NewDjangoAuth(d.URL+"/authenticator/", "", 100*time.Millisecond)

	start := time.Now()
	err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"}))
	if err == nil {
		t.Fatal("hanging django accepted the request")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("gave up after %s", d)
	}
}

func TestDjangoAuthDoesNotFollowRedirects(t *testing.T) {
	d := newFakeDjangoFunc(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login/" {
			w.WriteHeader(http.StatusOK) // a login page
			return
		}
		http.Redirect(w, r, "/login/", http.StatusFound)
	})
	auth := newTestDjangoAuth(d)

	err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"}))
	if got := authStatus(err); got != http.StatusBadGateway {
		t.Errorf("err = %v, want status 502", err)
	}
}

func TestDjangoAuthServerError(t *testing.T) {
	d := newFakeDjango(t, http.StatusInternalServerError)
	err := newTestDjangoAuth(d).Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"}))
	if got := authStatus(err); got != http.StatusBadGateway {
		t.Errorf("err = %v, want status 502", err)
	}
}

// Django decides which channels a session may read.
func TestServerChannelsAuthorizedByDjango(t *testing.T) {
	d := newFakeDjangoFunc(t, func(w http.ResponseWriter, r *http.Request) {
		for _, ch := range r.URL.Query()["channel"] {
			if ch != "public" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
		}
	})
	srv, h := newTestServerWithAuth(t, newTestDjangoAuth(d))

	req := getRequest(t, srv.URL+"/?subscribe=public,private")
	req.AddCookie(&http.Cookie{Name: "sessionid", Value: "s3cr3t"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}

	req = getRequest(t, srv.URL+"/?subscribe=public")
	req.AddCookie(&http.Cookie{Name: "sessionid", Value: "s3cr3t"})
	c := connect(t, req)
	waitClients(t, h, 1)
	h.Publish("public", []byte("data: ok\n\n"))
	if got := c.next(t); got != "ok" {
		t.Errorf("got %q", got)
	}
}

// With a socket, DjangoAuth connects to it whatever the URL host, which
// still gives the Host header and the path.
func TestDjangoAuthUnixSocket(t *testing.T) {
	path := socketPath(t)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var host, reqPath string
	var query url.Values
	d := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		host, reqPath, query = r.Host, r.URL.Path, r.URL.Query()
		mu.Unlock()
		if r.URL.Query().Get("sessionid") != "s3cr3t" {
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	d.Listener.Close()
	d.Listener = l
	d.Start()
	t.Cleanup(d.Close)

	// The host does not resolve: only the socket can answer.
	auth := NewDjangoAuth("http://dmbcau.invalid/authenticator/", path, time.Second)
	if err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"})); err != nil {
		t.Fatalf("Auth: %s", err)
	}
	mu.Lock()
	if host != "dmbcau.invalid" || reqPath != "/authenticator/" || query.Get("clientip") != "1.2.3.4" {
		t.Errorf("got Host %q, path %q, query %v", host, reqPath, query)
	}
	mu.Unlock()

	err = auth.Auth(authRequest("expired"), newClientFor("1.2.3.4", []string{"x"}))
	if got := authStatus(err); got != http.StatusForbidden {
		t.Errorf("err = %v, want status 403", err)
	}
}

func TestDjangoAuthUnixSocketDown(t *testing.T) {
	auth := NewDjangoAuth("http://dmbcau/authenticator/", socketPath(t), time.Second)
	err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"}))
	if err == nil || authStatus(err) != 0 {
		t.Errorf("err = %v, want a plain error", err)
	}
}
