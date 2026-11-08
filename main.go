package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
)

const timerTick = time.Second * 10

var (
	redisServer = flag.String("redis", "redis:6379", "redis server address: host:port or unix:/path/to/redis.sock")
	listenAddr  = flag.String("listen", ":80", "Address to bind: host:port or unix:/path/to/sock")
	listenMode  = flag.String("listen-mode", "0660", "Permissions (octal) of the -listen unix socket; ignored for TCP")
	metricsAddr = flag.String("metrics", "", "Address to serve Prometheus metrics on, at /metrics: host:port or unix:/path/to/sock; empty: no metrics. Keep it off the public -listen address")
	metricsMode = flag.String("metrics-mode", "0660", "Permissions (octal) of the -metrics unix socket; ignored for TCP")
	acao        = flag.String("origin", "", "Origin allowed to connect cross-origin with its cookies (Access-Control-Allow-Origin); empty for same-origin only")

	djangoServer = flag.String("dmbcau", "http://dmbcau/authenticator/", "dmbcau auth API endpoint")
	djangoSocket = flag.String("dmbcau-socket", "", "Unix socket to reach the auth API through (the -dmbcau URL still gives scheme, Host and path); empty: connect to the URL's host")
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
	l, err := listen(*listenAddr, *listenMode)
	if err != nil {
		Fatal("Listen @ %s: %s", *listenAddr, err)
	}
	auth := NewDjangoAuth(*djangoServer, *djangoSocket, *authTimeout)
	srv := &http.Server{Handler: http.HandlerFunc(sseHandler(hub, auth))}
	var metricsSrv *http.Server
	if *metricsAddr != "" {
		ml, err := listen(*metricsAddr, *metricsMode)
		if err != nil {
			Fatal("Listen for metrics @ %s: %s", *metricsAddr, err)
		}
		metricsSrv = &http.Server{Handler: metricsHandler(), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := metricsSrv.Serve(ml); !errors.Is(err, http.ErrServerClosed) {
				Fatal("Serve metrics: %s", err)
			}
		}()
		Info("Metrics @ %s/metrics", *metricsAddr)
	}
	// On SIGTERM (systemctl stop) or ^C close the servers: closing the
	// listeners removes the unix socket files. Close, not Shutdown: SSE
	// streams never become idle, so Shutdown would wait forever.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		Info("Shutting down")
		if metricsSrv != nil {
			metricsSrv.Close()
		}
		srv.Close()
	}()
	Info("RDY, Listening @ %s", *listenAddr)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		Fatal("Serve: %s", err)
	}
}

func sseHandler(hub *Hub, auth Authenticator) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		Info("Got connection: %s", ip)

		if _, ok := w.(http.Flusher); !ok {
			Warn("Streaming unsupported: %s", ip)
			connectionsTotal.WithLabelValues("streaming_unsupported").Inc()
			http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
			return
		}
		Debug("Streaming to %s", ip)

		channels := requestedChannels(r)
		if len(channels) == 0 {
			Info("No channels requested by %s", ip)
			connectionsTotal.WithLabelValues("no_channels").Inc()
			http.Error(w, "No channels: use ?subscribe=chan1,chan2", http.StatusBadRequest)
			return
		}
		Info("Subscribe %s @ %s", ip, channels)
		client := newClientFor(ip, channels)
		authStart := time.Now()
		err := auth.Auth(r, client)
		// A 5xx AuthError (e.g. Django answered 500) is the authenticator
		// failing, not refusing: count and log it as an error.
		var authErr *AuthError
		isAuthErr := errors.As(err, &authErr)
		switch {
		case err == nil:
			authDuration.WithLabelValues("ok").Observe(time.Since(authStart).Seconds())
		case isAuthErr && authErr.Status < 500:
			authDuration.WithLabelValues("refused").Observe(time.Since(authStart).Seconds())
			connectionsTotal.WithLabelValues("refused").Inc()
			Warn("Authentication failed for %s: %s", ip, err)
			http.Error(w, http.StatusText(authErr.Status), authErr.Status)
			return
		default:
			authDuration.WithLabelValues("error").Observe(time.Since(authStart).Seconds())
			connectionsTotal.WithLabelValues("auth_error").Inc()
			Error("Authentication error for %s: %s", ip, err)
			if isAuthErr {
				http.Error(w, http.StatusText(authErr.Status), authErr.Status)
			} else {
				http.Error(w, "Authentication error", http.StatusInternalServerError)
			}
			return
		}
		Info("Authenticated %s", ip)
		connectionsTotal.WithLabelValues("ok").Inc()
		hub.Subscribe(client)
		defer hub.Unsubscribe(client)

		// Counted once the stream is open; reason is set by each way out.
		start := time.Now()
		reason := "client_gone"
		defer func() {
			connectionDuration.Observe(time.Since(start).Seconds())
			disconnectionsTotal.WithLabelValues(reason).Inc()
		}()

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
			reason = "write_error"
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
					reason = "dropped"
					Info("Disconnected %s: dropped by the hub", ip)
					return
				}
				Debug("Send message %s", ip)
				if err := rc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
					Debug("Cannot set write deadline for %s: %s", ip, err)
				}
				n, err := w.Write(msg)
				bytesSentTotal.Add(float64(n))
				if err == nil {
					err = rc.Flush()
				}
				if err != nil {
					reason = "write_error"
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
