package main

import (
	"bufio"
	"context"
	"fmt"
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
