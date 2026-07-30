package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// simulatorClusterName is the name of the cluster created by simulator.VPX().
const simulatorClusterName = "DC0_C0"

func gaugeValue(t *testing.T, g *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.WithLabelValues(labels...).Write(&m); err != nil {
		t.Fatalf("reading gauge: %v", err)
	}
	return m.GetGauge().GetValue()
}

// clusterHostStats returns the total cores/memory and the largest single host
// cores/memory of the simulator cluster, computed independently of the
// exporter code.
func clusterHostStats(t *testing.T, ctx context.Context, c *vim25.Client) (totalCores, maxCores, totalMemory, maxMemory int64) {
	t.Helper()

	m := view.NewManager(c)
	cv, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		t.Fatalf("creating cluster view: %v", err)
	}
	defer cv.Destroy(ctx)

	var clusters []mo.ClusterComputeResource
	if err := cv.Retrieve(ctx, []string{"ClusterComputeResource"}, []string{"name", "host"}, &clusters); err != nil {
		t.Fatalf("retrieving clusters: %v", err)
	}
	if len(clusters) != 1 {
		t.Fatalf("expected 1 cluster, got %d", len(clusters))
	}

	var hosts []mo.HostSystem
	hv, err := m.CreateContainerView(ctx, clusters[0].Reference(), []string{"HostSystem"}, true)
	if err != nil {
		t.Fatalf("creating host view: %v", err)
	}
	defer hv.Destroy(ctx)
	if err := hv.Retrieve(ctx, []string{"HostSystem"}, []string{"summary.hardware"}, &hosts); err != nil {
		t.Fatalf("retrieving hosts: %v", err)
	}

	for _, h := range hosts {
		if h.Summary.Hardware == nil {
			continue
		}
		cores := int64(h.Summary.Hardware.NumCpuCores)
		memory := h.Summary.Hardware.MemorySize
		totalCores += cores
		totalMemory += memory
		if cores > maxCores {
			maxCores = cores
		}
		if memory > maxMemory {
			maxMemory = memory
		}
	}
	return totalCores, maxCores, totalMemory, maxMemory
}

// clusterVmRefs returns the VM references inside the simulator cluster.
func clusterVmRefs(t *testing.T, ctx context.Context, c *vim25.Client) []types.ManagedObjectReference {
	t.Helper()

	m := view.NewManager(c)
	cv, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		t.Fatalf("creating cluster view: %v", err)
	}
	defer cv.Destroy(ctx)

	clusters, err := cv.Find(ctx, []string{"ClusterComputeResource"}, nil)
	if err != nil {
		t.Fatalf("finding clusters: %v", err)
	}
	if len(clusters) == 0 {
		t.Fatal("no cluster found")
	}

	vv, err := m.CreateContainerView(ctx, clusters[0], []string{"VirtualMachine"}, true)
	if err != nil {
		t.Fatalf("creating vm view: %v", err)
	}
	defer vv.Destroy(ctx)

	vms, err := vv.Find(ctx, []string{"VirtualMachine"}, nil)
	if err != nil {
		t.Fatalf("finding vms: %v", err)
	}
	return vms
}

