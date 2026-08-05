package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

func TestClassifyVmAllocation(t *testing.T) {
	excludedFolder := types.ManagedObjectReference{Type: "Folder", Value: "group-v42"}

	configuredVm := func() mo.VirtualMachine {
		var vm mo.VirtualMachine
		vm.Summary.Config.NumCpu = 2
		vm.Summary.Config.MemorySizeMB = 4096
		return vm
	}

	tests := []struct {
		name         string
		vm           mo.VirtualMachine
		excluded     map[types.ManagedObjectReference]string
		wantCounted  bool
		wantReason   string
		wantExcluded string
	}{
		{
			name:        "included",
			vm:          configuredVm(),
			wantCounted: true,
			wantReason:  vmAllocationReasonIncluded,
		},
		{
			name: "srm placeholder",
			vm: func() mo.VirtualMachine {
				vm := configuredVm()
				vm.Summary.Config.ManagedBy = &types.ManagedByInfo{
					ExtensionKey: srmManagedByExtensionKey,
					Type:         srmPlaceholderVmType,
				}
				return vm
			}(),
			wantReason: vmAllocationReasonSrmPlaceholder,
		},
		{
			name: "srm test vm remains included",
			vm: func() mo.VirtualMachine {
				vm := configuredVm()
				vm.Summary.Config.ManagedBy = &types.ManagedByInfo{
					ExtensionKey: srmManagedByExtensionKey,
					Type:         "testVm",
				}
				return vm
			}(),
			wantCounted: true,
			wantReason:  vmAllocationReasonIncluded,
		},
		{
			name: "unrelated manager remains included",
			vm: func() mo.VirtualMachine {
				vm := configuredVm()
				vm.Summary.Config.ManagedBy = &types.ManagedByInfo{
					ExtensionKey: "example.extension",
					Type:         srmPlaceholderVmType,
				}
				return vm
			}(),
			wantCounted: true,
			wantReason:  vmAllocationReasonIncluded,
		},
		{
			name: "template",
			vm: func() mo.VirtualMachine {
				vm := configuredVm()
				vm.Summary.Config.Template = true
				return vm
			}(),
			wantReason: vmAllocationReasonTemplate,
		},
		{
			name:       "unpopulated config",
			vm:         mo.VirtualMachine{},
			wantReason: vmAllocationReasonUnpopulatedConfig,
		},
		{
			name: "excluded folder",
			vm: func() mo.VirtualMachine {
				vm := configuredVm()
				vm.Parent = &excludedFolder
				return vm
			}(),
			excluded:     map[types.ManagedObjectReference]string{excludedFolder: "IT-Notfall"},
			wantReason:   vmAllocationReasonExcludedFolder,
			wantExcluded: "IT-Notfall",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			counted, reason, excludedBy := classifyVmAllocation(tc.vm, tc.excluded)
			if counted != tc.wantCounted || reason != tc.wantReason || excludedBy != tc.wantExcluded {
				t.Errorf("classifyVmAllocation() = (%v, %q, %q), want (%v, %q, %q)",
					counted, reason, excludedBy, tc.wantCounted, tc.wantReason, tc.wantExcluded)
			}
		})
	}
}

func TestSummarizeClusterHostsSeparatesPhysicalCoresAndLogicalThreads(t *testing.T) {
	host := func(name string, cores, threads int16, memory int64) mo.HostSystem {
		var host mo.HostSystem
		host.Name = name
		host.Summary.Hardware = &types.HostHardwareSummary{
			NumCpuCores:   cores,
			NumCpuThreads: threads,
			MemorySize:    memory,
		}
		return host
	}

	capacity, missingHardwareHost, complete := summarizeClusterHosts([]mo.HostSystem{
		host("host-a", 8, 16, 64),
		host("host-b", 12, 12, 128),
	})
	if !complete {
		t.Fatalf("summarizeClusterHosts incomplete, missing hardware for %q", missingHardwareHost)
	}

	want := clusterHostCapacity{
		cpuPhysicalCoresTotal: 20,
		cpuThreadsTotal:       28,
		cpuReserveThreads:     16,
		cpuReserveThreadHost:  "host-a",
		memoryReserveBytes:    128,
		memoryReserveHost:     "host-b",
	}
	if capacity != want {
		t.Errorf("summarizeClusterHosts() = %+v, want %+v", capacity, want)
	}

	const allocatedVcpus = int64(10)
	if got := capacity.cpuThreadsTotal - capacity.cpuReserveThreads - allocatedVcpus; got != 2 {
		t.Errorf("logical CPU threads available = %d, want 2", got)
	}
}

