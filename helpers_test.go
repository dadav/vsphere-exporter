package main

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/property"
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
	TotalCores   int64
	TotalThreads int64
	MaxThreads   int64
	TotalMemory  int64
	MaxMemory    int64
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
		threads := int64(host.Summary.Hardware.NumCpuThreads)
		memory := host.Summary.Hardware.MemorySize

		stats.TotalCores += cores
		stats.TotalThreads += threads
		stats.TotalMemory += memory
		if threads > stats.MaxThreads {
			stats.MaxThreads = threads
		}
		if memory > stats.MaxMemory {
			stats.MaxMemory = memory
		}
	}
	return stats
}

// countedVm is a VM that clusterVmAllocation actually counts, together with its
// configured size and its parent folder.
type countedVm struct {
	Ref    types.ManagedObjectReference
	Parent types.ManagedObjectReference
	Cores  float64
	Memory float64
}

// firstCountedVm returns the first cluster VM that contributes to the
// allocation gauges, i.e. neither a template nor one with an unpopulated
// config, mirroring the skip rules of clusterVmAllocation.
func firstCountedVm(t *testing.T, ctx context.Context, client *vim25.Client) countedVm {
	t.Helper()

	refs := clusterVmRefs(t, ctx, client)
	if len(refs) == 0 {
		t.Fatal("no vms in cluster")
	}

	var vms []mo.VirtualMachine
	if err := property.DefaultCollector(client).Retrieve(ctx, refs, []string{"summary.config", "parent"}, &vms); err != nil {
		t.Fatalf("retrieving vms: %v", err)
	}

	for _, vm := range vms {
		if vm.Summary.Config.Template || vm.Summary.Config.NumCpu == 0 {
			continue
		}
		if vm.Parent == nil {
			continue
		}
		return countedVm{
			Ref:    vm.Reference(),
			Parent: *vm.Parent,
			Cores:  float64(vm.Summary.Config.NumCpu),
			Memory: float64(int64(vm.Summary.Config.MemorySizeMB) * 1024 * 1024),
		}
	}

	t.Fatal("no counted vm with a parent folder in cluster")
	return countedVm{}
}

// createEmptyCluster creates a compute cluster without any hosts, the inventory
// shape that makes the capacity calculation abort after the VM allocation.
func createEmptyCluster(t *testing.T, ctx context.Context, client *vim25.Client, name string) {
	t.Helper()

	viewManager := view.NewManager(client)
	datacenterView, err := viewManager.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"Datacenter"}, true)
	if err != nil {
		t.Fatalf("creating datacenter view: %v", err)
	}
	defer datacenterView.Destroy(ctx)

	var datacenters []mo.Datacenter
	if err := datacenterView.Retrieve(ctx, []string{"Datacenter"}, []string{"hostFolder"}, &datacenters); err != nil {
		t.Fatalf("retrieving datacenters: %v", err)
	}
	if len(datacenters) == 0 {
		t.Fatal("no datacenter found")
	}

	if _, err := object.NewFolder(client, datacenters[0].HostFolder).CreateCluster(ctx, name, types.ClusterConfigSpecEx{}); err != nil {
		t.Fatalf("creating cluster %q: %v", name, err)
	}
}

// createChildFolder creates a folder below the given parent folder.
func createChildFolder(t *testing.T, ctx context.Context, client *vim25.Client, parent types.ManagedObjectReference, name string) *object.Folder {
	t.Helper()

	folder, err := object.NewFolder(client, parent).CreateFolder(ctx, name)
	if err != nil {
		t.Fatalf("creating folder %q: %v", name, err)
	}
	return folder
}

// moveVmInto moves the VM into the given folder, which is how SRM placeholder
// VMs end up in their own folder.
func moveVmInto(t *testing.T, ctx context.Context, folder *object.Folder, vm countedVm) {
	t.Helper()

	task, err := folder.MoveInto(ctx, []types.ManagedObjectReference{vm.Ref})
	if err != nil {
		t.Fatalf("moving vm into folder: %v", err)
	}
	if err := task.Wait(ctx); err != nil {
		t.Fatalf("waiting for move into folder: %v", err)
	}
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
