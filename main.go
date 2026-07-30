package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const listenAddress = ":2112"

var (
	vcUsername    string
	vcPassword    string
	vcUrl         string
	enableMocking bool
	fetchInterval int64
)

func init() {
	flag.StringVar(&vcUsername, "username", os.Getenv("GOVC_USERNAME"), "Username for vCenter connection")
	flag.StringVar(&vcPassword, "password", os.Getenv("GOVC_PASSWORD"), "Password for vCenter connection")
	flag.StringVar(&vcUrl, "url", os.Getenv("GOVC_URL"), "URL for vCenter connection")
	flag.Int64Var(&fetchInterval, "interval", 30, "Interval in seconds in which data will be fetched")
	flag.BoolVar(&enableMocking, "mocking", os.Getenv("GOVC_MOCKING") == "1", "Enable vCenter mocking")
}

func main() {
	// Parsed here and not in init() so that the test binary can register its
	// own flags first.
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := newVCenterClient(ctx)
	if err != nil {
		slog.Error("creating vcenter client failed", "err", err)
		os.Exit(1)
	}

	if fetchInterval <= 0 {
		slog.Warn("invalid fetch interval, using default", "default_seconds", 30)
		fetchInterval = 30
	}

	datastoreMetricsLoop(ctx, client.Client, fetchInterval, newDatastoreMetrics(prometheus.DefaultRegisterer))
	clusterMetricsLoop(ctx, client.Client, fetchInterval, newClusterMetrics(prometheus.DefaultRegisterer))

	http.Handle("/metrics", promhttp.Handler())
	slog.Info("listening", "address", listenAddress)
	if err := http.ListenAndServe(listenAddress, nil); err != nil {
		slog.Error("http server failed", "err", err)
		os.Exit(1)
	}
}
