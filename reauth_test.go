package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testReauth = 20 * time.Millisecond

// switchAuth accepts until refuse or fail is set, and counts its calls.
// block, if set, holds every call after the first until it is closed.
type switchAuth struct {
	refuse, fail atomic.Bool
	calls        atomic.Int32
	block        chan struct{}
}

func (a *switchAuth) Auth(r *http.Request, _ *Client) error {
	if a.calls.Add(1) > 1 && a.block != nil {
		select {
		case <-a.block:
		case <-r.Context().Done():
			return r.Context().Err()
		}
	}
	switch {
	case a.refuse.Load():
		return &AuthError{http.StatusUnauthorized, "test"}
	case a.fail.Load():
		return errors.New("authenticator down")
	}
	return nil
}

func newReauthServer(t *testing.T, auth Authenticator) (*httptest.Server, *Hub) {
	t.Helper()
	h, _ := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(sseHandler(h, auth, testReauth)))
	t.Cleanup(srv.Close)
	return srv, h
}

// waitStreamClosed fails unless the server ends c's stream in time.
func waitStreamClosed(t *testing.T, c *sseClient) {
	t.Helper()
	timeout := time.After(testTimeout)
	for {
		select {
		case _, ok := <-c.lines:
			if !ok {
				return
			}
		case err := <-c.err:
			t.Fatalf("request failed: %s", err)
		case <-timeout:
			t.Fatal("the stream is still open")
		}
	}
}

// A session refused on a later check (logout, reset, permission gone)
// loses its open stream.
func TestReauthRefusedClosesStream(t *testing.T) {
	auth := &switchAuth{}
	srv, h := newReauthServer(t, auth)
	closedBefore := value(disconnectionsTotal.WithLabelValues("reauth_refused"))
	refusedBefore := value(reauthsTotal.WithLabelValues("refused"))

	c := connect(t, getRequest(t, srv.URL+"/?subscribe=reauth-refused"))
	waitClients(t, h, 1)
	eventually(t, "a check that passes", func() bool { return auth.calls.Load() >= 2 })
	h.Publish("reauth-refused", []byte("data: still in\n\n"))
	if got := c.next(t); got != "still in" {
		t.Fatalf("got %q, want still in", got)
	}

	auth.refuse.Store(true)
	waitStreamClosed(t, c)
	waitClients(t, h, 0)
	if d := value(disconnectionsTotal.WithLabelValues("reauth_refused")) - closedBefore; d != 1 {
		t.Errorf("disconnections reauth_refused +%v, want +1", d)
	}
	if d := value(reauthsTotal.WithLabelValues("refused")) - refusedBefore; d != 1 {
		t.Errorf("reauths refused +%v, want +1", d)
	}
}

// When the authenticator fails (dmbcau restarting), the streams stay open
// and are checked again at the next tick.
func TestReauthErrorKeepsStream(t *testing.T) {
	auth := &switchAuth{}
	srv, h := newReauthServer(t, auth)
	errorsBefore := value(reauthsTotal.WithLabelValues("error"))

	c := connect(t, getRequest(t, srv.URL+"/?subscribe=reauth-error"))
	waitClients(t, h, 1)
	auth.fail.Store(true)
	calls := auth.calls.Load()
	eventually(t, "three failed checks", func() bool { return auth.calls.Load() >= calls+3 })
	if d := value(reauthsTotal.WithLabelValues("error")) - errorsBefore; d < 2 {
		t.Errorf("reauths error +%v, want at least +2", d)
	}
	h.Publish("reauth-error", []byte("data: kept\n\n"))
	if got := c.next(t); got != "kept" {
		t.Fatalf("got %q, want kept", got)
	}

	// Back up, then refusing: the checks went on.
	auth.fail.Store(false)
	auth.refuse.Store(true)
	waitStreamClosed(t, c)
}

// A slow check doesn't hold the stream's messages up.
func TestReauthSlowDoesNotBlockMessages(t *testing.T) {
	auth := &switchAuth{block: make(chan struct{})}
	srv, h := newReauthServer(t, auth)

	c := connect(t, getRequest(t, srv.URL+"/?subscribe=reauth-slow"))
	waitClients(t, h, 1)
	eventually(t, "a check in progress", func() bool { return auth.calls.Load() >= 2 })
	for i := 0; i < 3; i++ {
		h.Publish("reauth-slow", []byte("data: m\n\n"))
		if got := c.next(t); got != "m" {
			t.Fatalf("got %q, want m", got)
		}
	}
	close(auth.block)
}

// A check asks Django exactly what the opening did: the same session, the
// browser's IP, the channels.
func TestReauthSendsTheSameQuery(t *testing.T) {
	var mu sync.Mutex
	status := http.StatusOK
	d := newFakeDjangoFunc(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
	})
	srv, h := newReauthServer(t, newTestDjangoAuth(d))

	req := getRequest(t, srv.URL+"/?subscribe=b,a")
	req.AddCookie(&http.Cookie{Name: "sessionid", Value: "s3cr3t"})
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	c := connect(t, req)
	waitClients(t, h, 1)
	eventually(t, "a check", func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.queries) >= 2
	})
	d.mu.Lock()
	first, check := d.queries[0].Encode(), d.queries[1].Encode()
	d.mu.Unlock()
	if check != first {
		t.Errorf("check asked %s, the opening %s", check, first)
	}
	want := "channel=a&channel=b&clientip=203.0.113.7&sessionid=s3cr3t"
	if first != want {
		t.Errorf("opening asked %s, want %s", first, want)
	}

	mu.Lock()
	status = http.StatusUnauthorized // e.g. logged out
	mu.Unlock()
	waitStreamClosed(t, c)
}

// With -reauth 0 a stream is checked only when it opens.
func TestReauthOff(t *testing.T) {
	auth := &switchAuth{}
	h, _ := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(sseHandler(h, auth, 0)))
	t.Cleanup(srv.Close)

	c := connect(t, getRequest(t, srv.URL+"/?subscribe=reauth-off"))
	waitClients(t, h, 1)
	auth.refuse.Store(true)
	time.Sleep(5 * testReauth)
	if n := auth.calls.Load(); n != 1 {
		t.Errorf("%d calls, want 1 (the opening)", n)
	}
	h.Publish("reauth-off", []byte("data: open\n\n"))
	if got := c.next(t); got != "open" {
		t.Fatalf("got %q, want open", got)
	}
}

func TestFirstReauth(t *testing.T) {
	every := time.Minute
	for i := 0; i < 1000; i++ {
		if d := firstReauth(every); d < every/2 || d >= every {
			t.Fatalf("firstReauth(%s) = %s, want in [%s, %s)", every, d, every/2, every)
		}
	}
	if d := firstReauth(1); d != 1 {
		t.Errorf("firstReauth(1) = %s", d)
	}
}
