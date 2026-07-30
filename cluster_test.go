package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
)

func TestCollectClusterMetrics(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		metrics := newClusterMetrics(prometheus.NewRegistry())

		if err := collectClusterMetrics(ctx, client, metrics); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}

		model := simulator.VPX()
		if got := gaugeValue(t, metrics.hostsTotal, simulatorClusterName); got != float64(model.ClusterHost) {
			t.Errorf("hosts_total = %v, want %v", got, model.ClusterHost)
		}

		stats := clusterHostStats(t, ctx, client)

		if got := gaugeValue(t, metrics.cpuCoresTotal, simulatorClusterName); got != float64(stats.TotalCores) {
			t.Errorf("cpu_cores_total = %v, want %v", got, stats.TotalCores)
		}

		vmCores := gaugeValue(t, metrics.vmCpuCores, simulatorClusterName)
		vmMemory := gaugeValue(t, metrics.vmMemory, simulatorClusterName)
		if vmCores <= 0 {
			t.Errorf("vm_cpu_cores_allocated = %v, want > 0", vmCores)
		}
		if vmMemory <= 0 {
			t.Errorf("vm_memory_bytes_allocated = %v, want > 0", vmMemory)
		}

		// The exported available values must be reproducible from the other
		// exported gauges, so dashboards can rely on the identity.
		coresTotal := gaugeValue(t, metrics.cpuCoresTotal, simulatorClusterName)
		wantCores := coresTotal - float64(stats.MaxCores) - vmCores
		if got := gaugeValue(t, metrics.cpuCoresAvail, simulatorClusterName); got != wantCores {
			t.Errorf("cpu_cores_available = %v, want %v (total %v - largest host %d - vms %v)",
				got, wantCores, coresTotal, stats.MaxCores, vmCores)
		}

		memoryTotal := gaugeValue(t, metrics.memoryTotal, simulatorClusterName)
		wantMemory := memoryTotal - float64(stats.MaxMemory) - vmMemory
		if got := gaugeValue(t, metrics.memoryAvail, simulatorClusterName); got != wantMemory {
			t.Errorf("memory_bytes_available = %v, want %v (total %v - largest host %d - vms %v)",
				got, wantMemory, memoryTotal, stats.MaxMemory, vmMemory)
		}
	})
}

func TestPoweredOffVmsAreCounted(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		before := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, before); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}
		coresBefore := gaugeValue(t, before.vmCpuCores, simulatorClusterName)
		memoryBefore := gaugeValue(t, before.vmMemory, simulatorClusterName)

		vms := clusterVmRefs(t, ctx, client)
		if len(vms) == 0 {
			t.Fatal("no vms in cluster")
		}
		task, err := object.NewVirtualMachine(client, vms[0]).PowerOff(ctx)
		if err != nil {
			t.Fatalf("power off: %v", err)
		}
		if err := task.Wait(ctx); err != nil {
			t.Fatalf("waiting for power off: %v", err)
		}

		after := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, after); err != nil {
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
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		reg := prometheus.NewRegistry()
		metrics := newClusterMetrics(reg)

		// Simulate a leftover series from a cluster that no longer exists.
		metrics.hostsTotal.WithLabelValues("ghost-cluster").Set(99)

		if err := collectClusterMetrics(ctx, client, metrics); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}

		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gathering metrics: %v", err)
		}
		for _, family := range families {
			if family.GetName() != "vcenter_cluster_hosts_total" {
				continue
			}
			for _, metric := range family.GetMetric() {
				for _, label := range metric.GetLabel() {
					if label.GetName() == "name" && label.GetValue() == "ghost-cluster" {
						t.Error("stale series for removed cluster still exported")
					}
				}
			}
		}
	})
}

func TestCollectClusterMetricsWithoutClusters(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		metrics := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, metrics); err != nil {
			t.Fatalf("collectClusterMetrics on ESX model: %v", err)
		}
	}, simulator.ESX())
}
