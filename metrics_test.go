package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// The metrics are global and the other tests move them too, so these
// tests look at how much a value changed, or use channel names of their
// own.

func value(c prometheus.Collector) float64 { return testutil.ToFloat64(c) }

// observations returns how many values the histogram h got.
func observations(t *testing.T, h prometheus.Observer) uint64 {
	t.Helper()
	var m dto.Metric
	if err := h.(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetHistogram().GetSampleCount()
}

// eventually fails the test if cond does not become true in time.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMetricsHandler(t *testing.T) {
	srv := httptest.NewServer(metricsHandler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, want := range []string{
		"sseredis_clients_connected ",
		`sseredis_connections_total{result="auth_error"} `, // exported before it happens
		`sseredis_redis_connects_total{result="error"} `,
		"go_goroutines ",
		"go_build_info{",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q", want)
		}
	}

	resp, err = http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/ answered %d, want 404", resp.StatusCode)
	}
}

func TestHubMetrics(t *testing.T) {
	h, sub := newTestHub(t)
	connected := value(clientsConnected)
	sent := value(messagesSentTotal.WithLabelValues("m-a"))
	unrouted := value(messagesUnroutedTotal.WithLabelValues("m-none"))
	heartbeats := value(heartbeatsSentTotal)

	a1 := newClientFor("1", []string{"m-a"})
	a2 := newClientFor("2", []string{"m-a", "m-b"})
	h.Subscribe(a1)
	h.Subscribe(a2)
	if got := value(clientsConnected) - connected; got != 2 {
		t.Errorf("clients_connected went up by %v, want 2", got)
	}
	if got := value(channelClients.WithLabelValues("m-a")); got != 2 {
		t.Errorf("channel_clients{m-a} = %v, want 2", got)
	}
	if got := value(channelClients.WithLabelValues("m-b")); got != 1 {
		t.Errorf("channel_clients{m-b} = %v, want 1", got)
	}
	if got := value(redisChannels); got != 2 {
		t.Errorf("redis_channels_subscribed = %v, want 2", got)
	}

	h.Publish("m-a", []byte("x"))
	h.Publish("m-none", []byte("x"))
	h.Heartbeat()
	if got := value(messagesSentTotal.WithLabelValues("m-a")) - sent; got != 2 {
		t.Errorf("messages_sent_total{m-a} went up by %v, want 2", got)
	}
	if got := value(messagesUnroutedTotal.WithLabelValues("m-none")) - unrouted; got != 1 {
		t.Errorf("messages_unrouted_total{m-none} went up by %v, want 1", got)
	}
	if got := value(heartbeatsSentTotal) - heartbeats; got != 2 {
		t.Errorf("heartbeats_sent_total went up by %v, want 2", got)
	}

	h.Unsubscribe(a2)
	if got := value(channelClients.WithLabelValues("m-b")); got != 0 {
		t.Errorf("after unsubscribe channel_clients{m-b} = %v, want 0", got)
	}
	if got := value(redisChannels); got != 1 {
		t.Errorf("after unsubscribe redis_channels_subscribed = %v, want 1", got)
	}
	if got := value(clientsConnected) - connected; got != 1 {
		t.Errorf("after unsubscribe clients_connected is up by %v, want 1", got)
	}

	errs := value(redisErrorsTotal)
	sub.mu.Lock()
	sub.err = errors.New("broken")
	sub.mu.Unlock()
	h.Unsubscribe(a1)
	if got := value(redisErrorsTotal) - errs; got != 1 {
		t.Errorf("failed unsubscribe: redis_errors_total went up by %v, want 1", got)
	}
}

func TestHubMetricsDroppedClient(t *testing.T) {
	h, _ := newTestHub(t)
	dropped := value(clientsDroppedTotal)
	c := newClientFor("slow", []string{"m-slow"})
	h.Subscribe(c)
	for i := 0; i <= clientBuffer; i++ {
		h.Publish("m-slow", []byte("x"))
	}
	if got := value(clientsDroppedTotal) - dropped; got != 1 {
		t.Errorf("clients_dropped_total went up by %v, want 1", got)
	}
}

