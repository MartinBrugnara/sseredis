package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

const timerTick = time.Second * 10

var (
	redisServer = flag.String("redis", "redis:6379", "redis server address")
	listenAddr  = flag.String("listen", ":80", "Address to bind")
	acao        = flag.String("origin", "*", "Access-Control-Allow-Origin")

	djangoServer = flag.String("dmbcau", "http://dmbcau/authenticator/", "dmbcau auth API endpoint")
	authTimeout  = flag.Duration("auth-timeout", 5*time.Second, "Timeout of a call to the auth API")

	// writeTimeout bounds each write to a client, so a stalled client
	// does not keep its handler around forever.
	writeTimeout = 30 * time.Second
)

func main() {
	// Setup logging
	logLv := flag.String("log", "INFO", "Log verbosity level")
	flag.Parse()
	if lv, err := parseLogLevel(*logLv); err != nil {
		Fatal("%s", err)
	} else {
		LOG_LEVEL = lv
	}
	Info("Log level: %s", LOG_LV_STR[LOG_LEVEL])

	hub := NewHub()
	go hub.Run()
	Info("Using Redis @ %s", *redisServer)
	go redisLoop(context.Background(), redisDialer(*redisServer), hub)
	go heartbeatLoop(hub, timerTick)

	// Listen for guests
	http.HandleFunc("/", sseHandler(hub, NewDjangoAuth(*djangoServer, *authTimeout)))
	Info("RDY, Listening @ %s", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}

func sseHandler(hub *Hub, auth Authenticator) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		Info("Got connection: %s", ip)

		if _, ok := w.(http.Flusher); !ok {
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

		channels := requestedChannels(r)
		if len(channels) == 0 {
			Info("No channels requested by %s", ip)
			http.Error(w, "No channels: use ?subscribe=chan1,chan2", http.StatusBadRequest)
			return
		}
		Info("Subscribe %s @ %s", ip, channels)
		client := newClientFor(ip, channels)
		if err := auth.Auth(r, client); err != nil {
			var authErr *AuthError
			if errors.As(err, &authErr) {
				Warn("Authentication failed for %s: %s", ip, err)
				http.Error(w, http.StatusText(authErr.Status), authErr.Status)
			} else {
				Error("Authentication error for %s: %s", ip, err)
				http.Error(w, "Authentication error", http.StatusInternalServerError)
			}
			return
		}
		Info("Authenticated %s", ip)
		hub.Subscribe(client)
		defer hub.Unsubscribe(client)

		Debug("Serving %s", ip)
		rc := http.NewResponseController(w)
		for {
			select {
			case <-r.Context().Done():
				Info("Disconnected %s", ip)
				return
			case msg, ok := <-client.out:
				if !ok {
					Info("Disconnected %s: dropped by the hub", ip)
					return
				}
				Debug("Send message %s", ip)
				if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
					Debug("Cannot set write deadline for %s: %s", ip, err)
				}
				if _, err := w.Write(msg); err != nil {
					Info("Disconnected %s: %s", ip, err)
					return
				}
				if err := rc.Flush(); err != nil {
					Info("Disconnected %s: %s", ip, err)
					return
				}
			}
		}
	}
}

// requestedChannels returns the channels in the subscribe parameter
// (comma separated), sorted, without empty names and duplicates.
func requestedChannels(r *http.Request) []string {
	var channels []string
	for _, ch := range strings.Split(r.FormValue("subscribe"), ",") {
		if ch != "" {
			channels = append(channels, ch)
		}
	}
	slices.Sort(channels)
	return slices.Compact(channels)
}