func TestClusterDebugDiagnostics(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		metrics := newClusterMetrics(prometheus.NewRegistry())

		if err := collectClusterMetricsWithDebug(ctx, client, metrics, nil, logger); err != nil {
			t.Fatalf("collectClusterMetricsWithDebug: %v", err)
		}

		logs := output.String()
		vmCount := len(clusterVmRefs(t, ctx, client))
		if got := strings.Count(logs, `msg="vm allocation decision"`); got != vmCount {
			t.Errorf("VM decision records = %d, want %d\n%s", got, vmCount, logs)
		}
		for _, fragment := range []string{
			`cluster=` + simulatorClusterName,
			`power_state=poweredOn`,
			`counted=true`,
			`reason=included`,
			`msg="cluster capacity calculation"`,
			`complete=true`,
			`cpu_ha_reserve_thread_host=`,
			`memory_ha_reserve_host=`,
			fmt.Sprintf("vm_vcpus_allocated=%d", int64(gaugeValue(t, metrics.vmVcpus, simulatorClusterName))),
			fmt.Sprintf("vm_memory_bytes_allocated=%d", int64(gaugeValue(t, metrics.vmMemory, simulatorClusterName))),
			fmt.Sprintf("memory_bytes_available=%d", int64(gaugeValue(t, metrics.memoryAvail, simulatorClusterName))),
			fmt.Sprintf("cpu_physical_cores_total=%d", int64(gaugeValue(t, metrics.cpuPhysicalCoresTotal, simulatorClusterName))),
			fmt.Sprintf("cpu_threads_total=%d", int64(gaugeValue(t, metrics.cpuThreadsTotal, simulatorClusterName))),
			fmt.Sprintf("cpu_threads_available=%d", int64(gaugeValue(t, metrics.cpuThreadsAvail, simulatorClusterName))),
			fmt.Sprintf("memory_bytes_total=%d", int64(gaugeValue(t, metrics.memoryTotal, simulatorClusterName))),
		} {
			if !strings.Contains(logs, fragment) {
				t.Errorf("debug output missing %q\n%s", fragment, logs)
			}
		}
	})
}

// TestLogClusterCalculationIncomplete pins the rule that an aborted calculation
// reports the values that were already computed. Reporting placeholder zeros
// here would make the debug record contradict the exported gauges.
func TestLogClusterCalculationIncomplete(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logClusterCalculation(logger, clusterCalculation{
		cluster:          "DC0_C9",
		reason:           "no_hosts",
		allocation:       vmAllocation{vcpus: 4, memoryBytes: 8192, totalVMs: 3, countedVMs: 2, skippedVMs: 1},
		memoryBytesTotal: 12345,
	})

	logs := output.String()
	for _, fragment := range []string{
		`cluster=DC0_C9`,
		`complete=false`,
		`reason=no_hosts`,
		`memory_bytes_total=12345`,
		`vm_vcpus_allocated=4`,
		`vm_memory_bytes_allocated=8192`,
		`vms_total=3`,
		`vms_counted=2`,
		`vms_skipped=1`,
	} {
		if !strings.Contains(logs, fragment) {
			t.Errorf("debug output missing %q\n%s", fragment, logs)
		}
	}
}

