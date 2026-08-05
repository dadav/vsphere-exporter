package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

const (
	vmAllocationReasonIncluded          = "included"
	vmAllocationReasonTemplate          = "template"
	vmAllocationReasonUnpopulatedConfig = "unpopulated_config"
	vmAllocationReasonExcludedFolder    = "excluded_folder"
)

type vmAllocation struct {
	vcpus       int64
	memoryBytes int64
	totalVMs    int
	countedVMs  int
	skippedVMs  int
}

type clusterHostCapacity struct {
	cpuCoresTotal        int64
	cpuReserveCores      int64
	cpuReserveHost       string
	cpuThreadsTotal      int64
	cpuReserveThreads    int64
	cpuReserveThreadHost string
	memoryReserveBytes   int64
	memoryReserveHost    string
}

// clusterCalculation captures the inputs of the cluster capacity formulas so
// debug mode explains exactly the numbers that were exported. Fields are filled
// in as they are computed, so the record of an aborted calculation carries
// everything known up to that point and zero for the rest. complete says
// whether all gauges of the cluster were exported, reason names the step that
// stopped it.
type clusterCalculation struct {
	cluster    string
	complete   bool
	reason     string
	allocation vmAllocation

	cpuCoresTotal        int64
	cpuReserveCores      int64
	cpuReserveHost       string
	cpuCoresAvailable    int64
	cpuThreadsTotal      int64
	cpuReserveThreads    int64
	cpuReserveThreadHost string
	cpuThreadsAvailable  int64

	memoryBytesTotal     int64
	memoryReserveBytes   int64
	memoryReserveHost    string
	memoryBytesAvailable int64
}

// clusterMetrics holds the compute cluster capacity gauges. All gauges are
// labelled with the cluster name.
type clusterMetrics struct {
	hostsTotal      *prometheus.GaugeVec
	hostsEffective  *prometheus.GaugeVec
	cpuCoresTotal   *prometheus.GaugeVec
	cpuThreadsTotal *prometheus.GaugeVec
	cpuMhzTotal     *prometheus.GaugeVec
	memoryTotal     *prometheus.GaugeVec
	vmCpuCores      *prometheus.GaugeVec
	vmVcpus         *prometheus.GaugeVec
	vmMemory        *prometheus.GaugeVec
	cpuCoresAvail   *prometheus.GaugeVec
	cpuThreadsAvail *prometheus.GaugeVec
	memoryAvail     *prometheus.GaugeVec
	scrapeFailures  prometheus.Counter
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
		cpuThreadsTotal: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_cpu_threads_total",
			Help: "Total number of logical CPU threads of all hosts in the compute cluster",
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
			Help: "Deprecated: use vcenter_cluster_vm_vcpus_allocated. Sum of configured vCPUs of all virtual machines in the compute cluster, including powered off ones, excluding VMs in excluded folders",
		}, []string{"name"}),
		vmVcpus: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_vm_vcpus_allocated",
			Help: "Sum of configured vCPUs of all virtual machines in the compute cluster, including powered off ones, excluding VMs in excluded folders",
		}, []string{"name"}),
		vmMemory: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_vm_memory_bytes_allocated",
			Help: "Sum of configured memory of all virtual machines in the compute cluster in bytes, including powered off ones, excluding VMs in excluded folders",
		}, []string{"name"}),
		cpuCoresAvail: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_cpu_cores_available",
			Help: "Deprecated: use vcenter_cluster_cpu_threads_available. Legacy physical CPU core calculation after subtracting the largest host and all allocated vCPUs. May be negative on overcommit",
		}, []string{"name"}),
		cpuThreadsAvail: auto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_cluster_cpu_threads_available",
			Help: "Logical CPU threads left in the compute cluster after subtracting the largest host (HA reserve) and all allocated vCPUs. May be negative on overcommit",
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
	m.cpuThreadsTotal.Reset()
	m.cpuMhzTotal.Reset()
	m.memoryTotal.Reset()
	m.vmCpuCores.Reset()
	m.vmVcpus.Reset()
	m.vmMemory.Reset()
	m.cpuCoresAvail.Reset()
	m.cpuThreadsAvail.Reset()
	m.memoryAvail.Reset()
}

