package main

import (
	"fmt"

	"github.com/gomodule/redigo/redis"
)

// redisLoop forwards the messages received on psc to the Hub.
func redisLoop(psc *redis.PubSubConn, h *Hub) {
	for {
		switch v := psc.Receive().(type) {
		case redis.Message:
			Debug("Message [%s] %s", v.Channel, v.Data)
			h.Publish(v.Channel, []byte(fmt.Sprintf("data: %s\n\n", v.Data)))
		case redis.Subscription:
			Debug("[%s] %s -> #%d", v.Channel, v.Kind, v.Count)
		case error:
			Error("%s", v)
		}
	}
}
