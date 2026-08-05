# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Prometheus exporter for vCenter capacity metrics (datastores and compute clusters), written in Go using govmomi. Serves metrics on `:2112/metrics`.

## Commands

Recipes are in the `justfile`:

- `just test`: `go vet` plus full test suite
- `just build`: build the `vsphere-exporter` binary
- `just run-mock`: run against the in-process vCenter simulator (no real vCenter needed)
- `just metrics`: curl the `vcenter_*` metrics from a running exporter
- `just fmt` / `just tidy`: gofmt / go mod tidy

Single test: `go test -run TestCollectClusterMetrics ./...`

Connection to a real vCenter comes from flags or env: `GOVC_URL`, `GOVC_USERNAME`, `GOVC_PASSWORD` (`GOVC_MOCKING=1` or `-mocking` for simulator mode).

`-debug` (`GOVC_DEBUG=1`, `just run-debug` against the simulator) is a one-shot
cluster calculation mode. It uses an isolated Prometheus registry, emits
structured VM decisions and capacity formula inputs, then exits without starting
scrape loops or an HTTP listener. Keep its diagnostics on the same collection
path as the exported cluster metrics. Every field of the capacity record is
filled in where its value is computed (`clusterCalculation` in `cluster.go`), so
an aborted calculation still reports the values that were exported instead of
placeholder zeros, and a debug record never contradicts a gauge.

## Layout

Flat package `main`, one file per feature so each can be regenerated on its own:

- `main.go`: flags, `main()`, HTTP wiring
- `client.go`: vCenter client construction (real and simulator)
- `datastore.go` / `cluster.go`: one metrics subsystem each
- `*_test.go` mirror the source files, shared test helpers live in `helpers_test.go`

## Architecture

Two independent background scrape loops started from `main()`, one per subsystem. Each subsystem follows the same three-part pattern, replicate it for new subsystems:

1. A `xxxMetrics` struct plus `newXxxMetrics(reg prometheus.Registerer)` constructor using `promauto.With(reg)`. The `Registerer` parameter is required: `main()` passes `prometheus.DefaultRegisterer`, tests pass a fresh `prometheus.NewRegistry()` to avoid duplicate-registration panics.
2. A pure, synchronous `collectXxxMetrics(ctx, *vim25.Client, metrics)` function doing one scrape pass. This is what tests call directly. It never touches the failure counter and never logs scrape errors, it returns them.
3. A `xxxMetricsLoop()` goroutine wrapper: immediate first collect, then ticker (`interval * time.Second`). It owns error handling, incrementing `..._scrape_failures_total` and logging once per failed pass. Errors never kill the loop.

Logging is `log/slog` with stable messages and explicit fields (`slog.Warn("cluster has no hosts, skipping available resources", "cluster", name)`). No prose-formatted `log.Printf`.

Cluster collection specifics, the non-obvious business logic:

- Logical CPU availability = sum of host `NumCpuThreads` minus the largest host thread count (HA failover reserve) minus configured VM vCPUs. Physical core, logical thread, and memory maxima are tracked independently because different hosts can be largest for each resource. Values may go negative on overcommit, do not clamp.
- Each supported available gauge derives from the same source as its exported total, so the identity `available == total - largest_host - allocated` holds across scraped values. Logical CPU uses the host-thread sum, memory uses the cluster summary `TotalMemory`.
- `vcenter_cluster_cpu_cores_total` remains the physical core count. `vcenter_cluster_vm_cpu_cores_allocated` and `vcenter_cluster_cpu_cores_available` are deprecated compatibility metrics whose existing values must not change; the latter deliberately preserves its legacy mixed core/vCPU calculation.
- VM allocation reads `vm.Summary.Config` (provisioned size), deliberately ignoring power state so powered-off VMs count. Templates and VMs with `NumCpu == 0` (unpopulated config) are skipped. The allocation is exported under `vcenter_cluster_vm_vcpus_allocated` and its deprecated alias from the same computed value.
- `-exclude-vm-folders` (`GOVC_EXCLUDE_VM_FOLDERS`, comma separated folder *names*) additionally skips VMs inside a matching folder **or any subfolder below one**, for SRM placeholder VMs which are not flagged as templates. Names are resolved once per pass via one `Folder` container view retrieving `name` and `parent`; the matched refs are expanded to all descendant folders, so the VM-side check stays a direct-parent lookup. An empty list short-circuits before the folder view, so the default path costs nothing. A configured name matching no folder warns and continues, it is not a scrape error. Note a VM's `parent` is its folder while cluster membership comes from the resource pool, so an excluded folder does not hide the VM from the per-cluster container view.
- Per-cluster errors are accumulated and returned via `errors.Join`, so one broken cluster neither aborts the pass nor stays invisible to the loop.
- Gauges are `Reset()` each pass, but only after the cluster list retrieve succeeded, so removed clusters drop out while a vCenter outage keeps the last known good values.
- If any host lacks `Summary.Hardware`, the core, thread, and available metrics for that cluster are skipped rather than exporting silently wrong sums. VM allocation gauges are still exported, they do not depend on host data.

## Testing

Tests use govmomi's in-process simulator (`simulator.Test`, VPX model, the same engine as the external vcsim binary, so no external binary is needed). VPX defaults: 1 datacenter, 1 cluster named `DC0_C0` with 3 hosts, VMs autostarted. Assertions check the available-value identity against the other exported gauges, recomputing only the largest-host value independently. The default simulator has equal core and thread counts, so the pure host-capacity test must use synthetic hosts with different values to prove the distinction.

`flag.Parse()` must stay in `main()`, not `init()`. In `init()` it runs before the test binary registers its own flags and breaks `go test`.
