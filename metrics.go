package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics, served on -metrics (see "Monitoring" in README.md).
//
// The channel label takes only channels Django authorized (the Hub only
// knows those), which are a handful of names. Never label by client IP:
// it is personal data and has no bound.
var (
	registry = prometheus.NewRegistry()

	clientsConnected = newGauge("clients_connected",
		"SSE streams open now.")
	channelClients = newGaugeVec("channel_clients",
		"SSE streams open now, by channel (a stream with several channels counts in each).", "channel")
	connectionsTotal = newCounterVec("connections_total",
		"Connection attempts by result: ok, no_channels, refused (the authenticator said no), auth_error (it failed) or streaming_unsupported.", "result")
	disconnectionsTotal = newCounterVec("disconnections_total",
		"Streams closed, by how the handler noticed: client_gone (client or server hung up), write_error (includes write timeouts) or dropped (the Hub had dropped it, see clients_dropped_total).", "reason")
	// Counted by the Hub, where the drop happens: the handler of a stalled
	// client is usually stuck in a write, and notices only when the write
	// times out (a write_error disconnection).
	clientsDroppedTotal = newCounter("clients_dropped_total",
		"Clients dropped because their queue was full (stalled or too slow).")
	connectionDuration = newHistogram("connection_duration_seconds",
		"How long the streams stayed open.",
		[]float64{1, 5, 30, 60, 300, 900, 1800, 3600, 2 * 3600, 4 * 3600, 12 * 3600, 24 * 3600})

	authDuration = newHistogramVec("auth_duration_seconds",
		"Time to authenticate a connection, by result: ok, refused or error.",
		[]float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}, "result")

	redisConnected = newGauge("redis_connected",
		"1 while connected to Redis, 0 otherwise.")
	redisConnectsTotal = newCounterVec("redis_connects_total",
		"Attempts to connect to Redis, by result: ok or error.", "result")
	redisErrorsTotal = newCounter("redis_errors_total",
		"Errors on the Redis connection, including the ones that close it, and failed (un)subscribes.")
	redisChannels = newGauge("redis_channels_subscribed",
		"Redis channels the clients need (subscribed to while connected).")
	redisMessagesTotal = newCounterVec("redis_messages_received_total",
		"Messages received from Redis, by channel.", "channel")

	messagesSentTotal = newCounterVec("messages_sent_total",
		"Messages queued to clients (one per client), by channel.", "channel")
	messagesUnroutedTotal = newCounterVec("messages_unrouted_total",
		"Messages received from Redis that no client was subscribed to, by channel.", "channel")
	heartbeatsSentTotal = newCounter("heartbeats_sent_total",
		"Heartbeat comments queued to clients (one per client).")
	bytesSentTotal = newCounter("bytes_sent_total",
		"Bytes written to the SSE streams (body only, heartbeats included).")
)

func init() {
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
	// Export the labels known in advance at 0, so rate() and alerts work
	// before the first event.
	for _, r := range []string{"ok", "no_channels", "refused", "auth_error", "streaming_unsupported"} {
		connectionsTotal.WithLabelValues(r)
	}
	for _, r := range []string{"client_gone", "write_error", "dropped"} {
		disconnectionsTotal.WithLabelValues(r)
	}
	for _, r := range []string{"ok", "refused", "error"} {
		authDuration.WithLabelValues(r)
	}
	for _, r := range []string{"ok", "error"} {
		redisConnectsTotal.WithLabelValues(r)
	}
}

// metricsHandler serves the metrics at /metrics, and 404 elsewhere.
func metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	return mux
}

const namespace = "sseredis"

func newGauge(name, help string) prometheus.Gauge {
	g := prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help})
	registry.MustRegister(g)
	return g
}

func newGaugeVec(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Namespace: namespace, Name: name, Help: help}, labels)
	registry.MustRegister(g)
	return g
}

func newCounter(name, help string) prometheus.Counter {
	c := prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help})
	registry.MustRegister(c)
	return c
}

func newCounterVec(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: name, Help: help}, labels)
	registry.MustRegister(c)
	return c
}

func newHistogram(name, help string, buckets []float64) prometheus.Histogram {
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets})
	registry.MustRegister(h)
	return h
}

func newHistogramVec(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Namespace: namespace, Name: name, Help: help, Buckets: buckets}, labels)
	registry.MustRegister(h)
	return h
}