// TestClusterDebugDiagnosticsWithoutHosts checks that a cluster whose
// calculation aborts still produces a record, and that the record agrees with
// the gauges that were exported for it.
func TestClusterDebugDiagnosticsWithoutHosts(t *testing.T) {
	const emptyCluster = "DC0_C9"

	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		createEmptyCluster(t, ctx, client, emptyCluster)

		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		metrics := newClusterMetrics(prometheus.NewRegistry())

		if err := collectClusterMetricsWithDebug(ctx, client, metrics, nil, logger); err != nil {
			t.Fatalf("collectClusterMetricsWithDebug: %v", err)
		}

		record := calculationRecordFor(t, output.String(), emptyCluster)
		for _, fragment := range []string{
			`complete=false`,
			`reason=no_hosts`,
			fmt.Sprintf("memory_bytes_total=%d", int64(gaugeValue(t, metrics.memoryTotal, emptyCluster))),
			fmt.Sprintf("vm_vcpus_allocated=%d", int64(gaugeValue(t, metrics.vmVcpus, emptyCluster))),
		} {
			if !strings.Contains(record, fragment) {
				t.Errorf("record for %s missing %q\n%s", emptyCluster, fragment, record)
			}
		}
	})
}

// calculationRecordFor returns the single capacity calculation line of the
// given cluster, so assertions cannot accidentally match another cluster.
func calculationRecordFor(t *testing.T, logs, cluster string) string {
	t.Helper()

	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, `msg="cluster capacity calculation"`) && strings.Contains(line, "cluster="+cluster+" ") {
			return line
		}
	}

	t.Fatalf("no capacity calculation record for cluster %q\n%s", cluster, logs)
	return ""
}

// TestClusterDebugDiagnosticsSkippedVm checks that a VM which does not count
// towards the allocation still shows up in the debug output, with the reason it
// was skipped and the folder that caused it.
func TestClusterDebugDiagnosticsSkippedVm(t *testing.T) {
	const excludedFolder = "IT-Notfall"

	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		vm := firstCountedVm(t, ctx, client)
		folder := createChildFolder(t, ctx, client, vm.Parent, excludedFolder)
		moveVmInto(t, ctx, folder, vm)

		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		metrics := newClusterMetrics(prometheus.NewRegistry())

		if err := collectClusterMetricsWithDebug(ctx, client, metrics, []string{excludedFolder}, logger); err != nil {
			t.Fatalf("collectClusterMetricsWithDebug: %v", err)
		}

		logs := output.String()
		for _, fragment := range []string{
			`counted=false reason=` + vmAllocationReasonExcludedFolder,
			`excluded_by_folder=` + excludedFolder,
			`vms_skipped=1`,
			fmt.Sprintf("vms_total=%d", len(clusterVmRefs(t, ctx, client))),
		} {
			if !strings.Contains(logs, fragment) {
				t.Errorf("debug output missing %q\n%s", fragment, logs)
			}
		}
	})
}

func TestClusterDebugDiagnosticsSrmPlaceholderVm(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		vm := firstCountedVm(t, ctx, client)
		setVmManagedBy(t, ctx, client, vm, srmManagedByExtensionKey, srmPlaceholderVmType)

		var output bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
		metrics := newClusterMetrics(prometheus.NewRegistry())

		if err := collectClusterMetricsWithDebug(ctx, client, metrics, nil, logger); err != nil {
			t.Fatalf("collectClusterMetricsWithDebug: %v", err)
		}

		logs := output.String()
		for _, fragment := range []string{
			`counted=false reason=` + vmAllocationReasonSrmPlaceholder,
			`managed_by_extension=` + srmManagedByExtensionKey,
			`managed_by_type=` + srmPlaceholderVmType,
			`vms_skipped=1`,
		} {
			if !strings.Contains(logs, fragment) {
				t.Errorf("debug output missing %q\n%s", fragment, logs)
			}
		}
	})
}

