package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strings"
)

var (
	djangoServer = flag.String("dmbcau", "http://dmbcau/authenticator/", "dmbcau auth API endpoint")
	redisServer  = flag.String("redis", "redis:6379", "redis server address")
	listenAddr   = flag.String("listen", ":80", "Address to bind")
)

func main() {

	log_lv := flag.String("log", "INFO", "Log verbosity level")
	flag.Parse()
	LOG_LEVEL = LOG_STR_LV[*log_lv]

	if err := Bootstrap(*redisServer); err != nil {
		return
	}
	Info("Bootstrap completed")

	http.HandleFunc("/", sseHandler)
	Info("Hadler registered")
	Info("Listening @ %s", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	ip := r.Header.Get("X-Forwarded-For")
	if len(ip) == 0 {
		ip = r.RemoteAddr
	}

	Info("Got connection: %s", ip)

	f, ok := w.(http.Flusher)
	if !ok {
		Warn("Straming unsupported: %s", ip)
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	Debug("Streaming to %s", ip)

	// Set proper headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	Debug("Sent Headers to %s", ip)

	// Authorize
	if sessions_id, err := r.Cookie("sessionid"); err != nil {
		Warn("Not valid cookie for %s", ip)
		w.WriteHeader(422)
		return
	} else {
		query := fmt.Sprintf("%s?sessionid=%s", *djangoServer, sessions_id.Value)
		if res, err := http.Get(query); err != nil {
			Error("Django error [%s]: %s", err, ip)
			w.WriteHeader(500)
			return
		} else if res.StatusCode != 200 {
			Warn("Django not OK [%d]: %s", res.StatusCode, ip)
			w.WriteHeader(res.StatusCode)
			return
		} // else auth
		Info("Authenticated %s", ip)
	}

	// Create Client
	c := &Client{out: make(chan []byte), subs: make(map[string]bool)}
	Info("Subscribe %s @ %s", ip, strings.Split(r.FormValue("subscribe"), ","))
	for _, v := range strings.Split(r.FormValue("subscribe"), ",") {
		c.subs[v] = true
	}

	// Serve
	c.Subscribe()
	defer c.Unsubscribe()

	Debug("Serving %s", ip)
	for msg := range c.out {
		Debug("Send message %s", ip)
		if _, err := w.Write(msg); err != nil {
			break
		}
		f.Flush()
	}

	Info("Disconnected %s", ip)
}
