# Architecture

## Goal

`helianthus-ebus-adapter-proxy` owns one southbound adapter connection and exposes concurrent northbound listener sessions.

## Current package layout

- `cmd/helianthus-ebus-adapter-proxy`: executable entry point.
- `internal/southbound/enh`: ENH southbound transport codec and driver.
- `internal/southbound/ens`: ENS southbound transport codec and driver.
- `internal/northbound/enh`: ENH northbound multi-session listener.
- `internal/northbound/ens`: ENS northbound multi-session listener.
- `internal/domain/downstream`: downstream frame contracts and client interface.
- `internal/domain/upstream`: upstream message contracts and client interface.
- `internal/domain/routing`: routing contract between downstream and upstream domains.
- `internal/proxy`: service orchestration for routing and publication.

## Runtime path (M1)

1. Initialize one southbound owner connection (ENH or ENS) to the adapter.
2. Start northbound listeners (ENH and/or ENS) that accept concurrent client sessions.
3. Decode per-session transport frames and convert them into `downstream.Frame` values.
4. Route frames through domain/proxy orchestration toward upstream publication.

## Session behavior

- Northbound listeners expose deterministic lifecycle hooks: connect, disconnect, error.
- Metrics snapshots include active sessions plus total connection/disconnect/error/frame counters.
- Malformed transport data is reported and parsing continues when recovery is possible.

## Terminology policy

- Runtime and protocol roles are documented as `initiator` and `target`.
- Legacy wording may appear only when mirroring external protocol definitions, with local clarification.
