package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

// sseClient reads an SSE stream line by line in the background.
type sseClient struct {
	resp  *http.Response
	lines chan string
	err   chan error
}

// connect opens an SSE stream. It does not wait for the response headers.
func connect(t *testing.T, req *http.Request) *sseClient {
	t.Helper()
	c := &sseClient{lines: make(chan string, 1024), err: make(chan error, 1)}
	// Hang up at the end of the test, even if no response arrived yet.
	ctx, cancel := context.WithCancel(req.Context())
	t.Cleanup(cancel)
	req = req.WithContext(ctx)
	respCh := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.err <- err
			return
		}
		respCh <- resp
		s := bufio.NewScanner(resp.Body)
		s.Buffer(nil, 1<<20)
		for s.Scan() {
			c.lines <- s.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		select {
		case resp := <-respCh:
			resp.Body.Close()
		default:
		}
	})
	return c
}

// next returns the data of the next event, skipping comments.
func (c *sseClient) next(t *testing.T) string {
	t.Helper()
	var data []string
	timeout := time.After(testTimeout)
	for {
		select {
		case err := <-c.err:
			t.Fatalf("request failed: %s", err)
		case line, ok := <-c.lines:
			if !ok {
				t.Fatal("stream closed")
			}
			switch {
			case line == "" && data != nil:
				return strings.Join(data, "\n")
			case strings.HasPrefix(line, "data: "):
				data = append(data, strings.TrimPrefix(line, "data: "))
			}
		case <-timeout:
			t.Fatal("timed out waiting for an event")
		}
	}
}

func getRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// newTestServer serves sseHandler without authentication.
func newTestServer(t *testing.T) (*httptest.Server, *Hub) {
	t.Helper()
	return newTestServerWithAuth(t, NoAuth{})
}

func newTestServerWithAuth(t *testing.T, auth Authenticator) (*httptest.Server, *Hub) {
	t.Helper()
	h, _ := newTestHub(t)
	srv := httptest.NewServer(http.HandlerFunc(sseHandler(h, auth)))
	t.Cleanup(srv.Close)
	return srv, h
}

func waitClients(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for h.clientCount() != n {
		if time.Now().After(deadline) {
			t.Fatalf("clientCount = %d, want %d", h.clientCount(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServerDeliversMessages(t *testing.T) {
	srv, h := newTestServer(t)
	c := connect(t, getRequest(t, srv.URL+"/?subscribe=x,y"))
	waitClients(t, h, 1)

	h.Publish("z", []byte("data: not for us\n\n"))
	h.Publish("y", []byte("data: hello\n\n"))
	if got := c.next(t); got != "hello" {
		t.Errorf("got %q, want hello", got)
	}
}

// A client that goes away must be removed, and must not block the others
// (it used to deadlock the hub).
func TestServerSurvivesDisconnects(t *testing.T) {
	srv, h := newTestServer(t)

	for i := 0; i < 20; i++ {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(conn, "GET /?subscribe=x HTTP/1.1\r\nHost: test\r\n\r\n")
		waitClients(t, h, 1)
		h.Publish("x", []byte("data: m\n\n"))
		conn.Close()
		for j := 0; j < 10; j++ {
			h.Publish("x", []byte("data: m\n\n"))
		}
		waitClients(t, h, 0)
	}

	c := connect(t, getRequest(t, srv.URL+"/?subscribe=x"))
	waitClients(t, h, 1)
	h.Publish("x", []byte("data: alive\n\n"))
	if got := c.next(t); got != "alive" {
		t.Errorf("got %q, want alive", got)
	}
}

// A client that stays connected but stops reading must not delay the
// others; it gets dropped once its queue is full.
func TestServerStalledClientDoesNotBlockOthers(t *testing.T) {
	srv, h := newTestServer(t)

	stalled, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stalled.Close() })
	stalled.(*net.TCPConn).SetReadBuffer(4096)
	fmt.Fprintf(stalled, "GET /?subscribe=x HTTP/1.1\r\nHost: test\r\n\r\n")
	waitClients(t, h, 1)

	c := connect(t, getRequest(t, srv.URL+"/?subscribe=x"))
	waitClients(t, h, 2)

	// Publish in lockstep with the reading client: if the stalled one
	// blocked the hub, this would time out.
	payload := strings.Repeat("p", 64<<10)
	for i := 0; i < 4*clientBuffer; i++ {
		h.Publish("x", []byte(fmt.Sprintf("data: %d %s\n\n", i, payload)))
		got := c.next(t)
		if want := fmt.Sprintf("%d ", i); !strings.HasPrefix(got, want) {
			t.Fatalf("message %d starts with %q", i, got[:10])
		}
	}
	waitClients(t, h, 1)
}

func TestRequestedChannels(t *testing.T) {
	for query, want := range map[string][]string{
		"":                       nil,
		"?subscribe=":            nil,
		"?subscribe=,,":          nil,
		"?subscribe=b,a,,b":      {"a", "b"},
		"?subscribe=a&other=1":   {"a"},
		"?subscribe=a%2Cb":       {"a", "b"},
		"?subscribe=user:1:feed": {"user:1:feed"},
	} {
		r := httptest.NewRequest("GET", "/"+query, nil)
		if got := requestedChannels(r); !slices.Equal(got, want) {
			t.Errorf("%q: channels = %q, want %q", query, got, want)
		}
	}
}

func TestServerRejectsNoChannels(t *testing.T) {
	srv, h := newTestServer(t)
	for _, query := range []string{"", "?subscribe=", "?subscribe=,"} {
		resp, err := http.Get(srv.URL + "/" + query)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", query, resp.StatusCode)
		}
	}
	if n := h.clientCount(); n != 0 {
		t.Errorf("clientCount = %d, want 0", n)
	}
}

// A multi-line payload reaches the client intact, as one event.
func TestServerMultiLinePayload(t *testing.T) {
	srv, h := newTestServer(t)
	c := connect(t, getRequest(t, srv.URL+"/?subscribe=x"))
	waitClients(t, h, 1)

	payload := "line1\n\nevent: evil\nline4"
	h.Publish("x", formatEvent([]byte(payload)))
	h.Publish("x", formatEvent([]byte("next")))
	if got := c.next(t); got != payload {
		t.Errorf("got %q, want %q", got, payload)
	}
	if got := c.next(t); got != "next" {
		t.Errorf("got %q, want next", got)
	}
}

// The headers arrive right away, before any message, and only on success.
func TestServerHeaders(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/?subscribe=x", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no response headers before any message: %s", err)
	}
	defer resp.Body.Close()
	for name, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	resp, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("error answered with Content-Type %q", ct)
	}
}