func TestCollectClusterMetrics(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		reg := prometheus.NewRegistry()
		metrics := newClusterMetrics(reg)

		if err := collectClusterMetrics(ctx, client, metrics, nil); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}

		model := simulator.VPX()
		if got := gaugeValue(t, metrics.hostsTotal, simulatorClusterName); got != float64(model.ClusterHost) {
			t.Errorf("hosts_total = %v, want %v", got, model.ClusterHost)
		}

		stats := clusterHostStats(t, ctx, client)

		if got := gaugeValue(t, metrics.cpuPhysicalCoresTotal, simulatorClusterName); got != float64(stats.TotalCores) {
			t.Errorf("cpu_physical_cores_total = %v, want %v", got, stats.TotalCores)
		}
		if got := gaugeValue(t, metrics.cpuThreadsTotal, simulatorClusterName); got != float64(stats.TotalThreads) {
			t.Errorf("cpu_threads_total = %v, want %v", got, stats.TotalThreads)
		}

		vmVcpus := gaugeValue(t, metrics.vmVcpus, simulatorClusterName)
		vmMemory := gaugeValue(t, metrics.vmMemory, simulatorClusterName)
		if vmVcpus <= 0 {
			t.Errorf("vm_vcpus_allocated = %v, want > 0", vmVcpus)
		}
		if vmMemory <= 0 {
			t.Errorf("vm_memory_bytes_allocated = %v, want > 0", vmMemory)
		}

		// The exported available values must be reproducible from the other
		// exported gauges, so dashboards can rely on the identity.
		threadsTotal := gaugeValue(t, metrics.cpuThreadsTotal, simulatorClusterName)
		wantThreads := threadsTotal - float64(stats.MaxThreads) - vmVcpus
		if got := gaugeValue(t, metrics.cpuThreadsAvail, simulatorClusterName); got != wantThreads {
			t.Errorf("cpu_threads_available = %v, want %v (total %v - largest host %d - vCPUs %v)",
				got, wantThreads, threadsTotal, stats.MaxThreads, vmVcpus)
		}

		memoryTotal := gaugeValue(t, metrics.memoryTotal, simulatorClusterName)
		wantMemory := memoryTotal - float64(stats.MaxMemory) - vmMemory
		if got := gaugeValue(t, metrics.memoryAvail, simulatorClusterName); got != wantMemory {
			t.Errorf("memory_bytes_available = %v, want %v (total %v - largest host %d - vms %v)",
				got, wantMemory, memoryTotal, stats.MaxMemory, vmMemory)
		}

		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gathering metrics: %v", err)
		}
		removed := map[string]bool{
			"vcenter_cluster_cpu_cores_total":        true,
			"vcenter_cluster_vm_cpu_cores_allocated": true,
			"vcenter_cluster_cpu_cores_available":    true,
		}
		for _, family := range families {
			if removed[family.GetName()] {
				t.Errorf("removed metric family %q is still exported", family.GetName())
			}
		}
	})
}