func TestServerConnectionMetrics(t *testing.T) {
	for _, tc := range []struct {
		auth       Authenticator
		url        string
		result     string
		authResult string // auth_duration_seconds label, "" if not authenticated
	}{
		{NoAuth{}, "/", "no_channels", ""},
		{refuseAll{http.StatusForbidden}, "/?subscribe=x", "refused", "refused"},
		{failAll{}, "/?subscribe=x", "auth_error", "error"},
	} {
		srv, _ := newTestServerWithAuth(t, tc.auth)
		before := value(connectionsTotal.WithLabelValues(tc.result))
		var authBefore uint64
		if tc.authResult != "" {
			authBefore = observations(t, authDuration.WithLabelValues(tc.authResult))
		}
		resp, err := http.Get(srv.URL + tc.url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := value(connectionsTotal.WithLabelValues(tc.result)) - before; got != 1 {
			t.Errorf("connections_total{%s} went up by %v, want 1", tc.result, got)
		}
		if tc.authResult != "" {
			if got := observations(t, authDuration.WithLabelValues(tc.authResult)) - authBefore; got != 1 {
				t.Errorf("auth_duration_seconds{%s} got %d observations, want 1", tc.authResult, got)
			}
		}
	}
}

func TestServerStreamMetrics(t *testing.T) {
	srv, h := newTestServer(t)
	ok := value(connectionsTotal.WithLabelValues("ok"))
	authOK := observations(t, authDuration.WithLabelValues("ok"))
	gone := value(disconnectionsTotal.WithLabelValues("client_gone"))
	durations := observations(t, connectionDuration)
	bytes := value(bytesSentTotal)

	ctx, hangUp := context.WithCancel(context.Background())
	c := connect(t, getRequest(t, srv.URL+"/?subscribe=m-stream").WithContext(ctx))
	waitClients(t, h, 1)
	msg := "data: hello\n\n"
	h.Publish("m-stream", []byte(msg))
	c.next(t)
	if got := value(connectionsTotal.WithLabelValues("ok")) - ok; got != 1 {
		t.Errorf("connections_total{ok} went up by %v, want 1", got)
	}
	if got := observations(t, authDuration.WithLabelValues("ok")) - authOK; got != 1 {
		t.Errorf("auth_duration_seconds{ok} got %d observations, want 1", got)
	}
	if got := value(bytesSentTotal) - bytes; got != float64(len(msg)) {
		t.Errorf("bytes_sent_total went up by %v, want %d", got, len(msg))
	}

	hangUp()
	waitClients(t, h, 0)
	eventually(t, "the disconnection to be counted", func() bool {
		return value(disconnectionsTotal.WithLabelValues("client_gone"))-gone == 1
	})
	if got := observations(t, connectionDuration) - durations; got != 1 {
		t.Errorf("connection_duration_seconds got %d observations, want 1", got)
	}
}

func TestServerDroppedDisconnection(t *testing.T) {
	srv, h := newTestServer(t)
	dropped := value(disconnectionsTotal.WithLabelValues("dropped"))
	connect(t, getRequest(t, srv.URL+"/?subscribe=m-drop"))
	waitClients(t, h, 1)

	// Drop the client from the Hub while its handler waits for messages.
	h.do(func() {
		for cl := range h.clients {
			h.remove(cl)
		}
	})
	eventually(t, "the dropped disconnection", func() bool {
		return value(disconnectionsTotal.WithLabelValues("dropped"))-dropped == 1
	})
}

func TestRedisMetrics(t *testing.T) {
	fastBackoff(t)
	m := miniredis.RunT(t)
	h := NewHub()
	go h.Run()
	c := newClientFor("c", []string{"m-redis"})
	h.Subscribe(c)

	connectOK := value(redisConnectsTotal.WithLabelValues("ok"))
	connectErr := value(redisConnectsTotal.WithLabelValues("error"))
	received := value(redisMessagesTotal.WithLabelValues("m-redis"))

	var failed bool
	dial := func() (redis.Conn, error) {
		if !failed {
			failed = true // only this goroutine dials
			return nil, errors.New("connection refused")
		}
		return redis.Dial("tcp", m.Addr())
	}
	startRedisLoop(t, dial, h)
	publishUntilReceived(t, m, "m-redis", "one")
	receive(t, c)

	if got := value(redisConnected); got != 1 {
		t.Errorf("redis_connected = %v, want 1", got)
	}
	if got := value(redisConnectsTotal.WithLabelValues("error")) - connectErr; got != 1 {
		t.Errorf("redis_connects_total{error} went up by %v, want 1", got)
	}
	if got := value(redisConnectsTotal.WithLabelValues("ok")) - connectOK; got != 1 {
		t.Errorf("redis_connects_total{ok} went up by %v, want 1", got)
	}
	if got := value(redisMessagesTotal.WithLabelValues("m-redis")) - received; got != 1 {
		t.Errorf("redis_messages_received_total{m-redis} went up by %v, want 1", got)
	}

	m.Close()
	eventually(t, "redis_connected to drop to 0", func() bool { return value(redisConnected) == 0 })
}