func TestSetCORSHeaders(t *testing.T) {
	for _, tc := range []struct {
		origin, allow, credentials string
	}{
		{"", "", ""},
		{"*", "*", ""},
		{"https://app.example.org", "https://app.example.org", "true"},
	} {
		h := http.Header{}
		setCORSHeaders(h, tc.origin)
		if got := h.Get("Access-Control-Allow-Origin"); got != tc.allow {
			t.Errorf("%q: Allow-Origin = %q, want %q", tc.origin, got, tc.allow)
		}
		if got := h.Get("Access-Control-Allow-Credentials"); got != tc.credentials {
			t.Errorf("%q: Allow-Credentials = %q, want %q", tc.origin, got, tc.credentials)
		}
	}
}

// newTimeoutServer serves sseHandler through newServer, with timeouts of d.
func newTimeoutServer(t *testing.T, d time.Duration) (*httptest.Server, *Hub) {
	t.Helper()
	oldHeader, oldIdle := readHeaderTimeout, idleTimeout
	readHeaderTimeout, idleTimeout = d, d
	t.Cleanup(func() { readHeaderTimeout, idleTimeout = oldHeader, oldIdle })
	h, _ := newTestHub(t)
	srv := httptest.NewUnstartedServer(nil)
	srv.Config = newServer(http.HandlerFunc(sseHandler(h, NoAuth{})))
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, h
}

// waitClosed fails the test if the server does not close conn in time.
func waitClosed(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(testTimeout))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Fatalf("connection not closed by the server: %s", err)
	}
}

func TestServerClosesIdleConnections(t *testing.T) {
	srv, _ := newTimeoutServer(t, 50*time.Millisecond)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	// A complete request (400, no channels), then nothing: the kept-alive
	// connection must be closed after idleTimeout.
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	waitClosed(t, conn)
}

func TestServerClosesSlowHeaders(t *testing.T) {
	srv, _ := newTimeoutServer(t, 50*time.Millisecond)
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	fmt.Fprintf(conn, "GET /?subscribe=x HTTP/1.1\r\nHost: test\r\n") // headers never end
	waitClosed(t, conn)
}

// The timeouts must not cut a stream that waits long for its next message.
func TestServerTimeoutsDoNotCutStreams(t *testing.T) {
	srv, h := newTimeoutServer(t, 50*time.Millisecond)
	c := connect(t, getRequest(t, srv.URL+"/?subscribe=x"))
	waitClients(t, h, 1)
	time.Sleep(300 * time.Millisecond)
	h.Publish("x", []byte("data: still here\n\n"))
	if got := c.next(t); got != "still here" {
		t.Errorf("got %q", got)
	}
}
