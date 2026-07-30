# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Prometheus exporter for vCenter capacity metrics (datastores and compute clusters), written in Go using govmomi. Serves metrics on `:2112/metrics`. Entire implementation lives in `main.go`; all tests in `main_test.go`.

## Commands

Recipes are in the `justfile`:

- `just test` — `go vet` + full test suite
- `just build` — build the `vsphere-exporter` binary
- `just run-mock` — run against the in-process vCenter simulator (no real vCenter needed)
- `just metrics` — curl the `vcenter_*` metrics from a running exporter
- `just fmt` / `just tidy` — gofmt / go mod tidy

Single test: `go test -run TestCollectClusterMetrics ./...`

Connection to a real vCenter comes from flags or env: `GOVC_URL`, `GOVC_USERNAME`, `GOVC_PASSWORD` (`GOVC_MOCKING=1` or `-mocking` for simulator mode).

## Architecture

Two independent background scrape loops started from `main()`, one per subsystem (datastores, clusters). Each subsystem follows the same pattern — replicate it for new subsystems:

1. A `xxxMetrics` struct + `newXxxMetrics(reg prometheus.Registerer)` constructor using `promauto.With(reg)`. The `Registerer` parameter is required: `main()` passes `prometheus.DefaultRegisterer`, tests pass a fresh `prometheus.NewRegistry()` to avoid duplicate-registration panics.
2. A pure, synchronous `collectXxxMetrics(ctx, *vim25.Client, metrics)` function doing one scrape pass — this is what tests call directly.
3. A `xxxMetricsLoop()` goroutine wrapper: immediate first collect, then ticker (`interval * time.Second`), errors increment a `..._scrape_failures_total` counter and never kill the loop.

Cluster collection specifics (the non-obvious business logic):

- "Available" = cluster total − largest host (HA failover reserve; cores and memory maxima tracked independently) − sum of VM allocations. May go negative on overcommit; do not clamp.
- VM allocation reads `vm.Summary.Config` (provisioned size), deliberately ignoring power state so powered-off VMs count. Templates and VMs with `NumCpu == 0` (unpopulated config) are skipped.
- Per-cluster errors are logged/counted and skip only that cluster, never the whole pass.
- Gauges are `Reset()` each pass (only after the cluster list retrieve succeeded) so removed/renamed clusters don't leave stale series.
- If any host lacks `Summary.Hardware`, the cores-total and available metrics for that cluster are skipped entirely rather than exporting silently wrong sums.

## Testing

Tests use govmomi's in-process simulator (`simulator.Test`, VPX model — same engine as the external vcsim binary, so no external binary is needed). VPX defaults: 1 datacenter, 1 cluster named `DC0_C0` with 3 hosts, VMs autostarted. Tests assert the exported arithmetic identity against independently recomputed simulator values.

`flag.Parse()` must stay in `main()`, not `init()` — in `init()` it runs before the test binary registers its own flags and breaks `go test`.
