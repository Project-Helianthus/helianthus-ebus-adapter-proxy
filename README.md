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
- Emulated target profile registry in `internal/emulation/targets` with runtime enable/disable and deterministic route selection.
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

## M4 integration path map (#14/#15/#16/#17)

- Issue #14 (`scripts/run-ebusd-compat-harness.sh`): validates config-only ebusd migration and emits `RESULT: PASS|FAIL` in `.verify/issue14/ebusd-compat-harness.log`.
- Issue #15 (`scripts/run-gateway-direct-proxy-smoke.sh`): validates gateway `enh://`/`ens://` direct-proxy transport and emits `PASS|FAIL: gateway path readiness profile=...`.
- Issue #16 (HA add-on linkage): aligns `proxy_profile` + `proxy_endpoint` semantics with `../helianthus-ha-addon/README.md` and `../helianthus-ha-addon/SMOKE_RUNBOOK.md`.
- Issue #17 (`scripts/run-ha-integration-dual-topology-smoke.sh`): validates coexistence (`ebusd` + adapter-proxy) and requires `CHECK_DUAL_TOPOLOGY_PATH` readiness markers.

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

## Gateway direct proxy profile assets (M4, issue #15)

- Dependency: `d3vi1/helianthus-ebusgateway#92` merged into `main` (endpoint-URI transport profile support for `enh://` and `ens://`).
- Smoke profile templates for gateway live in:
  - `profiles/gateway-direct-proxy/agent-local.enh.md`
  - `profiles/gateway-direct-proxy/agent-local.ens.md`
- HA add-on profile linkage details are tracked in issue #16 section below.
- Cross-repo smoke runner:
  - `scripts/run-gateway-direct-proxy-smoke.sh`
- Dual-topology smoke notes:
  - Keep `ebusd` on its direct adapter path and validate gateway direct-to-proxy with two runs (`enh` then `ens`).
  - Keep source-address separation between topologies (`ebusd` on `0x31`, gateway smoke on dedicated addresses like `0xF0`/`0xF1`).
  - HA add-on smoke checklist proxy markers must match:
    - `Proxy profile: <disabled|enh|ens>`
    - `Proxy endpoint: <profile>://<host>:<port>` when `proxy_profile` is `enh`/`ens`
    - `Proxy endpoint: (none)` when `proxy_profile` is `disabled`

Cross-repo verification steps against `helianthus-ebusgateway` `main`:

```bash
cd ../helianthus-ebusgateway
git checkout main
git pull --ff-only origin main
cd ../helianthus-ebus-adapter-proxy
```

ENH direct proxy smoke:

```bash
./scripts/run-gateway-direct-proxy-smoke.sh \
  --gateway-repo ../helianthus-ebusgateway \
  --profile enh \
  --proxy-host 127.0.0.1 \
  --proxy-port 19001 \
  --source-address 0xF0
```

ENS direct proxy smoke:

```bash
./scripts/run-gateway-direct-proxy-smoke.sh \
  --gateway-repo ../helianthus-ebusgateway \
  --profile ens \
  --proxy-host 127.0.0.1 \
  --proxy-port 19002 \
  --source-address 0xF1
```

Expected result for each run:

```text
PASS: gateway smoke profile <enh|ens> completed against 127.0.0.1:<port>
PASS: gateway path readiness profile=<enh|ens> endpoint=<enh|ens>://127.0.0.1:<port>
```

Deterministic failure shape for gateway path readiness:

```text
FAIL: gateway path readiness profile=<enh|ens> endpoint=<enh|ens>://127.0.0.1:<port> (see <repo>/.verify/issue15/gateway-smoke-<profile>.log)
```

## HA add-on proxy profile linkage (M4, issue #16)

- Coordination target: `d3vi1/helianthus-ha-addon#30` / PR `#31`.
- Cross-repo profile configuration and smoke runbook live in:
  - `../helianthus-ha-addon/README.md`
  - `../helianthus-ha-addon/SMOKE_RUNBOOK.md`
- Deterministic checklist markers must include:
  - `[PASS] CHECK_LOG_PROXY_PROFILE :: ...`
  - `[PASS] CHECK_LOG_PROXY_ENDPOINT :: ...`

Cross-repo setup:

```bash
cd ../helianthus-ha-addon
git checkout main
git pull --ff-only origin main
cd ../helianthus-ebus-adapter-proxy
```

HA add-on proxy marker checklist (ENH example):

```bash
cd ../helianthus-ha-addon
python3 scripts/smoke_addon_checklist.py \
  --log-file /tmp/helianthus-addon.log \
  --transport enh \
  --network tcp \
  --address 192.168.100.2:9999 \
  --proxy-profile enh \
  --proxy-endpoint 127.0.0.1:19001
```

Expected proxy marker checks in checklist output:

```text
[PASS] CHECK_LOG_PROXY_PROFILE :: ...
[PASS] CHECK_LOG_PROXY_ENDPOINT :: ...
```

## HA integration dual-topology smoke path (M4, issue #17)

- Coordination target: `d3vi1/helianthus-ha-integration#58` / PR `#59`.
- Cross-repo smoke runner:
  - `scripts/run-ha-integration-dual-topology-smoke.sh`
- The runner executes HA integration dual-topology smoke (`ebusd` endpoint + adapter-proxy endpoint) and checks deterministic marker `CHECK_DUAL_TOPOLOGY_PATH`.
- Gateway readiness is reported with deterministic marker lines:
  - `PASS: gateway readiness dual-topology path ...`
  - `FAIL: gateway readiness dual-topology path ...`

Cross-repo setup:

```bash
cd ../helianthus-ha-integration
git fetch origin
git checkout main
git pull --ff-only origin main
cd ../helianthus-ebus-adapter-proxy
```

If PR `#59` is not merged yet, use the feature branch with `--allow-non-main`:

```bash
cd ../helianthus-ha-integration
git fetch origin issue-58-ha-dual-topology-smoke
git checkout issue-58-ha-dual-topology-smoke
cd ../helianthus-ebus-adapter-proxy
```

ENH dual-topology smoke:

```bash
./scripts/run-ha-integration-dual-topology-smoke.sh \
  --ha-repo ../helianthus-ha-integration \
  --allow-non-main \
  --proxy-profile enh \
  --proxy-port 19001 \
  --ebusd-host 127.0.0.1 \
  --ebusd-port 8888
```

ENS dual-topology smoke:

```bash
./scripts/run-ha-integration-dual-topology-smoke.sh \
  --ha-repo ../helianthus-ha-integration \
  --allow-non-main \
  --proxy-profile ens \
  --proxy-port 19002 \
  --ebusd-host 127.0.0.1 \
  --ebusd-port 8888
```

Expected gateway-readiness markers:

```text
[PASS] CHECK_DUAL_TOPOLOGY_PATH :: mode=coexistence_ready ...
PASS: gateway readiness dual-topology path ebusd_endpoint=tcp://127.0.0.1:8888 proxy_endpoint=<enh|ens>://127.0.0.1:<port>
PASS: ha integration dual-topology smoke completed for proxy profile <enh|ens>
```

Deterministic gateway-readiness failure shape:

```text
[FAIL] CHECK_DUAL_TOPOLOGY_PATH :: ...
FAIL: gateway readiness dual-topology path ebusd_endpoint=tcp://127.0.0.1:8888 proxy_endpoint=<enh|ens>://127.0.0.1:<port> reason=...
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
