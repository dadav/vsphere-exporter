# vsphere-exporter

Prometheus exporter for vCenter capacity metrics: datastores and compute clusters.
Metrics are served on `:2112/metrics`.

## Usage

```sh
GOVC_URL=https://vcenter.example.com/sdk \
GOVC_USERNAME=monitoring \
GOVC_PASSWORD=secret \
./vsphere-exporter -interval 30
```

Flags (each falls back to the env var): `-url` (`GOVC_URL`), `-username` (`GOVC_USERNAME`),
`-password` (`GOVC_PASSWORD`), `-interval` (seconds), `-mocking` (`GOVC_MOCKING=1`,
runs against an in-process vCenter simulator for local testing),
`-exclude-vm-folders` (`GOVC_EXCLUDE_VM_FOLDERS`), and `-debug`
(`GOVC_DEBUG=1`).

### Debugging cluster calculations

Use `-debug` or `GOVC_DEBUG=1` to run one cluster collection, print structured
calculation diagnostics to stderr, and exit:

```sh
./vsphere-exporter -debug
```

Debug mode does not start the background collection loops, open port `2112`, or
expose metrics. It prints one decision for every VM in each cluster, including
whether the VM was counted and why it was skipped. It also prints the complete
CPU and memory calculation with cluster totals, largest-host HA reserves, VM
allocations, and available capacity. `-exclude-vm-folders` is honoured, and
powered-off VMs are shown and counted normally.

### Excluding VMs by folder

Site Recovery Manager placeholder VMs are recovery stubs that reserve no real
capacity, but vSphere does not mark them as templates, so by default they inflate
the allocation metrics. Pass the folder names they live in to leave them out:

```sh
./vsphere-exporter -exclude-vm-folders 'IT-Notfall'
```

The value is a comma separated list of folder *names*, matched across the whole
inventory. A VM is skipped when it sits in a matching folder or in any subfolder
below one. Names that match no folder are logged as a warning and otherwise
ignored, so a renamed or not-yet-created folder never breaks a scrape.

Development commands are in the `justfile` (`just test`, `just build`, `just run-mock`, `just metrics`).

## Metrics

| Metric | Description |
|---|---|
| `vcenter_cluster_hosts_total` | Hosts in the cluster |
| `vcenter_cluster_hosts_effective` | Connected, non-maintenance hosts |
| `vcenter_cluster_cpu_cores_total` | Physical CPU cores of all hosts |
| `vcenter_cluster_cpu_mhz_total` | Total CPU capacity in MHz |
| `vcenter_cluster_memory_bytes_total` | Total memory capacity |
| `vcenter_cluster_vm_cpu_cores_allocated` | Sum of configured vCPUs of all VMs, powered-off included, excluded folders left out |
| `vcenter_cluster_vm_memory_bytes_allocated` | Sum of configured VM memory, powered-off included, excluded folders left out |
| `vcenter_cluster_cpu_cores_available` | Cores left after HA reserve and VM allocation |
| `vcenter_cluster_memory_bytes_available` | Memory left after HA reserve and VM allocation |
| `vcenter_datastore_capacity_bytes` | Datastore capacity |
| `vcenter_datastore_free_bytes` | Datastore free space |
| `vcenter_cluster_scrape_failures_total`, `vcenter_datastore_scrape_failures_total` | Scrape error counters |

Cluster metrics are labelled with `name` (cluster name), datastore metrics with `name` and `url`.

The `*_available` metrics already implement the capacity formula, no PromQL math needed:

```
available = cluster total - largest host (reserved for HA failover) - sum of VM allocations
```

VM allocations come from the VM config, so powered-off VMs are included.
Available values go negative when the cluster is overcommitted past the HA reserve.

## Example queries (Grafana)

Remaining CPU cores and memory per cluster (HA reserve already subtracted):

```promql
vcenter_cluster_cpu_cores_available
vcenter_cluster_memory_bytes_available
```

Remaining memory as a percentage of cluster capacity:

```promql
100 * vcenter_cluster_memory_bytes_available / vcenter_cluster_memory_bytes_total
```

CPU overcommit ratio (allocated vCPUs per physical core, > 1 means overcommitted):

```promql
vcenter_cluster_vm_cpu_cores_allocated / vcenter_cluster_cpu_cores_total
```

How many more VMs of a given size still fit (example: 4 vCPUs, 16 GiB), limited by
whichever resource runs out first:

```promql
clamp_min(
  (
      floor(vcenter_cluster_cpu_cores_available / 4)
   <= floor(vcenter_cluster_memory_bytes_available / (16 * 1024 * 1024 * 1024))
  )
  or floor(vcenter_cluster_memory_bytes_available / (16 * 1024 * 1024 * 1024)),
0)
```

(The `<= ... or ...` construct is the PromQL idiom for the element-wise minimum of two vectors.)

Datastore usage percentage:

```promql
100 * (1 - vcenter_datastore_free_bytes / vcenter_datastore_capacity_bytes)
```

Alert on scrape failures:

```promql
increase(vcenter_cluster_scrape_failures_total[15m]) > 0
```
