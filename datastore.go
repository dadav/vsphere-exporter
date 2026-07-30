package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
)

type datastoreMetrics struct {
	capacity       *prometheus.GaugeVec
	free           *prometheus.GaugeVec
	scrapeFailures prometheus.Counter
}

func newDatastoreMetrics(reg prometheus.Registerer) *datastoreMetrics {
	auto := promauto.With(reg)
	return &datastoreMetrics{
		capacity: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_datastore_capacity_bytes",
			Help: "Total capacity of the datastore in bytes",
		}, []string{"url", "name"}),
		free: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_datastore_free_bytes",
			Help: "Total free space of the datastore in bytes",
		}, []string{"url", "name"}),
		scrapeFailures: auto.NewCounter(prometheus.CounterOpts{
			Name: "vcenter_datastore_scrape_failures_total",
			Help: "Number of datastore scrape failures",
		}),
	}
}

func collectDatastoreMetrics(ctx context.Context, datastoreView *view.ContainerView, metrics *datastoreMetrics) error {
	var datastores []mo.Datastore
	if err := datastoreView.Retrieve(ctx, []string{"Datastore"}, []string{"summary"}, &datastores); err != nil {
		return err
	}

	for _, ds := range datastores {
		metrics.capacity.WithLabelValues(ds.Summary.Url, ds.Summary.Name).Set(float64(ds.Summary.Capacity))
		metrics.free.WithLabelValues(ds.Summary.Url, ds.Summary.Name).Set(float64(ds.Summary.FreeSpace))
	}
	return nil
}

func datastoreMetricsLoop(ctx context.Context, client *vim25.Client, interval int64, metrics *datastoreMetrics) {
	go func() {
		viewManager := view.NewManager(client)

		datastoreView, err := viewManager.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"Datastore"}, true)
		if err != nil {
			slog.Error("creating datastore container view failed", "err", err)
			os.Exit(1)
		}
		defer datastoreView.Destroy(ctx)

		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()

		metrics.scrapeFailures.Add(0)

		if err := collectDatastoreMetrics(ctx, datastoreView, metrics); err != nil {
			metrics.scrapeFailures.Inc()
			slog.Error("datastore scrape failed", "err", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := collectDatastoreMetrics(ctx, datastoreView, metrics); err != nil {
					metrics.scrapeFailures.Inc()
					slog.Error("datastore scrape failed", "err", err)
				}
			}
		}
	}()
}
