# Architecture

## Goal

`helianthus-ebus-adapter-proxy` owns one southbound adapter connection and exposes concurrent northbound listener sessions.

## Current package layout

- `cmd/helianthus-ebus-adapter-proxy`: executable entry point.
- `internal/southbound/enh`: ENH southbound transport codec and driver.
- `internal/southbound/ens`: ENS southbound transport codec and driver.
- `internal/northbound/enh`: ENH northbound multi-session listener.
- `internal/northbound/ens`: ENS northbound multi-session listener.
- `internal/session`: session registry, lifecycle hooks, and bounded per-session queues.
- `internal/scheduler/write`: adaptive write scheduler with starvation guard.
- `internal/sourcepolicy`: source-address selection policy, recent-activity guard, and owner lease lifecycle.
- `internal/domain/downstream`: downstream frame contracts and client interface.
- `internal/domain/upstream`: upstream message contracts and client interface.
- `internal/domain/routing`: routing contract between downstream and upstream domains.
- `internal/proxy`: service orchestration for routing and publication.

## Runtime path (M3)

1. Initialize one southbound owner connection (ENH or ENS) to the adapter.
2. Start northbound listeners (ENH and/or ENS) that accept concurrent client sessions.
3. Acquire source-address leases through `internal/sourcepolicy.LeaseManager` using deterministic `Policy` selection.
4. Register client sessions in `internal/session.Manager` with stable IDs and bounded inbound/outbound queues.
5. Decode per-session transport frames and enqueue/dequeue `downstream.Frame` values per session.
6. Build scheduler candidates from per-session queue depth and choose next writer via `AdaptiveScheduler`.
7. Route selected frames through domain/proxy orchestration toward upstream publication.

## Session behavior

- Northbound listeners expose deterministic lifecycle hooks: connect, disconnect, error.
- Metrics snapshots include active sessions plus total connection/disconnect/error/frame counters.
- Malformed transport data is reported and parsing continues when recovery is possible.

## Backpressure and overflow behavior

- Queue limits are configured through `session.Options{InboundCapacity, OutboundCapacity}`.
- Full-queue enqueue operations reject with `BackpressureError` and stable sentinel matching:
  `ErrInboundBackpressure` or `ErrOutboundBackpressure`, and `ErrQueueFull`.
- `Unregister` clears per-session queue contents; cleared entries are counted as dropped frames.
- Stable queue metric counters are exposed both per session (`Session.QueueMetrics`) and aggregated (`Manager.Metrics()`):
  `RejectedInbound`, `RejectedOutbound`, `DroppedInbound`, `DroppedOutbound`.

## Scheduler behavior

- `AdaptiveScheduler` prioritizes queue pressure (`QueueDepth`) and uses deterministic tie-breaking.
- Starvation guard (`Options.StarvationAfter`) forces service for stale sessions under sustained skewed load.

## Source-address policy behavior

- `Policy` normalizes candidate source addresses (sorted, unique, filtered for `0x00`/`0xFF`) before selection.
- Selection filters candidates by allow-list, block-list, in-use addresses, and optional recent-activity guard.
- In `soft` reservation mode, soft-reserved addresses (default includes `0x31`) are avoided unless no alternatives remain.
- `AllowSoftReserved` or `ReservationMode=disabled` allows direct selection of soft-reserved candidates.
- If all candidates are filtered by recent activity, selection returns `ErrRecentlyActiveAddress`; otherwise it returns `ErrNoSourceAddressAvailable`.

## Lease lifecycle and conflict outcomes

- Lease lifecycle operations are `Acquire`, `Renew`, `Release`, and `Expire`, serialized under one manager mutex.
- `Acquire` rejects empty owner IDs, removes expired leases first, then creates a lease with `AcquiredAt`, `RenewedAt`, and `ExpiresAt`.
- `Renew` extends `ExpiresAt` for an active lease; `Release` removes owner/address ownership mappings.
- `Expire` removes leases where `ExpiresAt <= now` and returns expired leases sorted by address, then owner ID.
- Conflict outcomes are stable contract codes: `source_address_lease.address_in_use`, `source_address_lease.owner_already_has_lease`, `source_address_lease.owner_lease_missing`, and `source_address_lease.owner_lease_expired`.

## Terminology policy

- Runtime and protocol roles are documented as `initiator` and `target`.
- Legacy wording may appear only when mirroring external protocol definitions, with local clarification.
