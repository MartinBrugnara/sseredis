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
)

func main() {
	flag.Parse()
	Bootstrap(*redisServer)

	http.HandleFunc("/", sseHandler)
	log.Fatal(http.ListenAndServe(":8010", nil))
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	ip := r.Header.Get("X-Forwarded-For")
	fmt.Println("Got connection", ip)

	f, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	// Set proper headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	// Authorize
	if sessions_id, err := r.Cookie("session_id"); err != nil {
		w.WriteHeader(422)
		fmt.Println("Invalid cookie", ip)
		return
	} else {
		if res, err := http.Get(
			fmt.Sprintf("%s?session_id=%s", djangoServer, sessions_id.Value)); err != nil {
			fmt.Printf("ERROR %s: %s\n", ip, err)
			w.WriteHeader(500)
			return
		} else if res.StatusCode != 200 {
			fmt.Println("Django not OK: ", res.StatusCode, ip)
			w.WriteHeader(res.StatusCode)
			return
		} // else auth
	}

	fmt.Println("Auth", ip)

	// Create Client
	c := &Client{out: make(chan []byte)}
	for _, v := range strings.Split(r.FormValue("subscribe"), ",") {
		c.subs[v] = true
	}

	// Serve
	c.Subscribe()
	defer c.Unsubscribe()

	for msg := range c.out {
		if _, ok := fmt.Fprintf(w, "data: %s\n\n", msg); ok != nil {
			break
		}
		f.Flush()
	}

	fmt.Println("Out", ip)
}
