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

- "Available" = total minus largest host (HA failover reserve, cores and memory maxima tracked independently) minus sum of VM allocations. May go negative on overcommit, do not clamp.
- Each available gauge derives from the same source as its exported total, so the identity `available == total - largest_host - allocated` holds across scraped values. CPU uses the host-core sum for both, memory uses the cluster summary `TotalMemory` for both.
- VM allocation reads `vm.Summary.Config` (provisioned size), deliberately ignoring power state so powered-off VMs count. Templates and VMs with `NumCpu == 0` (unpopulated config) are skipped.
- Per-cluster errors are accumulated and returned via `errors.Join`, so one broken cluster neither aborts the pass nor stays invisible to the loop.
- Gauges are `Reset()` each pass, but only after the cluster list retrieve succeeded, so removed clusters drop out while a vCenter outage keeps the last known good values.
- If any host lacks `Summary.Hardware`, the cores-total and available metrics for that cluster are skipped rather than exporting silently wrong sums. VM allocation gauges are still exported, they do not depend on host data.

## Testing

Tests use govmomi's in-process simulator (`simulator.Test`, VPX model, the same engine as the external vcsim binary, so no external binary is needed). VPX defaults: 1 datacenter, 1 cluster named `DC0_C0` with 3 hosts, VMs autostarted. Assertions check the available-value identity against the other exported gauges, recomputing only the largest-host value independently.

`flag.Parse()` must stay in `main()`, not `init()`. In `init()` it runs before the test binary registers its own flags and breaks `go test`.