func TestCollectClusterMetrics(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		metrics := newClusterMetrics(prometheus.NewRegistry())

		if err := collectClusterMetrics(ctx, c, metrics); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}

		model := simulator.VPX()
		if got := gaugeValue(t, metrics.hostsTotal, simulatorClusterName); got != float64(model.ClusterHost) {
			t.Errorf("hosts_total = %v, want %v", got, model.ClusterHost)
		}

		totalCores, maxCores, totalMemory, maxMemory := clusterHostStats(t, ctx, c)

		if got := gaugeValue(t, metrics.cpuCoresTotal, simulatorClusterName); got != float64(totalCores) {
			t.Errorf("cpu_cores_total = %v, want %v", got, totalCores)
		}
		if got := gaugeValue(t, metrics.memoryTotal, simulatorClusterName); got != float64(totalMemory) {
			t.Errorf("memory_bytes_total = %v, want %v", got, totalMemory)
		}

		vmCores := gaugeValue(t, metrics.vmCpuCores, simulatorClusterName)
		vmMemory := gaugeValue(t, metrics.vmMemory, simulatorClusterName)
		if vmCores <= 0 {
			t.Errorf("vm_cpu_cores_allocated = %v, want > 0", vmCores)
		}
		if vmMemory <= 0 {
			t.Errorf("vm_memory_bytes_allocated = %v, want > 0", vmMemory)
		}

		wantCores := float64(totalCores-maxCores) - vmCores
		if got := gaugeValue(t, metrics.cpuCoresAvail, simulatorClusterName); got != wantCores {
			t.Errorf("cpu_cores_available = %v, want %v (total %d - largest host %d - vms %v)",
				got, wantCores, totalCores, maxCores, vmCores)
		}

		wantMemory := float64(totalMemory-maxMemory) - vmMemory
		if got := gaugeValue(t, metrics.memoryAvail, simulatorClusterName); got != wantMemory {
			t.Errorf("memory_bytes_available = %v, want %v (total %d - largest host %d - vms %v)",
				got, wantMemory, totalMemory, maxMemory, vmMemory)
		}
	})
}

func TestPoweredOffVmsAreCounted(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		before := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, c, before); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}
		coresBefore := gaugeValue(t, before.vmCpuCores, simulatorClusterName)
		memoryBefore := gaugeValue(t, before.vmMemory, simulatorClusterName)

		vms := clusterVmRefs(t, ctx, c)
		if len(vms) == 0 {
			t.Fatal("no vms in cluster")
		}
		task, err := object.NewVirtualMachine(c, vms[0]).PowerOff(ctx)
		if err != nil {
			t.Fatalf("power off: %v", err)
		}
		if err := task.Wait(ctx); err != nil {
			t.Fatalf("waiting for power off: %v", err)
		}

		after := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, c, after); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}

		if got := gaugeValue(t, after.vmCpuCores, simulatorClusterName); got != coresBefore {
			t.Errorf("vm_cpu_cores_allocated after power off = %v, want unchanged %v", got, coresBefore)
		}
		if got := gaugeValue(t, after.vmMemory, simulatorClusterName); got != memoryBefore {
			t.Errorf("vm_memory_bytes_allocated after power off = %v, want unchanged %v", got, memoryBefore)
		}
	})
}

func TestStaleClusterSeriesAreDropped(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		reg := prometheus.NewRegistry()
		metrics := newClusterMetrics(reg)

		// Simulate a leftover series from a cluster that no longer exists.
		metrics.hostsTotal.WithLabelValues("ghost-cluster").Set(99)

		if err := collectClusterMetrics(ctx, c, metrics); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}

		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gathering metrics: %v", err)
		}
		for _, mf := range families {
			if mf.GetName() != "vcenter_cluster_hosts_total" {
				continue
			}
			for _, m := range mf.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "name" && l.GetValue() == "ghost-cluster" {
						t.Error("stale series for removed cluster still exported")
					}
				}
			}
		}
	})
}

func TestCollectClusterMetricsWithoutClusters(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		metrics := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, c, metrics); err != nil {
			t.Fatalf("collectClusterMetrics on ESX model: %v", err)
		}
	}, simulator.ESX())
}

func TestCollectDatastoreMetrics(t *testing.T) {
	simulator.Test(func(ctx context.Context, c *vim25.Client) {
		metrics := newDatastoreMetrics(prometheus.NewRegistry())

		m := view.NewManager(c)
		v, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"Datastore"}, true)
		if err != nil {
			t.Fatalf("creating datastore view: %v", err)
		}
		defer v.Destroy(ctx)

		if err := collectDatastoreMetrics(ctx, v, metrics); err != nil {
			t.Fatalf("collectDatastoreMetrics: %v", err)
		}

		var datastores []mo.Datastore
		if err := v.Retrieve(ctx, []string{"Datastore"}, []string{"summary"}, &datastores); err != nil {
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
