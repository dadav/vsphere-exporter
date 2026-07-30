package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// simulatorClusterName is the name of the cluster created by simulator.VPX().
const simulatorClusterName = "DC0_C0"

func gaugeValue(t *testing.T, gauge *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	var metric dto.Metric
	if err := gauge.WithLabelValues(labels...).Write(&metric); err != nil {
		t.Fatalf("reading gauge: %v", err)
	}
	return metric.GetGauge().GetValue()
}

// hostStats are the host hardware totals of a cluster, computed independently
// of the exporter code.
type hostStats struct {
	TotalCores  int64
	MaxCores    int64
	TotalMemory int64
	MaxMemory   int64
}

func clusterHostStats(t *testing.T, ctx context.Context, client *vim25.Client) hostStats {
	t.Helper()

	viewManager := view.NewManager(client)
	clusterView, err := viewManager.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		t.Fatalf("creating cluster view: %v", err)
	}
	defer clusterView.Destroy(ctx)

	var clusters []mo.ClusterComputeResource
	if err := clusterView.Retrieve(ctx, []string{"ClusterComputeResource"}, []string{"name", "host"}, &clusters); err != nil {
		t.Fatalf("retrieving clusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}

	hostView, err := viewManager.CreateContainerView(ctx, clusters[0].Reference(), []string{"HostSystem"}, true)
	if err != nil {
		t.Fatalf("creating host view: %v", err)
	}
	defer hostView.Destroy(ctx)

	var hosts []mo.HostSystem
	if err := hostView.Retrieve(ctx, []string{"HostSystem"}, []string{"summary.hardware"}, &hosts); err != nil {
		t.Fatalf("retrieving hosts: %v", err)
	}

	var stats hostStats
	for _, host := range hosts {
		if host.Summary.Hardware == nil {
			continue
		}
		cores := int64(host.Summary.Hardware.NumCpuCores)
		memory := host.Summary.Hardware.MemorySize

		stats.TotalCores += cores
		stats.TotalMemory += memory
		if cores > stats.MaxCores {
			stats.MaxCores = cores
		}
		if memory > stats.MaxMemory {
			stats.MaxMemory = memory
		}
	}
	return stats
}

// clusterVmRefs returns the VM references inside the simulator cluster.
func clusterVmRefs(t *testing.T, ctx context.Context, client *vim25.Client) []types.ManagedObjectReference {
	t.Helper()

	viewManager := view.NewManager(client)
	clusterView, err := viewManager.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		t.Fatalf("creating cluster view: %v", err)
	}
	defer clusterView.Destroy(ctx)

	clusters, err := clusterView.Find(ctx, []string{"ClusterComputeResource"}, nil)
	if err != nil {
		t.Fatalf("finding clusters: %v", err)
	}
	if len(clusters) == 0 {
		t.Fatal("no cluster found")
	}

	vmView, err := viewManager.CreateContainerView(ctx, clusters[0], []string{"VirtualMachine"}, true)
	if err != nil {
		t.Fatalf("creating vm view: %v", err)
	}
	defer vmView.Destroy(ctx)

	vms, err := vmView.Find(ctx, []string{"VirtualMachine"}, nil)
	if err != nil {
		t.Fatalf("finding vms: %v", err)
	}
	return vms
}
