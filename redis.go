package main

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/gomodule/redigo/redis"
)

var (
	// Delay before reconnecting to Redis: it starts at minBackoff and
	// doubles at each failed attempt, up to maxBackoff. Redis is usually
	// unreachable because it is being upgraded or restarted.
	minBackoff = 1 * time.Second
	maxBackoff = 30 * time.Second
)

// nextBackoff returns the delay to use after one of d failed.
func nextBackoff(d time.Duration) time.Duration {
	if d < minBackoff {
		return minBackoff
	}
	return min(2*d, maxBackoff)
}

// redisLoop keeps a Redis connection, subscribed to the Hub's channels,
// and forwards the messages to the Hub. It reconnects with exponential
// backoff when the connection fails, until ctx is done.
func redisLoop(ctx context.Context, dial func() (redis.Conn, error), h *Hub) {
	var backoff time.Duration
	for {
		if backoff > 0 {
			Info("Reconnecting to Redis in %s", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}
		start := time.Now()
		err := redisSession(ctx, dial, h)
		if ctx.Err() != nil {
			return
		}
		Error("Redis: %s", err)
		// A connection that stayed up for a while starts over; one that
		// fails right away (Redis still starting) keeps backing off.
		if time.Since(start) > maxBackoff {
			backoff = minBackoff
		} else {
			backoff = nextBackoff(backoff)
		}
	}
}

// redisSession connects to Redis and forwards messages until the
// connection fails.
func redisSession(ctx context.Context, dial func() (redis.Conn, error), h *Hub) error {
	conn, err := dial()
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer conn.Close()
	// Unblock Receive when ctx is done.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	Info("Connected to Redis")

	psc := &redis.PubSubConn{Conn: conn}
	h.SetSubscriber(psc)
	defer h.SetSubscriber(nil)

	for {
		switch v := psc.Receive().(type) {
		case redis.Message:
			Debug("Message [%s] %s", v.Channel, v.Data)
			h.Publish(v.Channel, formatEvent(v.Data))
		case redis.Subscription:
			Debug("[%s] %s -> #%d", v.Channel, v.Kind, v.Count)
		case error:
			if conn.Err() != nil {
				return v
			}
			Error("Redis: %s", v)
		}
	}
}

// redisDialer returns a function that connects to the Redis at addr:
// host:port, or unix:/path/to/redis.sock.
func redisDialer(addr string) func() (redis.Conn, error) {
	network, address := splitNetAddr(addr)
	return func() (redis.Conn, error) {
		c, err := redis.Dial(network, address, redis.DialConnectTimeout(5*time.Second))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", addr, err)
		}
		return c, nil
	}
}

// formatEvent returns the SSE event carrying data. Each line of data gets
// its own "data:" field (the browser joins them back with "\n"), so a
// payload cannot end the event early or inject other fields (event:, id:,
// retry:). CR, LF and CRLF all end a line in SSE.
func formatEvent(data []byte) []byte {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	var b bytes.Buffer
	for _, line := range bytes.Split(data, []byte("\n")) {
		b.WriteString("data: ")
		b.Write(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}
