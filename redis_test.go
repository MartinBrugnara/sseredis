package main

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gomodule/redigo/redis"
)

func TestNextBackoff(t *testing.T) {
	var got []time.Duration
	var d time.Duration
	for i := 0; i < 8; i++ {
		d = nextBackoff(d)
		got = append(got, d)
	}
	s := time.Second
	want := []time.Duration{1 * s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s, 30 * s}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("backoffs = %v, want %v", got, want)
	}
}

// fastBackoff shortens the reconnect delays for the test.
func fastBackoff(t *testing.T) {
	oldMin, oldMax := minBackoff, maxBackoff
	minBackoff, maxBackoff = 10*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { minBackoff, maxBackoff = oldMin, oldMax })
}

// startRedisLoop runs redisLoop until the end of the test.
func startRedisLoop(t *testing.T, dial func() (redis.Conn, error), h *Hub) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		redisLoop(ctx, dial, h)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done }) // runs before fastBackoff's cleanup
}

// publishUntilReceived publishes msg on channel until a subscriber gets it.
func publishUntilReceived(t *testing.T, m *miniredis.Miniredis, channel, msg string) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for m.Publish(channel, msg) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("nobody subscribed to %s", channel)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func receive(t *testing.T, c *Client) string {
	t.Helper()
	select {
	case m := <-c.out:
		return string(m)
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a message")
		return ""
	}
}

func TestRedisLoopDeliversAndReconnects(t *testing.T) {
	fastBackoff(t)
	m := miniredis.RunT(t)
	h := NewHub()
	go h.Run()

	// Subscribed before Redis is connected: SetSubscriber subscribes it.
	c := newClientFor("c", []string{"x"})
	h.Subscribe(c)
	startRedisLoop(t, redisDialer(m.Addr()), h)

	publishUntilReceived(t, m, "x", "one")
	if got := receive(t, c); got != "data: one\n\n" {
		t.Errorf("got %q", got)
	}

	// Redis restarts (upgrade): the subscriptions must come back.
	m.Close()
	if err := m.Restart(); err != nil {
		t.Fatal(err)
	}
	publishUntilReceived(t, m, "x", "two")
	if got := receive(t, c); got != "data: two\n\n" {
		t.Errorf("after restart got %q", got)
	}

	// Channels subscribed after the reconnect work too.
	d := newClientFor("d", []string{"y"})
	h.Subscribe(d)
	publishUntilReceived(t, m, "y", "three")
	if got := receive(t, d); got != "data: three\n\n" {
		t.Errorf("new channel got %q", got)
	}
}

func TestRedisLoopRetriesUntilRedisIsUp(t *testing.T) {
	fastBackoff(t)
	m := miniredis.RunT(t)
	h := NewHub()
	go h.Run()
	c := newClientFor("c", []string{"x"})
	h.Subscribe(c)

	var attempts atomic.Int32
	dial := func() (redis.Conn, error) {
		if attempts.Add(1) <= 3 {
			return nil, errors.New("connection refused")
		}
		return redis.Dial("tcp", m.Addr())
	}
	startRedisLoop(t, dial, h)

	publishUntilReceived(t, m, "x", "up")
	if got := receive(t, c); got != "data: up\n\n" {
		t.Errorf("got %q", got)
	}
	if n := attempts.Load(); n != 4 {
		t.Errorf("dialled %d times, want 4", n)
	}
}
