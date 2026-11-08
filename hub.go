package main

import (
	"time"
)

const (
	// clientBuffer is how many messages may wait for a client. A client
	// that falls further behind is dropped (see Hub.send).
	clientBuffer = 64

	heartbeat = ":heartbeat signal\n\n"
)

// subscriber is the part of redis.PubSubConn the Hub needs.
type subscriber interface {
	Subscribe(channels ...interface{}) error
	Unsubscribe(channels ...interface{}) error
}

// Client is a connected SSE client.
type Client struct {
	ip   string
	subs map[string]bool

	// out carries the messages to write to the client. Only the Hub
	// goroutine sends on it, and it closes it when it drops the client.
	out chan []byte
}

func newClientFor(ip string, channels []string) *Client {
	c := &Client{ip: ip, out: make(chan []byte, clientBuffer), subs: make(map[string]bool)}
	for _, ch := range channels {
		c.subs[ch] = true
	}
	return c
}

// Hub keeps the connected clients and the Redis subscriptions.
//
// All its state is owned by the goroutine running Run; the other methods
// hand it functions over ops. The Hub goroutine never blocks on a client
// (see send), so one slow or dead client cannot stall the others.
type Hub struct {
	ops      chan func()
	clients  map[*Client]struct{}
	channels map[string]int // channel -> number of clients subscribed to it
	sub      subscriber
}

// NewHub returns a Hub that subscribes to Redis channels through sub.
func NewHub(sub subscriber) *Hub {
	return &Hub{
		ops:      make(chan func()),
		clients:  make(map[*Client]struct{}),
		channels: make(map[string]int),
		sub:      sub,
	}
}

// Run executes the Hub operations. It never returns.
func (h *Hub) Run() {
	for op := range h.ops {
		op()
	}
}

// do runs op on the Hub goroutine and waits for it to finish.
func (h *Hub) do(op func()) {
	done := make(chan struct{})
	h.ops <- func() {
		op()
		close(done)
	}
	<-done
}

// Subscribe adds c and subscribes to the Redis channels nobody was
// subscribed to yet.
func (h *Hub) Subscribe(c *Client) {
	h.do(func() {
		h.clients[c] = struct{}{}
		for s := range c.subs {
			h.channels[s]++
			if h.channels[s] == 1 {
				h.sub.Subscribe(s)
			}
		}
	})
}

// Unsubscribe removes c, if the Hub did not already drop it.
func (h *Hub) Unsubscribe(c *Client) {
	h.do(func() { h.remove(c) })
}

// Publish sends msg to the clients subscribed to channel.
func (h *Hub) Publish(channel string, msg []byte) {
	h.do(func() {
		for c := range h.clients {
			if c.subs[channel] {
				h.send(c, msg)
			}
		}
	})
}

// Heartbeat sends a comment to every client, so proxies keep the
// connections open and dead ones are noticed.
func (h *Hub) Heartbeat() {
	h.do(func() {
		for c := range h.clients {
			h.send(c, []byte(heartbeat))
		}
	})
}

// send queues msg for c without blocking. If c's queue is full the client
// is not keeping up (or its handler is gone): drop it.
func (h *Hub) send(c *Client, msg []byte) {
	select {
	case c.out <- msg:
	default:
		Warn("Dropping slow client %s", c.ip)
		h.remove(c)
	}
}

// remove forgets c, unsubscribes from the channels nobody needs anymore and
// closes c.out so its handler returns. Must run on the Hub goroutine.
func (h *Hub) remove(c *Client) {
	if _, ok := h.clients[c]; !ok {
		return
	}
	delete(h.clients, c)
	close(c.out)
	for s := range c.subs {
		h.channels[s]--
		if h.channels[s] == 0 {
			delete(h.channels, s)
			h.sub.Unsubscribe(s)
		}
	}
}

// clientCount returns the number of connected clients (used by tests).
func (h *Hub) clientCount() int {
	var n int
	h.do(func() { n = len(h.clients) })
	return n
}

// heartbeatLoop calls h.Heartbeat every interval.
func heartbeatLoop(h *Hub, interval time.Duration) {
	for range time.Tick(interval) {
		Debug("ping")
		h.Heartbeat()
	}
}
