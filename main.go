package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/property"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

var (
	vcUsername    string
	vcPassword    string
	vcUrl         string
	enableMocking bool
	fetchInterval int64
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

func init() {
	flag.StringVar(&vcUsername, "username", os.Getenv("GOVC_USERNAME"), "Username for vCenter connection")
	flag.StringVar(&vcPassword, "password", os.Getenv("GOVC_PASSWORD"), "Password for vCenter connection")
	flag.StringVar(&vcUrl, "url", os.Getenv("GOVC_URL"), "URL for vCenter connection")
	flag.Int64Var(&fetchInterval, "interval", 30, "Interval in seconds in which data will be fetched")
	flag.BoolVar(&enableMocking, "mocking", os.Getenv("GOVC_MOCKING") == "1", "Enable vCenter mocking")
}

func newVCenterClient(ctx context.Context) (*govmomi.Client, error) {
	if enableMocking {
		return newMockClient(ctx)
	}
	return newRealClient(ctx)
}

func newMockClient(ctx context.Context) (*govmomi.Client, error) {
	// VPX (and not ESX) because the ESX model has no compute clusters.
	model := simulator.VPX()
	if err := model.Create(); err != nil {
		return nil, err
	}

	s := model.Service.NewServer()
	u, _ := url.Parse(s.URL.String())
	u.User = url.UserPassword("user", "pass")
	return govmomi.NewClient(ctx, u, false)
}

func newRealClient(ctx context.Context) (*govmomi.Client, error) {
	if vcUrl == "" || vcUsername == "" || vcPassword == "" {
		return nil, fmt.Errorf("GOVC_URL, GOVC_USERNAME, and GOVC_PASSWORD must be set")
	}
	u, err := url.Parse(vcUrl)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(vcUsername, vcPassword)
	return govmomi.NewClient(ctx, u, false)
}

func collectDatastoreMetrics(ctx context.Context, v *view.ContainerView, metrics *datastoreMetrics) error {
	var datastores []mo.Datastore
	if err := v.Retrieve(ctx, []string{"Datastore"}, []string{"summary"}, &datastores); err != nil {
		return err
	}

	for _, ds := range datastores {
		metrics.capacity.WithLabelValues(ds.Summary.Url, ds.Summary.Name).Set(float64(ds.Summary.Capacity))
		metrics.free.WithLabelValues(ds.Summary.Url, ds.Summary.Name).Set(float64(ds.Summary.FreeSpace))
	}
	return nil
}

func metricsLoop(ctx context.Context, c *vim25.Client, interval int64, metrics *datastoreMetrics) {
	go func() {
		m := view.NewManager(c)

		v, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"Datastore"}, true)
		if err != nil {
			log.Fatalf("Error creating container view: %v\n", err)
		}
		defer v.Destroy(ctx)

		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()

		metrics.scrapeFailures.Add(0)

		if err := collectDatastoreMetrics(ctx, v, metrics); err != nil {
			metrics.scrapeFailures.Inc()
			log.Printf("Error retrieving datastores: %v\n", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := collectDatastoreMetrics(ctx, v, metrics); err != nil {
					metrics.scrapeFailures.Inc()
					log.Printf("Error retrieving datastores: %v\n", err)
				}
			}
		}
	}()
}

// collectClusterMetrics does a single scrape pass over all compute clusters.
//
// Available resources are computed as:
//
//	total - largest host (reserved for HA failover) - sum of all VM allocations
//
// VM allocations come from the VM config and therefore include powered off VMs.
// Available values may be negative when the cluster is overcommitted.
func collectClusterMetrics(ctx context.Context, c *vim25.Client, metrics *clusterMetrics) error {
	m := view.NewManager(c)

	cv, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"ClusterComputeResource"}, true)
	if err != nil {
		return fmt.Errorf("creating cluster container view: %w", err)
	}
	defer cv.Destroy(ctx)

	var clusters []mo.ClusterComputeResource
	if err := cv.Retrieve(ctx, []string{"ClusterComputeResource"}, []string{"name", "summary", "host"}, &clusters); err != nil {
		return fmt.Errorf("retrieving clusters: %w", err)
	}

	pc := property.DefaultCollector(c)

	// Reset only after the cluster list was retrieved successfully, so a
	// failing vCenter connection does not wipe the last known good values.
	metrics.resetGauges()

	// Errors inside a single cluster are logged and counted but do not abort
	// the pass, so one broken cluster cannot starve the others.
	for _, cluster := range clusters {
		if err := collectOneCluster(ctx, m, pc, cluster, metrics); err != nil {
			metrics.scrapeFailures.Inc()
			log.Printf("Error collecting metrics of cluster %q: %v\n", cluster.Name, err)
		}
	}

	return nil
}

