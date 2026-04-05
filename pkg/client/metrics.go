package client

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	MetricsRegistry = prometheus.NewRegistry()
	factory         = promauto.With(MetricsRegistry)

	activeStreamsMetric = factory.NewGauge(prometheus.GaugeOpts{
		Name: "portless_client_active_streams",
		Help: "Current number of active proxy streams.",
	})
	requestsTotalMetric = factory.NewCounterVec(prometheus.CounterOpts{
		Name: "portless_client_requests_total",
		Help: "Total proxied requests by status code.",
	}, []string{"status_code"})
	connectedMetric = factory.NewGauge(prometheus.GaugeOpts{
		Name: "portless_client_connected",
		Help: "Whether the client is currently connected to the server.",
	})
)

func observeProxyResponse(status int) {
	requestsTotalMetric.WithLabelValues(strconv.Itoa(status)).Inc()
}

func setConnectedMetric(connected bool) {
	if connected {
		connectedMetric.Set(1)
		return
	}
	connectedMetric.Set(0)
}
