package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/garyburd/redigo/redis"
)

const (
	TIMER_TICK = time.Second * 10
	HEARTBEAT  = ":heartbeat signal\n\n"
)

type Client struct {
	out  chan []byte
	subs map[string]bool
}

var (
	psc *redis.PubSubConn

	// Subscription and connected clients
	stats_mux = &sync.Mutex{}
	stats     = make(map[string]int)
	clients   = make(map[*Client]bool)
)

func Bootstrap(address string) error {
	c, err := redis.Dial("tcp", address)
	if err != nil {
		Error("Connecting to Redis. %s", err)
		return err
	}
	psc = &redis.PubSubConn{Conn: c}
	Info("Connected to Redis @ %s", address)

	// Initialize structures
	stats_mux = &sync.Mutex{}

	go ProcessMessages()
	go Ping() // Use-full to clean up
	return nil
}

func Ping() {
	t := time.Tick(TIMER_TICK)
	for _ = range t {
		for c, _ := range clients {
			c.out <- []byte(HEARTBEAT)
		}
	}
}

func (c *Client) Subscribe() {
	clients[c] = true
	stats_mux.Lock()
	for s, _ := range c.subs {
		if _, ex := stats[s]; ex {
			stats[s] += 1
		} else {
			stats[s] = 1
			psc.Subscribe(s)
		}
	}
	stats_mux.Unlock()
}

func (c *Client) Unsubscribe() {
	delete(clients, c)
	stats_mux.Lock()
	for s, _ := range c.subs {
		if v, _ := stats[s]; v != 1 {
			stats[s] -= 1
		} else {
			delete(stats, s)
			psc.Unsubscribe(s)
		}
	}
	stats_mux.Unlock()
}

func ProcessMessages() {
	for {
		switch v := psc.Receive().(type) {
		case redis.Message:
			Debug("Message [%s] %s\n", v.Channel, v.Data)
			msg := fmt.Sprintf("data: %s\n\n", v.Data)
			for c, _ := range clients {
				if _, do := c.subs[v.Channel]; do {
					c.out <- []byte(msg)
				}
			}
		case redis.Subscription:
			Debug("[%s] %s -> #%d\n", v.Channel, v.Kind, v.Count)
		case error:
			Error("%s", v)
		}
	}
}
