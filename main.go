package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vmware/govmomi/vim25"
)

const listenAddress = ":2112"

var (
	vcUsername       string
	vcPassword       string
	vcUrl            string
	enableMocking    bool
	enableDebug      bool
	fetchInterval    int64
	excludeVmFolders string
)

func init() {
	flag.StringVar(&vcUsername, "username", os.Getenv("GOVC_USERNAME"), "Username for vCenter connection")
	flag.StringVar(&vcPassword, "password", os.Getenv("GOVC_PASSWORD"), "Password for vCenter connection")
	flag.StringVar(&vcUrl, "url", os.Getenv("GOVC_URL"), "URL for vCenter connection")
	flag.Int64Var(&fetchInterval, "interval", 30, "Interval in seconds in which data will be fetched")
	flag.BoolVar(&enableMocking, "mocking", os.Getenv("GOVC_MOCKING") == "1", "Enable vCenter mocking")
	flag.BoolVar(&enableDebug, "debug", os.Getenv("GOVC_DEBUG") == "1",
		"Print one-shot cluster calculation diagnostics and exit without exposing metrics")
	flag.StringVar(&excludeVmFolders, "exclude-vm-folders", os.Getenv("GOVC_EXCLUDE_VM_FOLDERS"),
		"Comma-separated folder names whose VMs, including those in subfolders, are additionally excluded from the cluster allocation metrics")
}

// splitCommaList splits a comma separated flag value into trimmed, non-empty
// entries. An empty or blank value yields nil.
func splitCommaList(value string) []string {
	var entries []string
	for _, entry := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(entry); trimmed != "" {
			entries = append(entries, trimmed)
		}
	}
	return entries
}

func main() {
	// Parsed here and not in init() so that the test binary can register its
	// own flags first.
	flag.Parse()
	if enableDebug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelDebug,
		})))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := newVCenterClient(ctx)
	if err != nil {
		slog.Error("creating vcenter client failed", "err", err)
		os.Exit(1)
	}

	excludedFolders := splitCommaList(excludeVmFolders)
	if len(excludedFolders) > 0 {
		slog.Info("excluding vms by folder", "folders", excludedFolders)
	}
	if enableDebug {
		if err := runDebugMode(ctx, client.Client, excludedFolders, slog.Default()); err != nil {
			slog.Error("debug collection failed", "err", err)
			os.Exit(1)
		}
		slog.Info("debug collection complete")
		return
	}

	if fetchInterval <= 0 {
		slog.Warn("invalid fetch interval, using default", "default_seconds", 30)
		fetchInterval = 30
	}

	datastoreMetricsLoop(ctx, client.Client, fetchInterval, newDatastoreMetrics(prometheus.DefaultRegisterer))
	clusterMetricsLoop(ctx, client.Client, fetchInterval, newClusterMetrics(prometheus.DefaultRegisterer), excludedFolders)

	http.Handle("/metrics", promhttp.Handler())
	slog.Info("listening", "address", listenAddress)
	if err := http.ListenAndServe(listenAddress, nil); err != nil {
		slog.Error("http server failed", "err", err)
		os.Exit(1)
	}
}

// runDebugMode executes the production cluster calculation once using an
// isolated registry. The registry is deliberately never attached to an HTTP
// handler, so debug mode cannot expose metrics.
func runDebugMode(ctx context.Context, client *vim25.Client, excludedFolders []string, logger *slog.Logger) error {
	metrics := newClusterMetrics(prometheus.NewRegistry())
	return collectClusterMetricsWithDebug(ctx, client, metrics, excludedFolders, logger)
}
