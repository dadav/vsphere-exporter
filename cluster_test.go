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

		if err := collectClusterMetrics(ctx, client, metrics, nil); err != nil {
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
		if err := collectClusterMetrics(ctx, client, before, nil); err != nil {
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
		if err := collectClusterMetrics(ctx, client, after, nil); err != nil {
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

		if err := collectClusterMetrics(ctx, client, metrics, nil); err != nil {
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

// assertVmExcluded collects with the given exclusion list and checks that
// exactly the given VM is missing from the allocation gauges and that the
// available identity still holds against the reduced allocation.
func assertVmExcluded(t *testing.T, ctx context.Context, client *vim25.Client, excludedNames []string, vm countedVm, coresBefore, memoryBefore float64) {
	t.Helper()

	excluded := newClusterMetrics(prometheus.NewRegistry())
	if err := collectClusterMetrics(ctx, client, excluded, excludedNames); err != nil {
		t.Fatalf("collectClusterMetrics with exclusion: %v", err)
	}

	wantCores := coresBefore - vm.Cores
	if got := gaugeValue(t, excluded.vmCpuCores, simulatorClusterName); got != wantCores {
		t.Errorf("vm_cpu_cores_allocated with exclusion = %v, want %v (%v - excluded vm %v)",
			got, wantCores, coresBefore, vm.Cores)
	}
	wantMemory := memoryBefore - vm.Memory
	if got := gaugeValue(t, excluded.vmMemory, simulatorClusterName); got != wantMemory {
		t.Errorf("vm_memory_bytes_allocated with exclusion = %v, want %v (%v - excluded vm %v)",
			got, wantMemory, memoryBefore, vm.Memory)
	}

	stats := clusterHostStats(t, ctx, client)
	coresTotal := gaugeValue(t, excluded.cpuCoresTotal, simulatorClusterName)
	wantCoresAvail := coresTotal - float64(stats.MaxCores) - wantCores
	if got := gaugeValue(t, excluded.cpuCoresAvail, simulatorClusterName); got != wantCoresAvail {
		t.Errorf("cpu_cores_available with exclusion = %v, want %v", got, wantCoresAvail)
	}
	memoryTotal := gaugeValue(t, excluded.memoryTotal, simulatorClusterName)
	wantMemoryAvail := memoryTotal - float64(stats.MaxMemory) - wantMemory
	if got := gaugeValue(t, excluded.memoryAvail, simulatorClusterName); got != wantMemoryAvail {
		t.Errorf("memory_bytes_available with exclusion = %v, want %v", got, wantMemoryAvail)
	}
}

// TestExcludedFolderVmsNotCounted covers the SRM placeholder case: VMs sitting
// in a dedicated folder are recovery stubs that consume no resources, so they
// must not show up in the allocation and available gauges.
func TestExcludedFolderVmsNotCounted(t *testing.T) {
	const excludedFolder = "IT-Notfall"

	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		baseline := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, baseline, nil); err != nil {
			t.Fatalf("collectClusterMetrics baseline: %v", err)
		}
		coresBefore := gaugeValue(t, baseline.vmCpuCores, simulatorClusterName)
		memoryBefore := gaugeValue(t, baseline.vmMemory, simulatorClusterName)

		vm := firstCountedVm(t, ctx, client)
		folder := createChildFolder(t, ctx, client, vm.Parent, excludedFolder)
		moveVmInto(t, ctx, folder, vm)

		assertVmExcluded(t, ctx, client, []string{excludedFolder}, vm, coresBefore, memoryBefore)

		// Without the flag the very same VM counts again, folder placement
		// alone must not change anything.
		unfiltered := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, unfiltered, nil); err != nil {
			t.Fatalf("collectClusterMetrics without exclusion: %v", err)
		}
		if got := gaugeValue(t, unfiltered.vmCpuCores, simulatorClusterName); got != coresBefore {
			t.Errorf("vm_cpu_cores_allocated without exclusion = %v, want unchanged %v", got, coresBefore)
		}
		if got := gaugeValue(t, unfiltered.vmMemory, simulatorClusterName); got != memoryBefore {
			t.Errorf("vm_memory_bytes_allocated without exclusion = %v, want unchanged %v", got, memoryBefore)
		}
	})
}

// TestExcludedSubfolderVmsNotCounted checks that the exclusion is recursive:
// a VM in a subfolder below the excluded folder is excluded as well, even
// though its direct parent has a different name.
func TestExcludedSubfolderVmsNotCounted(t *testing.T) {
	const excludedFolder = "IT-Notfall"

	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		baseline := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, baseline, nil); err != nil {
			t.Fatalf("collectClusterMetrics baseline: %v", err)
		}
		coresBefore := gaugeValue(t, baseline.vmCpuCores, simulatorClusterName)
		memoryBefore := gaugeValue(t, baseline.vmMemory, simulatorClusterName)

		vm := firstCountedVm(t, ctx, client)
		parent := createChildFolder(t, ctx, client, vm.Parent, excludedFolder)
		nested := createChildFolder(t, ctx, client, parent.Reference(), "nested")
		moveVmInto(t, ctx, nested, vm)

		assertVmExcluded(t, ctx, client, []string{excludedFolder}, vm, coresBefore, memoryBefore)
	})
}

// TestExcludedFolderNameNotFound makes sure a configured folder that does not
// exist is a warning, not a scrape failure.
func TestExcludedFolderNameNotFound(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		baseline := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, baseline, nil); err != nil {
			t.Fatalf("collectClusterMetrics baseline: %v", err)
		}

		metrics := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, metrics, []string{"does-not-exist"}); err != nil {
			t.Fatalf("collectClusterMetrics with unknown folder: %v", err)
		}

		want := gaugeValue(t, baseline.vmCpuCores, simulatorClusterName)
		if got := gaugeValue(t, metrics.vmCpuCores, simulatorClusterName); got != want {
			t.Errorf("vm_cpu_cores_allocated = %v, want unchanged %v", got, want)
		}
	})
}

func TestCollectClusterMetricsWithoutClusters(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		metrics := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, metrics, nil); err != nil {
			t.Fatalf("collectClusterMetrics on ESX model: %v", err)
		}
	}, simulator.ESX())
}
