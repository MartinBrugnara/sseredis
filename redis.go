package main

import (
	"fmt"
	"time"

	"github.com/garyburd/redigo/redis"
)

const (
	timerTick = time.Second * 10
	heartbeat = ":heartbeat signal\n\n"
)

// Db represents all the connected clients
type Db map[*Client]interface{}

// Client is a connected client
type Client struct {
	out  chan []byte
	subs map[string]bool
}

func ping(opsCh chan<- func(Db)) {
	t := time.Tick(timerTick)
	for _ = range t {
		Debug("ping")
		opsCh <- func(db Db) {
			for c := range db {
				c.out <- []byte(heartbeat)
			}
		}
	}
}

func redisLoop(opsCh chan<- func(Db)) {
	for {
		switch v := psc.Receive().(type) {
		case redis.Message:
			Debug("Message [%s] %s\n", v.Channel, v.Data)
			msg := fmt.Sprintf("data: %s\n\n", v.Data)
			opsCh <- func(msg string) func(Db) {
				return func(db Db) {
					for c := range db {
						if _, do := c.subs[v.Channel]; do {
							c.out <- []byte(msg)
						}
					}
				}
			}(msg)
		case redis.Subscription:
			Debug("[%s] %s -> #%d\n", v.Channel, v.Kind, v.Count)
		case error:
			Error("%s", v)
		}
	}
}

func loop(ops <-chan func(Db)) {
	// Client and list of channels
	db := make(map[*Client]interface{})

	for op := range ops {
		op(db)
	}
}

// ChannelManager atomically manages clients subscriptions
type ChannelManager map[string]uint

// Subscribe guess what
func (stats ChannelManager) Subscribe(ops chan<- func(Db), c *Client) {
	ops <- func(db Db) {
		db[c] = true
		for s := range c.subs {
			if _, ex := stats[s]; ex {
				stats[s]++
			} else {
				stats[s] = 1
				psc.Subscribe(s)
			}
		}
	}
}

// Unsubscribe guess what
func (stats ChannelManager) Unsubscribe(ops chan<- func(Db), c *Client) {
	ops <- func(db Db) {
		delete(db, c)
		for s := range c.subs {
			if v, _ := stats[s]; v != 1 {
				stats[s]--
			} else {
				delete(stats, s)
				psc.Unsubscribe(s)
			}
		}
	}
}