// collectClusterMetrics does a single scrape pass over all compute clusters.
// It is the entry point of the scrape loop, debug diagnostics are off.
func collectClusterMetrics(ctx context.Context, client *vim25.Client, metrics *clusterMetrics, excludedFolderNames []string) error {
	return collectClusterMetricsWithDebug(ctx, client, metrics, excludedFolderNames, nil)
}

// collectClusterMetricsWithDebug does a single scrape pass over all compute
// clusters. A non-nil debugLogger additionally emits one record per VM
// allocation decision and one per cluster capacity calculation, so the exported
// numbers can be explained without a second code path.
//
// Logical CPU and memory availability are computed as:
//
//	total - largest host (reserved for HA failover) - sum of all VM allocations
//
// CPU capacity uses logical host threads so it has the same scheduling unit as
// configured VM vCPUs. VM allocations come from the VM config and therefore
// include powered off VMs. Available values may be negative when the cluster is
// overcommitted. The physical-core availability gauge keeps its legacy formula
// for compatibility.
//
// excludedFolderNames are VM folder names whose VMs, including those in
// subfolders, do not count towards the allocation, e.g. the folder Site
// Recovery Manager keeps its placeholder VMs in. An empty list disables the
// lookup entirely.
//
// Errors of individual clusters are collected and returned joined, so one
// broken cluster neither aborts the pass nor stays invisible to the caller.
func collectClusterMetricsWithDebug(ctx context.Context, client *vim25.Client, metrics *clusterMetrics, excludedFolderNames []string, debugLogger *slog.Logger) error {
	viewManager := view.NewManager(client)

	excludedFolders, err := resolveExcludedFolders(ctx, viewManager, client.ServiceContent.RootFolder, excludedFolderNames)
	if err != nil {
		return fmt.Errorf("resolving excluded folders: %w", err)
	}

	clusterView, err := viewManager.CreateContainerView(ctx, client.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		return fmt.Errorf("creating cluster container view: %w", err)
	}
	defer clusterView.Destroy(ctx)

	var clusters []mo.ClusterComputeResource
	if err := clusterView.Retrieve(ctx, []string{"ClusterComputeResource"}, []string{"name", "summary", "host"}, &clusters); err != nil {
		return fmt.Errorf("retrieving clusters: %w", err)
	}
	if debugLogger != nil {
		sortByName(clusters, func(cluster mo.ClusterComputeResource) string { return cluster.Name })
	}

	propCollector := property.DefaultCollector(client)

	// Reset only after the cluster list was retrieved successfully, so a
	// failing vCenter connection does not wipe the last known good values.
	metrics.resetGauges()

	var errs []error
	for _, cluster := range clusters {
		if err := collectOneCluster(ctx, viewManager, propCollector, cluster, excludedFolders, metrics, debugLogger); err != nil {
			errs = append(errs, fmt.Errorf("cluster %q: %w", cluster.Name, err))
		}
	}

	return errors.Join(errs...)
}

// resolveExcludedFolders looks up all folders whose name matches one of the
// given names and returns them plus all their descendant folders as a set, so
// VMs in subfolders of an excluded folder are excluded as well. Names are
// matched against the whole inventory, so the same name in several datacenters
// is honoured.
//
// A name without any matching folder is logged and otherwise ignored, it is not
// an error: the folder may not exist yet or may have been renamed, and that must
// not stop the scrape.
func resolveExcludedFolders(ctx context.Context, viewManager *view.Manager, root types.ManagedObjectReference, names []string) (map[types.ManagedObjectReference]string, error) {
	if len(names) == 0 {
		return nil, nil
	}

	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = false
	}

	folderView, err := viewManager.CreateContainerView(ctx, root, []string{"Folder"}, true)
	if err != nil {
		return nil, fmt.Errorf("creating folder container view: %w", err)
	}
	defer folderView.Destroy(ctx)

	var folders []mo.Folder
	if err := folderView.Retrieve(ctx, []string{"Folder"}, []string{"name", "parent"}, &folders); err != nil {
		return nil, fmt.Errorf("retrieving folders: %w", err)
	}

	excluded := make(map[types.ManagedObjectReference]string)
	parents := make(map[types.ManagedObjectReference]types.ManagedObjectReference, len(folders))
	for _, folder := range folders {
		if folder.Parent != nil {
			parents[folder.Reference()] = *folder.Parent
		}
		if _, ok := wanted[folder.Name]; !ok {
			continue
		}
		wanted[folder.Name] = true
		excluded[folder.Reference()] = folder.Name
	}

	// Expand to all descendant folders. Each iteration pulls in the folders
	// directly below an already excluded one, so the loop runs at most
	// max-folder-depth times before reaching the fixpoint.
	for changed := true; changed; {
		changed = false
		for folder, parent := range parents {
			if _, alreadyExcluded := excluded[folder]; alreadyExcluded {
				continue
			}
			if excludedBy, parentExcluded := excluded[parent]; parentExcluded {
				excluded[folder] = excludedBy
				changed = true
			}
		}
	}

	for name, found := range wanted {
		if !found {
			slog.Warn("excluded vm folder not found in inventory", "folder", name)
		}
	}

	return excluded, nil
}

