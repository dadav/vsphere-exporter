package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// clusterMetrics holds the compute cluster capacity gauges. All gauges are
// labelled with the cluster name.
type clusterMetrics struct {
	hostsTotal     *prometheus.GaugeVec
	hostsEffective *prometheus.GaugeVec
	cpuCoresTotal  *prometheus.GaugeVec
	cpuMhzTotal    *prometheus.GaugeVec
	memoryTotal    *prometheus.GaugeVec
	vmCpuCores     *prometheus.GaugeVec
	vmMemory       *prometheus.GaugeVec
	cpuCoresAvail  *prometheus.GaugeVec
	memoryAvail    *prometheus.GaugeVec
	scrapeFailures prometheus.Counter
}

func newClusterMetrics(reg prometheus.Registerer) *clusterMetrics {
	auto := promauto.With(reg)
	return &clusterMetrics{
		hostsTotal: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_hosts_total",
			Help: "Number of hosts in the compute cluster",
		}, []string{"name"}),
		hostsEffective: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_hosts_effective",
			Help: "Number of effective (connected, not in maintenance) hosts in the compute cluster",
		}, []string{"name"}),
		cpuCoresTotal: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_cpu_cores_total",
			Help: "Total number of physical CPU cores of all hosts in the compute cluster",
		}, []string{"name"}),
		cpuMhzTotal: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_cpu_mhz_total",
			Help: "Total CPU capacity of the compute cluster in MHz",
		}, []string{"name"}),
		memoryTotal: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_memory_bytes_total",
			Help: "Total memory capacity of the compute cluster in bytes",
		}, []string{"name"}),
		vmCpuCores: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_vm_cpu_cores_allocated",
			Help: "Sum of configured vCPUs of all virtual machines in the compute cluster, including powered off ones",
		}, []string{"name"}),
		vmMemory: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_vm_memory_bytes_allocated",
			Help: "Sum of configured memory of all virtual machines in the compute cluster in bytes, including powered off ones",
		}, []string{"name"}),
		cpuCoresAvail: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_cpu_cores_available",
			Help: "Physical CPU cores left in the compute cluster after subtracting the largest host (HA reserve) and all allocated vCPUs. May be negative on overcommit",
		}, []string{"name"}),
		memoryAvail: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_memory_bytes_available",
			Help: "Memory left in the compute cluster in bytes after subtracting the largest host (HA reserve) and all allocated memory. May be negative on overcommit",
		}, []string{"name"}),
		scrapeFailures: auto.NewCounter(prometheus.CounterOpts{
			Name: "vcenter_cluster_scrape_failures_total",
			Help: "Number of cluster scrape failures",
		}),
	}
}

// resetGauges drops all previously exported per-cluster series so that
// clusters removed or renamed in vCenter do not linger with stale values.
func (m *clusterMetrics) resetGauges() {
	m.hostsTotal.Reset()
	m.hostsEffective.Reset()
	m.cpuCoresTotal.Reset()
	m.cpuMhzTotal.Reset()
	m.memoryTotal.Reset()
	m.vmCpuCores.Reset()
	m.vmMemory.Reset()
	m.cpuCoresAvail.Reset()
	m.memoryAvail.Reset()
}

// collectClusterMetrics does a single scrape pass over all compute clusters.
//
// Available resources are computed as:
//
//	total - largest host (reserved for HA failover) - sum of all VM allocations
//
// VM allocations come from the VM config and therefore include powered off VMs.
// Available values may be negative when the cluster is overcommitted.
//
// Errors of individual clusters are collected and returned joined, so one
// broken cluster neither aborts the pass nor stays invisible to the caller.
func collectClusterMetrics(ctx context.Context, client *vim25.Client, metrics *clusterMetrics) error {
	viewManager := view.NewManager(client)

	clusterView, err := viewManager.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		return fmt.Errorf("creating cluster container view: %w", err)
	}
	defer clusterView.Destroy(ctx)

	var clusters []mo.ClusterComputeResource
	if err := clusterView.Retrieve(ctx, []string{"ClusterComputeResource"}, []string{"name", "summary", "host"}, &clusters); err != nil {
		return fmt.Errorf("retrieving clusters: %w", err)
	}

	propCollector := property.DefaultCollector(client)

	// Reset only after the cluster list was retrieved successfully, so a
	// failing vCenter connection does not wipe the last known good values.
	metrics.resetGauges()

	var errs []error
	for _, cluster := range clusters {
		if err := collectOneCluster(ctx, viewManager, propCollector, cluster, metrics); err != nil {
			errs = append(errs, fmt.Errorf("cluster %q: %w", cluster.Name, err))
		}
	}

	return errors.Join(errs...)
}

