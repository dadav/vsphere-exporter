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

### SRM placeholder VMs

Site Recovery Manager placeholder VMs are excluded automatically when vSphere
reports `summary.config.managedBy.extensionKey` as `com.vmware.vcDR` and its
type as `placeholderVm`. SRM recovery-test VMs with type `testVm` remain
included because they can consume cluster capacity. No configuration is needed.

### Excluding VMs by folder

Use the folder exclusion as an additional override for VMs that should not count
towards allocation metrics but are not identifiable as SRM placeholders:

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
| `vcenter_cluster_cpu_physical_cores_total` | Physical CPU cores of all hosts |
| `vcenter_cluster_cpu_threads_total` | Logical CPU threads of all hosts |
| `vcenter_cluster_cpu_threads_ha_reserved` | Logical CPU threads on the largest host, reserved by the exporter for single-host HA failover |
| `vcenter_cluster_cpu_mhz_total` | Total CPU capacity in MHz |
| `vcenter_cluster_memory_bytes_total` | Total memory capacity |
| `vcenter_cluster_memory_bytes_ha_reserved` | Memory on the largest-memory host, reserved by the exporter for single-host HA failover |
| `vcenter_cluster_vm_vcpus_allocated` | Sum of configured vCPUs of all VMs, powered-off included, SRM placeholders and excluded folders left out |
| `vcenter_cluster_vm_memory_bytes_allocated` | Sum of configured VM memory, powered-off included, SRM placeholders and excluded folders left out |
| `vcenter_cluster_cpu_threads_available` | Logical CPU threads left after HA reserve and VM allocation |
| `vcenter_cluster_memory_bytes_available` | Memory left after HA reserve and VM allocation |
| `vcenter_cluster_datastore_info` | Cluster-to-datastore mapping, with a value of `1` for each relationship |
| `vcenter_datastore_capacity_bytes` | Datastore capacity |
| `vcenter_datastore_free_bytes` | Datastore free space |
| `vcenter_cluster_scrape_failures_total`, `vcenter_datastore_scrape_failures_total` | Scrape error counters |

Cluster metrics are labelled with `name` (cluster name), datastore metrics with `name` and `url`.
`vcenter_cluster_datastore_info` uses `cluster`, `name`, and `url`, where `name` and `url`
match the datastore metrics exactly.

CPU metric migration: replace `vcenter_cluster_cpu_cores_total` with
`vcenter_cluster_cpu_physical_cores_total`, `vcenter_cluster_vm_cpu_cores_allocated`
with `vcenter_cluster_vm_vcpus_allocated`, and `vcenter_cluster_cpu_cores_available`
with `vcenter_cluster_cpu_threads_available`. The old names are no longer exported.

The logical CPU thread and memory availability metrics implement the same capacity formula that can now be reproduced from the exported gauges:

```
CPU threads available = total host threads - HA-reserved threads - allocated vCPUs
memory available = total memory - HA-reserved memory - allocated VM memory
```

The CPU and memory HA reserves are calculated independently, so heterogeneous
clusters may use different largest hosts for the two values. They are the
exporter's single-host failover inputs, not vCenter admission-control policy
percentages.

VM allocations come from the VM config, so powered-off VMs are included.
Available values go negative when the cluster is overcommitted past the HA reserve.

## Example queries (Grafana)

Remaining logical CPU threads and memory per cluster (HA reserve already subtracted):

```promql
vcenter_cluster_cpu_threads_available
vcenter_cluster_memory_bytes_available
```

Calculate the same values explicitly in Grafana:

```promql
vcenter_cluster_cpu_threads_total
  - vcenter_cluster_cpu_threads_ha_reserved
  - vcenter_cluster_vm_vcpus_allocated

vcenter_cluster_memory_bytes_total
  - vcenter_cluster_memory_bytes_ha_reserved
  - vcenter_cluster_vm_memory_bytes_allocated
```

Remaining memory as a percentage of cluster capacity:

```promql
100 * vcenter_cluster_memory_bytes_available / vcenter_cluster_memory_bytes_total
```

Logical CPU thread allocation ratio (allocated vCPUs per host thread):

```promql
vcenter_cluster_vm_vcpus_allocated / vcenter_cluster_cpu_threads_total
```

Physical-core overcommit ratio (allocated vCPUs per physical core):

```promql
vcenter_cluster_vm_vcpus_allocated / vcenter_cluster_cpu_physical_cores_total
```

How many more VMs of a given size still fit (example: 4 vCPUs, 16 GiB), limited by
whichever resource runs out first:

```promql
clamp_min(
  (
      floor(vcenter_cluster_cpu_threads_available / 4)
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

Add the compute cluster label to datastore metrics:

```promql
vcenter_datastore_free_bytes
  * on(name, url) group_right
    vcenter_cluster_datastore_info
```

For a datastore shared by multiple compute clusters, this returns one series per
cluster-datastore relationship.

Alert on scrape failures:

```promql
increase(vcenter_cluster_scrape_failures_total[15m]) > 0
```
