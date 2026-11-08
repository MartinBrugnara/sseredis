package main

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// fakeSub records the Redis (un)subscriptions.
type fakeSub struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSub) record(op string, channels []interface{}) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range channels {
		f.calls = append(f.calls, fmt.Sprintf("%s %s", op, c))
	}
	return nil
}

func (f *fakeSub) Subscribe(channels ...interface{}) error   { return f.record("sub", channels) }
func (f *fakeSub) Unsubscribe(channels ...interface{}) error { return f.record("unsub", channels) }

func (f *fakeSub) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newTestHub(t *testing.T) (*Hub, *fakeSub) {
	t.Helper()
	sub := &fakeSub{}
	h := NewHub(sub)
	go h.Run()
	return h, sub
}

// drain returns the messages queued for c.
func drain(c *Client) []string {
	var got []string
	for {
		select {
		case m, ok := <-c.out:
			if !ok {
				return got
			}
			got = append(got, string(m))
		default:
			return got
		}
	}
}

func TestHubSubscriptionsAreRefCounted(t *testing.T) {
	h, sub := newTestHub(t)
	a := newClientFor("a", []string{"x", "y"})
	b := newClientFor("b", []string{"x"})

	h.Subscribe(a)
	h.Subscribe(b)
	h.Unsubscribe(a)
	h.Unsubscribe(b)

	want := []string{"sub x", "sub y", "unsub y", "unsub x"}
	// x and y are subscribed in map order.
	got := sub.Calls()
	if got[0] == "sub y" {
		got[0], got[1] = got[1], got[0]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("redis calls = %v, want %v", got, want)
	}
}

func TestHubPublishOnlyToSubscribers(t *testing.T) {
	h, _ := newTestHub(t)
	a := newClientFor("a", []string{"x"})
	b := newClientFor("b", []string{"y"})
	h.Subscribe(a)
	h.Subscribe(b)

	h.Publish("x", []byte("to x"))
	h.Heartbeat()

	if got, want := drain(a), []string{"to x", heartbeat}; !reflect.DeepEqual(got, want) {
		t.Errorf("a got %q, want %q", got, want)
	}
	if got, want := drain(b), []string{heartbeat}; !reflect.DeepEqual(got, want) {
		t.Errorf("b got %q, want %q", got, want)
	}
}

func TestHubDropsSlowClient(t *testing.T) {
	h, sub := newTestHub(t)
	slow := newClientFor("slow", []string{"x"})
	fast := newClientFor("fast", []string{"x"})
	h.Subscribe(slow)
	h.Subscribe(fast)

	// slow never reads; fast reads everything. Publish must not block.
	for i := 0; i <= clientBuffer; i++ {
		h.Publish("x", []byte("m"))
		drain(fast)
	}

	if n := len(drain(slow)); n != clientBuffer {
		t.Errorf("slow got %d messages before being dropped, want %d", n, clientBuffer)
	}
	if _, ok := <-slow.out; ok {
		t.Error("slow.out is still open")
	}
	if n := h.clientCount(); n != 1 {
		t.Errorf("clientCount = %d, want 1", n)
	}

	// The handler of the dropped client still calls Unsubscribe: no-op.
	h.Unsubscribe(slow)
	h.Publish("x", []byte("still works"))
	if got := drain(fast); !reflect.DeepEqual(got, []string{"still works"}) {
		t.Errorf("fast got %q after the drop", got)
	}
	for _, c := range sub.Calls() {
		if c == "unsub x" {
			t.Error("unsubscribed from x while fast is still subscribed")
		}
	}
}