func collectOneCluster(ctx context.Context, viewManager *view.Manager, propCollector *property.Collector, cluster mo.ClusterComputeResource, excludedFolders map[types.ManagedObjectReference]string, metrics *clusterMetrics, debugLogger *slog.Logger) error {
	name := cluster.Name
	calc := clusterCalculation{cluster: name}

	var summary *types.ComputeResourceSummary
	if cluster.Summary != nil {
		summary = cluster.Summary.GetComputeResourceSummary()
		metrics.hostsTotal.WithLabelValues(name).Set(float64(summary.NumHosts))
		metrics.hostsEffective.WithLabelValues(name).Set(float64(summary.NumEffectiveHosts))
		metrics.cpuMhzTotal.WithLabelValues(name).Set(float64(summary.TotalCpu))
		metrics.memoryTotal.WithLabelValues(name).Set(float64(summary.TotalMemory))
		// Recorded here and not only on the complete path, so the debug record
		// of an aborted calculation still matches the exported total.
		calc.memoryBytesTotal = summary.TotalMemory
	} else {
		slog.Warn("cluster has no summary, skipping capacity totals", "cluster", name)
	}

	// Allocation does not depend on host data, so it is exported even for
	// clusters without hosts.
	allocation, err := clusterVmAllocation(ctx, viewManager, cluster.Reference(), name, excludedFolders, debugLogger)
	if err != nil {
		return fmt.Errorf("retrieving vms: %w", err)
	}
	calc.allocation = allocation
	metrics.vmCpuCores.WithLabelValues(name).Set(float64(allocation.vcpus))
	metrics.vmVcpus.WithLabelValues(name).Set(float64(allocation.vcpus))
	metrics.vmMemory.WithLabelValues(name).Set(float64(allocation.memoryBytes))

	if len(cluster.Host) == 0 {
		calc.reason = "no_hosts"
		logClusterCalculation(debugLogger, calc)
		slog.Warn("cluster has no hosts, skipping available resources", "cluster", name)
		return nil
	}

	var hosts []mo.HostSystem
	if err := propCollector.Retrieve(ctx, cluster.Host, []string{"name", "summary.hardware"}, &hosts); err != nil {
		return fmt.Errorf("retrieving hosts: %w", err)
	}
	if debugLogger != nil {
		sortByName(hosts, func(host mo.HostSystem) string { return host.Name })
	}

	capacity, missingHardwareHost, complete := summarizeClusterHosts(hosts)
	if !complete {
		calc.reason = "host_hardware_missing"
		logClusterCalculation(debugLogger, calc)
		// Incomplete hardware data (e.g. disconnected host) would make
		// the host-derived capacity values silently wrong, so skip them
		// entirely for this cluster instead of exporting bad numbers.
		slog.Warn("host has no hardware summary, skipping core, thread and available metrics",
			"cluster", name, "host", missingHardwareHost)
		return nil
	}

	calc.cpuCoresTotal = capacity.cpuCoresTotal
	calc.cpuReserveCores = capacity.cpuReserveCores
	calc.cpuReserveHost = capacity.cpuReserveHost
	calc.cpuCoresAvailable = capacity.cpuCoresTotal - capacity.cpuReserveCores - allocation.vcpus
	calc.cpuThreadsTotal = capacity.cpuThreadsTotal
	calc.cpuReserveThreads = capacity.cpuReserveThreads
	calc.cpuReserveThreadHost = capacity.cpuReserveThreadHost
	calc.cpuThreadsAvailable = capacity.cpuThreadsTotal - capacity.cpuReserveThreads - allocation.vcpus
	calc.memoryReserveBytes = capacity.memoryReserveBytes
	calc.memoryReserveHost = capacity.memoryReserveHost

	metrics.cpuCoresTotal.WithLabelValues(name).Set(float64(capacity.cpuCoresTotal))
	metrics.cpuCoresAvail.WithLabelValues(name).Set(float64(calc.cpuCoresAvailable))
	metrics.cpuThreadsTotal.WithLabelValues(name).Set(float64(capacity.cpuThreadsTotal))
	metrics.cpuThreadsAvail.WithLabelValues(name).Set(float64(calc.cpuThreadsAvailable))

	// Available memory is derived from the same cluster summary value that is
	// exported as the total, so both gauges stay consistent.
	if summary == nil {
		calc.reason = "cluster_summary_missing"
		logClusterCalculation(debugLogger, calc)
		slog.Warn("cluster has no summary, skipping available memory", "cluster", name)
		return nil
	}
	calc.memoryBytesAvailable = summary.TotalMemory - capacity.memoryReserveBytes - allocation.memoryBytes
	metrics.memoryAvail.WithLabelValues(name).Set(float64(calc.memoryBytesAvailable))

	calc.complete = true
	calc.reason = "complete"
	logClusterCalculation(debugLogger, calc)

	return nil
}

