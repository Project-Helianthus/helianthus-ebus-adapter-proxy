# helianthus-ebus-adapter-proxy

eBUS adapter proxy service with southbound transport drivers and northbound multi-session listeners.

## What is included

- Southbound drivers:
  - `internal/southbound/enh`: ENH dial/read/write lifecycle with reconnect hooks and timeout handling.
  - `internal/southbound/ens`: ENS dial/read/write lifecycle with reconnect hooks and timeout handling.
- Northbound listeners:
  - `internal/northbound/enh`: concurrent ENH listener sessions with metrics and lifecycle hooks.
  - `internal/northbound/ens`: concurrent ENS listener sessions with metrics and lifecycle hooks.
- Domain contracts and proxy orchestration in `internal/domain/*` and `internal/proxy`.
- Source-address policy and lease lifecycle components in `internal/sourcepolicy`.
- CI workflow that runs tests, vet, and terminology checks.
- Repository guardrail and architecture documents.

## Runtime shape (M3)

- One southbound owner connection to the physical adapter (ENH or ENS).
- Multiple concurrent northbound client sessions (ENH and ENS listeners).
- `internal/sourcepolicy.Policy` applies deterministic source-address selection filters before lease assignment.
- `internal/sourcepolicy.LeaseManager` enforces one active lease per owner and one owner per source address.
- Listener sessions decode transport frames and pass them to proxy domain handling.
- `internal/session.Manager` tracks session lifecycle, stable session identity, and bounded per-session queues.
- `internal/scheduler/write.AdaptiveScheduler` selects the next writer from queue-pressure candidates and applies starvation protection.

## Queue backpressure semantics (M2)

- Session queues are bounded via `internal/session.Options{InboundCapacity, OutboundCapacity}`.
- `EnqueueInbound` and `EnqueueOutbound` reject when a queue is full and return typed `BackpressureError` values.
- Rejections classify as `ErrInboundBackpressure` or `ErrOutboundBackpressure`, and still satisfy `errors.Is(err, ErrQueueFull)`.
- Backpressure outcomes are stable for callers: `errors.Is(err, ErrInboundBackpressure|ErrOutboundBackpressure|ErrQueueFull)`.
- Disconnect (`Unregister`) clears queued frames for that session and counts them as dropped.
- `internal/session.Session.QueueMetrics` and `internal/session.Manager.Metrics()` expose deterministic counters:
  `RejectedInbound`, `RejectedOutbound`, `DroppedInbound`, `DroppedOutbound`.

## Source-address policy and lease semantics (M3)

- Candidate addresses are normalized (sorted, unique) and invalid reserved values (`0x00`, `0xFF`) are dropped.
- Selection applies allow-list, block-list, in-use, and recent-activity filters in deterministic order.
- Default reservation mode is `soft` with `0x31` soft-reserved; the policy avoids soft-reserved addresses when alternatives exist.
- Soft-reserved candidates are still selectable when no alternatives remain, `ReservationMode` is `disabled`, or `AllowSoftReserved` is set.
- Recent-activity guard blocks addresses observed within the configured activity window for new leases; at the exact window boundary the address becomes eligible again.
- When every candidate is filtered only by recent activity, selection returns `ErrRecentlyActiveAddress`; otherwise exhaustion returns `ErrNoSourceAddressAvailable`.

## ebusd compatibility harness (M4)

- Goal: prove config-only migration for ebusd clients by switching only `host:port` from direct adapter endpoint to proxy endpoint.
- Scope: no ebusd code changes and no ebusd patch required.
- Command set: representative ENH-style requests (`req_init`, `req_start`, `req_info`, `req_send`) executed unchanged across both endpoints.
- Topology: deterministic local smoke path uses a mock adapter and local proxy endpoint, so CI does not need physical eBUS hardware.

Run:

```bash
./scripts/run-ebusd-compat-harness.sh
```

Smoke output format:

```text
Issue #14 ebusd compatibility harness
MODE: sim (local topology with mock adapter)
MIGRATION: config-only endpoint switch (host/port)
DIRECT_ENDPOINT=127.0.0.1:<port>
PROXY_ENDPOINT=127.0.0.1:<port>
PASS req_init request=0xC0 0x91 direct_response=0x80 0x91 proxy_response=0x80 0x91
...
RESULT: PASS (config-only migration verified; no ebusd patch required)
```

Failure shape:

```text
FAIL req_send request=... direct_response=... proxy_response=...
RESULT: FAIL (proxy responses diverge from direct endpoint)
```

## Terminology policy

- Use `initiator` and `target` across code and docs.
- If integration with an external protocol/source requires legacy wording, keep it scoped to that source and add a short clarification in the same context.
- Keep repository terminology gate-compliant (`./scripts/terminology-gate.sh`).

## Quick start

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
./scripts/terminology-gate.sh
```

## Repository documents

- `AGENTS.md`
- `ARCHITECTURE.md`
- `CONVENTIONS.md`
