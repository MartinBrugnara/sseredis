package main

import (
	"fmt"
	"sync"

	"github.com/garyburd/redigo/redis"
)

type Client struct {
	out  chan []byte
	subs map[string]bool
}

var (
	psc *redis.PubSubConn

	// Subscription and connected clients
	stats_mux *sync.Mutex
	stats     map[string]int
	clients   map[*Client]bool
)

func Bootstrap(address string) {
	// Connect to Redis
	c, err := redis.Dial("tcp", address)
	if err != nil {
		panic(err)
	}
	psc = &redis.PubSubConn{Conn: c}

	// Initialize structures
	stats_mux = &sync.Mutex{}

	go ProcessMessages()
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
			// DEBUG
			fmt.Printf("%s: message: %s\n", v.Channel, v.Data)

			payload := fmt.Sprintf("data: %s\n\n", v.Data)
			for c, _ := range clients {
				if _, do := c.subs[v.Channel]; do {
					c.out <- []byte(payload)
				}
			}
		case redis.Subscription:
			// DEBUG
			fmt.Printf("%s: %s %d\n", v.Channel, v.Kind, v.Count)
		case error:
			fmt.Printf("ERROR: %s", v)
		}
	}
}
