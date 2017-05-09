package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/garyburd/redigo/redis"
)

var (
	djangoServer = flag.String("dmbcau", "http://dmbcau/authenticator/", "dmbcau auth API endpoint")
	redisServer  = flag.String("redis", "redis:6379", "redis server address")
	listenAddr   = flag.String("listen", ":80", "Address to bind")
	acao         = flag.String("origin", "*", "Access-Control-Allow-Origin")

	psc *redis.PubSubConn
)

func main() {
	// Setup logging
	logLv := flag.String("log", "INFO", "Log verbosity level")
	flag.Parse()
	LOG_LEVEL = LOG_STR_LV[*logLv]
	Info("Log level: %s", LOG_LV_STR[LOG_LEVEL])

	// Setup Redis
	if c, err := redis.Dial("tcp", *redisServer); err != nil {
		Fatal("Connecting to Redis. %s", err)
	} else {
		psc = &redis.PubSubConn{Conn: c}
	}
	Info("Connected to Redis @ %s", *redisServer)

	// Setup sync
	ops := make(chan func(Db))
	go loop(ops)
	go redisLoop(ops)
	go ping(ops)

	// Prepare to handle guests
	manager := make(ChannelManager)

	// Listen for guests
	http.HandleFunc("/", sseHandler(ops, manager))
	Info("RDY, Listening @ %s", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

func sseHandler(ops chan func(Db), manager ChannelManager) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("X-Forwarded-For")
		if len(ip) == 0 {
			ip = r.RemoteAddr
		}
		Info("Got connection: %s", ip)

		f, ok := w.(http.Flusher)
		if !ok {
			Warn("Streaming unsupported: %s", ip)
			http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
			return
		}
		Debug("Streaming to %s", ip)

		// Set proper headers
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Access-Control-Allow-Origin", *acao)
		Debug("Sent Headers to %s", ip)

		if err := auth(w, r); err != nil {
			return
		}
		client := newClient(w, r)
		manager.Subscribe(ops, client)
		defer manager.Unsubscribe(ops, client)

		Debug("Serving %s", ip)
		for msg := range client.out {
			Debug("Send message %s", ip)
			if _, err := w.Write(msg); err != nil {
				break
			}
			f.Flush()
		}

		Info("Disconnected %s", ip)
	}
}

func auth(w http.ResponseWriter, r *http.Request) error {
	ip := r.Header.Get("X-Forwarded-For")
	sessionsID, err := r.Cookie("sessionid")
	if err != nil {
		Warn("Not valid cookie for %s", ip)
		w.WriteHeader(http.StatusUnprocessableEntity)
		return errors.New("Not valid cookie")
	}

	query := fmt.Sprintf("%s?sessionid=%s", *djangoServer, sessionsID.Value)
	if res, err := http.Get(query); err != nil {
		Error("Django error [%s]: %s", err, ip)
		w.WriteHeader(http.StatusInternalServerError)
		return errors.New("Django error")
	} else if res.StatusCode != http.StatusOK {
		Warn("Django not OK [%d]: %s", res.StatusCode, ip)
		w.WriteHeader(res.StatusCode)
		return errors.New("Django not OK")
	} // else authenticate
	Info("Authenticated %s", ip)
	return nil
}

func newClient(w http.ResponseWriter, r *http.Request) *Client {
	ip := r.Header.Get("X-Forwarded-For")
	c := &Client{out: make(chan []byte), subs: make(map[string]bool)}
	Info("Subscribe %s @ %s", ip, strings.Split(r.FormValue("subscribe"), ","))
	for _, v := range strings.Split(r.FormValue("subscribe"), ",") {
		c.subs[v] = true
	}
	return c
}
