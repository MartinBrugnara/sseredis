package main

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// socketPath returns the path of a socket file in a new temporary
// directory. Socket paths are limited to ~100 bytes, and t.TempDir can be
// longer than that (long TMPDIR, long test names), so it falls back to
// /tmp.
func socketPath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if len(dir) > 80 {
		var err error
		if dir, err = os.MkdirTemp("/tmp", "sseredis-test-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
	}
	return filepath.Join(dir, "s.sock")
}

// unixClient returns an HTTP client connecting to the unix socket at path.
func unixClient(path string) *http.Client {
	return &http.Client{Transport: unixTransport(path)}
}

func get(t *testing.T, c *http.Client, url string, header http.Header) string {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSplitNetAddr(t *testing.T) {
	for _, tc := range []struct{ addr, network, address string }{
		{":80", "tcp", ":80"},
		{"redis:6379", "tcp", "redis:6379"},
		{"[::1]:6379", "tcp", "[::1]:6379"},
		{"unix:/run/redis/redis-server.sock", "unix", "/run/redis/redis-server.sock"},
		{"unix:rel.sock", "unix", "rel.sock"},
	} {
		network, address := splitNetAddr(tc.addr)
		if network != tc.network || address != tc.address {
			t.Errorf("splitNetAddr(%q) = %q, %q, want %q, %q", tc.addr, network, address, tc.network, tc.address)
		}
	}
}

func TestParseFileMode(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want fs.FileMode
		ok   bool
	}{
		{"0660", 0o660, true},
		{"666", 0o666, true},
		{"0", 0, true},
		{"0777", 0o777, true},
		{"01777", 0, false}, // no sticky/setuid bits
		{"0668", 0, false},
		{"rw-rw----", 0, false},
		{"", 0, false},
	} {
		got, err := parseFileMode(tc.s)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseFileMode(%q) = %o, %v", tc.s, got, err)
		}
	}
}

// The unix socket gets the requested mode, serves HTTP and is removed
// when the listener is closed. Behind it clientIP falls back to "unix",
// or uses X-Forwarded-For.
func TestListenUnix(t *testing.T) {
	path := socketPath(t)
	l, err := listen(unixPrefix+path, "0640")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Type() != fs.ModeSocket || fi.Mode().Perm() != 0o640 {
		t.Errorf("socket mode %s, want a socket with 0640", fi.Mode())
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, clientIP(r))
	})}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(l) }()

	c := unixClient(path)
	if got := get(t, c, "http://sseredis/", nil); got != "unix" {
		t.Errorf("clientIP without X-Forwarded-For = %q, want unix", got)
	}
	h := http.Header{"X-Forwarded-For": {"203.0.113.7, 10.0.0.2"}}
	if got := get(t, c, "http://sseredis/", h); got != "203.0.113.7" {
		t.Errorf("clientIP with X-Forwarded-For = %q, want 203.0.113.7", got)
	}

	srv.Close()
	if err := <-done; err != http.ErrServerClosed {
		t.Errorf("Serve: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket file still there after close: %v", err)
	}
}

func TestListenUnixRemovesStaleSocket(t *testing.T) {
	path := socketPath(t)
	// A socket left behind by a crashed run.
	old, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	old.(*net.UnixListener).SetUnlinkOnClose(false)
	old.Close()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("stale socket not created: %v", err)
	}

	l, err := listen(unixPrefix+path, "0660")
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	defer l.Close()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestListenUnixKeepsRegularFile(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := listen(unixPrefix+path, "0660"); err == nil {
		l.Close()
		t.Fatal("listen replaced a regular file")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "precious" {
		t.Errorf("regular file changed: %q, %v", b, err)
	}
}

func TestListenUnixBadMode(t *testing.T) {
	path := socketPath(t)
	if l, err := listen(unixPrefix+path, "rw"); err == nil {
		l.Close()
		t.Fatal("listen accepted mode rw")
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket created despite the bad mode: %v", err)
	}
}

// For TCP the mode is ignored.
func TestListenTCP(t *testing.T) {
	l, err := listen("127.0.0.1:0", "not a mode")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if l.Addr().Network() != "tcp" {
		t.Errorf("network %s", l.Addr().Network())
	}
}