func collectOneCluster(ctx context.Context, m *view.Manager, pc *property.Collector, cluster mo.ClusterComputeResource, metrics *clusterMetrics) error {
	name := cluster.Name

	if cluster.Summary != nil {
		summary := cluster.Summary.GetComputeResourceSummary()
		metrics.hostsTotal.WithLabelValues(name).Set(float64(summary.NumHosts))
		metrics.hostsEffective.WithLabelValues(name).Set(float64(summary.NumEffectiveHosts))
		metrics.cpuMhzTotal.WithLabelValues(name).Set(float64(summary.TotalCpu))
		metrics.memoryTotal.WithLabelValues(name).Set(float64(summary.TotalMemory))
	} else {
		log.Printf("Cluster %q has no summary, skipping capacity totals\n", name)
	}

	if len(cluster.Host) == 0 {
		log.Printf("Cluster %q has no hosts, skipping available resources\n", name)
		return nil
	}

	vmCores, vmMemory, err := clusterVmAllocation(ctx, m, cluster.Reference())
	if err != nil {
		return fmt.Errorf("retrieving vms: %w", err)
	}
	metrics.vmCpuCores.WithLabelValues(name).Set(float64(vmCores))
	metrics.vmMemory.WithLabelValues(name).Set(float64(vmMemory))

	var hosts []mo.HostSystem
	if err := pc.Retrieve(ctx, cluster.Host, []string{"summary.hardware"}, &hosts); err != nil {
		return fmt.Errorf("retrieving hosts: %w", err)
	}

	// Largest host by cores and largest host by memory are tracked
	// independently, which is the conservative HA reserve for
	// heterogeneous clusters.
	var totalCores, maxHostCores int64
	var totalMemory, maxHostMemory int64
	for _, host := range hosts {
		if host.Summary.Hardware == nil {
			// Incomplete hardware data (e.g. disconnected host) would make
			// the core and available values silently wrong, so skip them
			// entirely for this cluster instead of exporting bad numbers.
			log.Printf("Host %q in cluster %q has no hardware summary, skipping core and available metrics\n", host.Reference().Value, name)
			return nil
		}
		cores := int64(host.Summary.Hardware.NumCpuCores)
		memory := host.Summary.Hardware.MemorySize

		totalCores += cores
		totalMemory += memory
		if cores > maxHostCores {
			maxHostCores = cores
		}
		if memory > maxHostMemory {
			maxHostMemory = memory
		}
	}
	metrics.cpuCoresTotal.WithLabelValues(name).Set(float64(totalCores))

	metrics.cpuCoresAvail.WithLabelValues(name).Set(float64(totalCores - maxHostCores - vmCores))
	metrics.memoryAvail.WithLabelValues(name).Set(float64(totalMemory - maxHostMemory - vmMemory))

	return nil
}

// clusterVmAllocation sums the configured vCPUs and memory (bytes) of all
// virtual machines inside the given cluster. Templates and VMs without a
// populated config are skipped. Power state is deliberately ignored, so
// powered off VMs count towards the allocation.
func clusterVmAllocation(ctx context.Context, m *view.Manager, cluster types.ManagedObjectReference) (int64, int64, error) {
	vv, err := m.CreateContainerView(ctx, cluster, []string{"VirtualMachine"}, true)
	if err != nil {
		return 0, 0, fmt.Errorf("creating vm container view: %w", err)
	}
	defer vv.Destroy(ctx)

	var vms []mo.VirtualMachine
	if err := vv.Retrieve(ctx, []string{"VirtualMachine"}, []string{"summary.config"}, &vms); err != nil {
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

func clusterMetricsLoop(ctx context.Context, c *vim25.Client, interval int64, metrics *clusterMetrics) {
	go func() {
		ticker := time.NewTicker(time.Duration(interval) * time.Second)
		defer ticker.Stop()

		metrics.scrapeFailures.Add(0)

		if err := collectClusterMetrics(ctx, c, metrics); err != nil {
			metrics.scrapeFailures.Inc()
			log.Printf("Error collecting cluster metrics: %v\n", err)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := collectClusterMetrics(ctx, c, metrics); err != nil {
					metrics.scrapeFailures.Inc()
					log.Printf("Error collecting cluster metrics: %v\n", err)
				}
			}
		}
	}()
}

func main() {
	// Parsed here and not in init() so that the test binary can register its
	// own flags first.
	flag.Parse()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := newVCenterClient(ctx)
	if err != nil {
		log.Fatalf("Error while creating client: %v\n", err)
	}

	if fetchInterval <= 0 {
		log.Println("Invalid fetch interval, using default 30s")
		fetchInterval = 30
	}

	metricsLoop(ctx, c.Client, fetchInterval, newDatastoreMetrics(prometheus.DefaultRegisterer))
	clusterMetricsLoop(ctx, c.Client, fetchInterval, newClusterMetrics(prometheus.DefaultRegisterer))

	http.Handle("/metrics", promhttp.Handler())
	fmt.Println("Listening on :2112")
	if err := http.ListenAndServe(":2112", nil); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