// summarizeClusterHosts derives the physical-core, logical-thread and memory
// HA reserve inputs independently. Heterogeneous clusters can have different
// largest hosts for each resource.
func summarizeClusterHosts(hosts []mo.HostSystem) (clusterHostCapacity, string, bool) {
	var capacity clusterHostCapacity
	for _, host := range hosts {
		if host.Summary.Hardware == nil {
			return clusterHostCapacity{}, host.Reference().Value, false
		}

		cores := int64(host.Summary.Hardware.NumCpuCores)
		capacity.cpuCoresTotal += cores
		if cores > capacity.cpuReserveCores {
			capacity.cpuReserveCores = cores
			capacity.cpuReserveHost = host.Name
		}

		threads := int64(host.Summary.Hardware.NumCpuThreads)
		capacity.cpuThreadsTotal += threads
		if threads > capacity.cpuReserveThreads {
			capacity.cpuReserveThreads = threads
			capacity.cpuReserveThreadHost = host.Name
		}

		memory := host.Summary.Hardware.MemorySize
		if memory > capacity.memoryReserveBytes {
			capacity.memoryReserveBytes = memory
			capacity.memoryReserveHost = host.Name
		}
	}

	return capacity, "", true
}

// clusterVmAllocation sums the configured vCPUs and memory (bytes) of all
// virtual machines inside the given cluster. Templates, VMs without a populated
// config and VMs whose parent folder is in excludedFolders are skipped. The
// excluded set already contains all descendant folders, so the direct parent
// lookup covers subfolders too. Power state is deliberately ignored, so powered
// off VMs count towards the allocation.
func clusterVmAllocation(ctx context.Context, viewManager *view.Manager, cluster types.ManagedObjectReference, clusterName string, excludedFolders map[types.ManagedObjectReference]string, debugLogger *slog.Logger) (vmAllocation, error) {
	vmView, err := viewManager.CreateContainerView(ctx, cluster, []string{"VirtualMachine"}, true)
	if err != nil {
		return vmAllocation{}, fmt.Errorf("creating vm container view: %w", err)
	}
	defer vmView.Destroy(ctx)

	var vms []mo.VirtualMachine
	properties := []string{"summary.config", "parent"}
	if debugLogger != nil {
		properties = append(properties, "name", "summary.runtime.powerState")
	}
	if err := vmView.Retrieve(ctx, []string{"VirtualMachine"}, properties, &vms); err != nil {
		return vmAllocation{}, err
	}
	if debugLogger != nil {
		sortByName(vms, func(vm mo.VirtualMachine) string { return vm.Name })
	}

	allocation := vmAllocation{totalVMs: len(vms)}
	for _, vm := range vms {
		counted, reason, excludedBy := classifyVmAllocation(vm, excludedFolders)
		memoryBytes := int64(vm.Summary.Config.MemorySizeMB) * 1024 * 1024
		if debugLogger != nil {
			debugLogger.Debug("vm allocation decision",
				"cluster", clusterName,
				"vm", vm.Name,
				"vm_ref", vm.Reference().Value,
				"power_state", vm.Summary.Runtime.PowerState,
				"cpu_cores", vm.Summary.Config.NumCpu,
				"vcpus", vm.Summary.Config.NumCpu,
				"memory_mib", vm.Summary.Config.MemorySizeMB,
				"memory_bytes", memoryBytes,
				"counted", counted,
				"reason", reason,
				"excluded_by_folder", excludedBy)
		}
		if !counted {
			allocation.skippedVMs++
			continue
		}
		allocation.countedVMs++
		allocation.vcpus += int64(vm.Summary.Config.NumCpu)
		allocation.memoryBytes += memoryBytes
	}
	return allocation, nil
}