func collectOneCluster(ctx context.Context, viewManager *view.Manager, propCollector *property.Collector, cluster mo.ClusterComputeResource, metrics *clusterMetrics) error {
	name := cluster.Name

	var summary *types.ComputeResourceSummary
	if cluster.Summary != nil {
		summary = cluster.Summary.GetComputeResourceSummary()
		metrics.hostsTotal.WithLabelValues(name).Set(float64(summary.NumHosts))
		metrics.hostsEffective.WithLabelValues(name).Set(float64(summary.NumEffectiveHosts))
		metrics.cpuMhzTotal.WithLabelValues(name).Set(float64(summary.TotalCpu))
		metrics.memoryTotal.WithLabelValues(name).Set(float64(summary.TotalMemory))
	} else {
		slog.Warn("cluster has no summary, skipping capacity totals", "cluster", name)
	}

	// Allocation does not depend on host data, so it is exported even for
	// clusters without hosts.
	vmCores, vmMemory, err := clusterVmAllocation(ctx, viewManager, cluster.Reference())
	if err != nil {
		return fmt.Errorf("retrieving vms: %w", err)
	}
	metrics.vmCpuCores.WithLabelValues(name).Set(float64(vmCores))
	metrics.vmMemory.WithLabelValues(name).Set(float64(vmMemory))

	if len(cluster.Host) == 0 {
		slog.Warn("cluster has no hosts, skipping available resources", "cluster", name)
		return nil
	}

	var hosts []mo.HostSystem
	if err := propCollector.Retrieve(ctx, cluster.Host, []string{"summary.hardware"}, &hosts); err != nil {
		return fmt.Errorf("retrieving hosts: %w", err)
	}

	// Largest host by cores and largest host by memory are tracked
	// independently, which is the conservative HA reserve for
	// heterogeneous clusters.
	var totalCores, maxHostCores, maxHostMemory int64
	for _, host := range hosts {
		if host.Summary.Hardware == nil {
			// Incomplete hardware data (e.g. disconnected host) would make
			// the core and available values silently wrong, so skip them
			// entirely for this cluster instead of exporting bad numbers.
			slog.Warn("host has no hardware summary, skipping core and available metrics",
				"cluster", name, "host", host.Reference().Value)
			return nil
		}
		cores := int64(host.Summary.Hardware.NumCpuCores)
		if cores > maxHostCores {
			maxHostCores = cores
		}
		totalCores += cores

		if memory := host.Summary.Hardware.MemorySize; memory > maxHostMemory {
			maxHostMemory = memory
		}
	}

	metrics.cpuCoresTotal.WithLabelValues(name).Set(float64(totalCores))
	metrics.cpuCoresAvail.WithLabelValues(name).Set(float64(totalCores - maxHostCores - vmCores))

	// Available memory is derived from the same cluster summary value that is
	// exported as the total, so both gauges stay consistent.
	if summary == nil {
		slog.Warn("cluster has no summary, skipping available memory", "cluster", name)
		return nil
	}
	metrics.memoryAvail.WithLabelValues(name).Set(float64(summary.TotalMemory - maxHostMemory - vmMemory))

	return nil
}

// clusterVmAllocation sums the configured vCPUs and memory (bytes) of all
// virtual machines inside the given cluster. Templates and VMs without a
// populated config are skipped. Power state is deliberately ignored, so
// powered off VMs count towards the allocation.
func clusterVmAllocation(ctx context.Context, viewManager *view.Manager, cluster types.ManagedObjectReference) (int64, int64, error) {
	vmView, err := viewManager.CreateContainerView(ctx, cluster, []string{"VirtualMachine"}, true)
	if err != nil {
		return 0, 0, fmt.Errorf("creating vm container view: %w", err)
	}
	defer vmView.Destroy(ctx)

	var vms []mo.VirtualMachine
	if err := vmView.Retrieve(ctx, []string{"VirtualMachine"}, []string{"summary.config"}, &vms); err != nil {
		return 0, 0, err
	}

	var cores, memory int64
	for _, vm := range vms {
		if vm.Summary.Config.Template {
			continue
		}
		if vm.Summary.Config.NumCpu == 0 {
			// Config not populated yet, e.g. VM currently being created.
			continue
		}
		cores += int64(vm.Summary.Config.NumCpu)
		memory += int64(vm.Summary.Config.MemorySizeMB) * 1024 * 1024
	}
	return cores, memory, nil
}

func clusterMetricsLoop(ctx context.Context, client *vim25.Client, interval int64, metrics *clusterMetrics) {
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()

		metrics.scrapeFailures.Add(0)

		if err := collectClusterMetrics(ctx, client, metrics); err != nil {
			metrics.scrapeFailures.Inc()
			slog.Error("cluster scrape failed", "err", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := collectClusterMetrics(ctx, client, metrics); err != nil {
					metrics.scrapeFailures.Inc()
					slog.Error("cluster scrape failed", "err", err)
				}
			}
		}
	}()
}