func TestPoweredOffVmsAreCounted(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		before := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, before, nil); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}
		vcpusBefore := gaugeValue(t, before.vmVcpus, simulatorClusterName)
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

		if got := gaugeValue(t, after.vmVcpus, simulatorClusterName); got != vcpusBefore {
			t.Errorf("vm_vcpus_allocated after power off = %v, want unchanged %v", got, vcpusBefore)
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

		// Simulate leftover physical and logical-CPU series from a cluster that
		// no longer exists.
		metrics.hostsTotal.WithLabelValues("ghost-cluster").Set(99)
		metrics.cpuPhysicalCoresTotal.WithLabelValues("ghost-cluster").Set(99)
		metrics.cpuThreadsTotal.WithLabelValues("ghost-cluster").Set(99)
		metrics.vmVcpus.WithLabelValues("ghost-cluster").Set(99)
		metrics.cpuThreadsAvail.WithLabelValues("ghost-cluster").Set(99)

		if err := collectClusterMetrics(ctx, client, metrics, nil); err != nil {
			t.Fatalf("collectClusterMetrics: %v", err)
		}

		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gathering metrics: %v", err)
		}
		for _, family := range families {
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
func assertVmExcluded(t *testing.T, ctx context.Context, client *vim25.Client, excludedNames []string, vm countedVm, vcpusBefore, memoryBefore float64) {
	t.Helper()

	excluded := newClusterMetrics(prometheus.NewRegistry())
	if err := collectClusterMetrics(ctx, client, excluded, excludedNames); err != nil {
		t.Fatalf("collectClusterMetrics with exclusion: %v", err)
	}

	wantVcpus := vcpusBefore - vm.Cores
	if got := gaugeValue(t, excluded.vmVcpus, simulatorClusterName); got != wantVcpus {
		t.Errorf("vm_vcpus_allocated with exclusion = %v, want %v (%v - excluded vm %v)",
			got, wantVcpus, vcpusBefore, vm.Cores)
	}
	wantMemory := memoryBefore - vm.Memory
	if got := gaugeValue(t, excluded.vmMemory, simulatorClusterName); got != wantMemory {
		t.Errorf("vm_memory_bytes_allocated with exclusion = %v, want %v (%v - excluded vm %v)",
			got, wantMemory, memoryBefore, vm.Memory)
	}

	stats := clusterHostStats(t, ctx, client)
	threadsTotal := gaugeValue(t, excluded.cpuThreadsTotal, simulatorClusterName)
	wantThreadsAvail := threadsTotal - float64(stats.MaxThreads) - wantVcpus
	if got := gaugeValue(t, excluded.cpuThreadsAvail, simulatorClusterName); got != wantThreadsAvail {
		t.Errorf("cpu_threads_available with exclusion = %v, want %v", got, wantThreadsAvail)
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
		vcpusBefore := gaugeValue(t, baseline.vmVcpus, simulatorClusterName)
		memoryBefore := gaugeValue(t, baseline.vmMemory, simulatorClusterName)

		vm := firstCountedVm(t, ctx, client)
		folder := createChildFolder(t, ctx, client, vm.Parent, excludedFolder)
		moveVmInto(t, ctx, folder, vm)

		assertVmExcluded(t, ctx, client, []string{excludedFolder}, vm, vcpusBefore, memoryBefore)

		// Without the flag the very same VM counts again, folder placement
		// alone must not change anything.
		unfiltered := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, unfiltered, nil); err != nil {
			t.Fatalf("collectClusterMetrics without exclusion: %v", err)
		}
		if got := gaugeValue(t, unfiltered.vmVcpus, simulatorClusterName); got != vcpusBefore {
			t.Errorf("vm_vcpus_allocated without exclusion = %v, want unchanged %v", got, vcpusBefore)
		}
		if got := gaugeValue(t, unfiltered.vmMemory, simulatorClusterName); got != memoryBefore {
			t.Errorf("vm_memory_bytes_allocated without exclusion = %v, want unchanged %v", got, memoryBefore)
		}
	})
}

func TestSrmPlaceholderVmsNotCounted(t *testing.T) {
	simulator.Test(func(ctx context.Context, client *vim25.Client) {
		baseline := newClusterMetrics(prometheus.NewRegistry())
		if err := collectClusterMetrics(ctx, client, baseline, nil); err != nil {
			t.Fatalf("collectClusterMetrics baseline: %v", err)
		}
		vcpusBefore := gaugeValue(t, baseline.vmVcpus, simulatorClusterName)
		memoryBefore := gaugeValue(t, baseline.vmMemory, simulatorClusterName)

		vm := firstCountedVm(t, ctx, client)
		setVmManagedBy(t, ctx, client, vm, srmManagedByExtensionKey, srmPlaceholderVmType)

		assertVmExcluded(t, ctx, client, nil, vm, vcpusBefore, memoryBefore)
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
		vcpusBefore := gaugeValue(t, baseline.vmVcpus, simulatorClusterName)
		memoryBefore := gaugeValue(t, baseline.vmMemory, simulatorClusterName)

		vm := firstCountedVm(t, ctx, client)
		parent := createChildFolder(t, ctx, client, vm.Parent, excludedFolder)
		nested := createChildFolder(t, ctx, client, parent.Reference(), "nested")
		moveVmInto(t, ctx, nested, vm)

		assertVmExcluded(t, ctx, client, []string{excludedFolder}, vm, vcpusBefore, memoryBefore)
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

		want := gaugeValue(t, baseline.vmVcpus, simulatorClusterName)
		if got := gaugeValue(t, metrics.vmVcpus, simulatorClusterName); got != want {
			t.Errorf("vm_vcpus_allocated = %v, want unchanged %v", got, want)
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
