package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
)

func TestCollectDatastoreMetrics(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		metrics := newDatastoreMetrics(prometheus.NewRegistry())

		viewManager := view.NewManager(client)
		datastoreView, err := viewManager.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"Datastore"}, true)
		if err != nil {
			t.Fatalf("creating datastore view: %v", err)
		}
		defer datastoreView.Destroy(ctx)

		if err := collectDatastoreMetrics(ctx, datastoreView, metrics); err != nil {
			t.Fatalf("collectDatastoreMetrics: %v", err)
		}

		var datastores []mo.Datastore
		if err := datastoreView.Retrieve(ctx, []string{"Datastore"}, []string{"summary"}, &datastores); err != nil {
			t.Fatalf("retrieving datastores: %v", err)
		}
		if len(datastores) == 0 {
			t.Fatal("no datastores in simulator")
		}

		ds := datastores[0]
		if got := gaugeValue(t, metrics.capacity, ds.Summary.Url, ds.Summary.Name); got != float64(ds.Summary.Capacity) {
			t.Errorf("datastore capacity = %v, want %v", got, ds.Summary.Capacity)
		}
	})
}
