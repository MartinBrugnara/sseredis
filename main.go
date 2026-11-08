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
	acao        = flag.String("origin", "", "Origin allowed to connect cross-origin with its cookies (Access-Control-Allow-Origin); empty for same-origin only")

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

		// Send the headers now: the browser's EventSource only fires
		// "open" once it has them, and the first message may be far away.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
		setCORSHeaders(w.Header(), *acao)
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		if err := rc.Flush(); err != nil {
			Info("Disconnected %s: %s", ip, err)
			return
		}
		Debug("Serving %s", ip)
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

// setCORSHeaders lets the page at origin read the stream. Authentication
// needs the browser to send cookies, which it does cross-origin only with
// a named origin and Access-Control-Allow-Credentials (and
// EventSource(url, {withCredentials: true})). "*" allows any origin but
// without cookies, so it only makes sense with NoAuth. Empty: same-origin
// only, no header.
func setCORSHeaders(h http.Header, origin string) {
	switch origin {
	case "":
	case "*":
		h.Set("Access-Control-Allow-Origin", "*")
	default:
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
	}
}