func classifyVmAllocation(vm mo.VirtualMachine, excludedFolders map[types.ManagedObjectReference]string) (bool, string, string) {
	if vm.Summary.Config.Template {
		return false, vmAllocationReasonTemplate, ""
	}
	if vm.Summary.Config.NumCpu == 0 {
		return false, vmAllocationReasonUnpopulatedConfig, ""
	}
	// The parent of a VM is its folder, while cluster membership comes from
	// the resource pool. An excluded folder does not hide the VM from the view.
	if vm.Parent != nil {
		if excludedBy, excluded := excludedFolders[*vm.Parent]; excluded {
			return false, vmAllocationReasonExcludedFolder, excludedBy
		}
	}
	return true, vmAllocationReasonIncluded, ""
}

// sortByName gives debug output a stable order. The managed object reference
// breaks ties between identically named objects, which vCenter allows across
// datacenters and folders.
func sortByName[T mo.Reference](items []T, name func(T) string) {
	slices.SortFunc(items, func(a, b T) int {
		return cmp.Or(
			cmp.Compare(name(a), name(b)),
			cmp.Compare(a.Reference().Value, b.Reference().Value),
		)
	})
}

func logClusterCalculation(logger *slog.Logger, calc clusterCalculation) {
	if logger == nil {
		return
	}
	logger.Debug("cluster capacity calculation",
		"cluster", calc.cluster,
		"complete", calc.complete,
		"reason", calc.reason,
		"vms_total", calc.allocation.totalVMs,
		"vms_counted", calc.allocation.countedVMs,
		"vms_skipped", calc.allocation.skippedVMs,
		"cpu_cores_total", calc.cpuCoresTotal,
		"cpu_ha_reserve_cores", calc.cpuReserveCores,
		"cpu_ha_reserve_host", calc.cpuReserveHost,
		"vm_cpu_cores_allocated", calc.allocation.vcpus,
		"cpu_cores_available", calc.cpuCoresAvailable,
		"cpu_threads_total", calc.cpuThreadsTotal,
		"cpu_ha_reserve_threads", calc.cpuReserveThreads,
		"cpu_ha_reserve_thread_host", calc.cpuReserveThreadHost,
		"vm_vcpus_allocated", calc.allocation.vcpus,
		"cpu_threads_available", calc.cpuThreadsAvailable,
		"memory_bytes_total", calc.memoryBytesTotal,
		"memory_ha_reserve_bytes", calc.memoryReserveBytes,
		"memory_ha_reserve_host", calc.memoryReserveHost,
		"vm_memory_bytes_allocated", calc.allocation.memoryBytes,
		"memory_bytes_available", calc.memoryBytesAvailable)
}

func clusterMetricsLoop(ctx context.Context, client *vim25.Client, interval int64, metrics *clusterMetrics, excludedFolderNames []string) {
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()

		metrics.scrapeFailures.Add(0)

		if err := collectClusterMetrics(ctx, client, metrics, excludedFolderNames); err != nil {
			metrics.scrapeFailures.Inc()
			slog.Error("cluster scrape failed", "err", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := collectClusterMetrics(ctx, client, metrics, excludedFolderNames); err != nil {
					metrics.scrapeFailures.Inc()
					slog.Error("cluster scrape failed", "err", err)
				}
			}
		}
	}()
}
