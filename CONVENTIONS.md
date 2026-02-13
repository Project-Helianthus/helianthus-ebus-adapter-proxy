# Conventions

## Language and layout

- Use Go for runtime and tooling.
- Keep domain contracts in `internal/domain/*`.
- Keep orchestration in `internal/proxy`.
- Keep executable wiring in `cmd/*`.
- Keep transport drivers in `internal/southbound/*`.
- Keep northbound listeners in `internal/northbound/*`.

## Code quality

- Format with `gofmt`.
- Keep interfaces small and domain-focused.
- Avoid introducing dependencies without clear need.
- Keep transport/listener APIs coherent across ENH and ENS packages.

## Testing expectations

- Transport codec tests must cover framing behavior, malformed input behavior, and recovery paths.
- Transport driver tests must cover lifecycle behavior, timeout mapping, reconnect behavior, and error propagation.
- Listener tests must cover concurrent sessions (2+ clients), lifecycle hooks/metrics, malformed-frame recovery, and timeout handling.
- Session manager tests must cover bounded queue backpressure behavior, overflow error taxonomy (`ErrInboundBackpressure`, `ErrOutboundBackpressure`, `ErrQueueFull`), and deterministic rejected/dropped metric counters.
- Scheduler tests must cover deterministic selection behavior, fairness under balanced load, and starvation-guard behavior under skewed sustained load.
- Scheduler concurrency tests must remain race-safe and verify valid session selection under concurrent `Select` calls.
- Source policy tests must cover deterministic allow/block/in-use filtering and soft-reserve behavior for `0x31` with and without `AllowSoftReserved`.
- Activity-window tests must verify the exact timing boundary (`window` is exclusive), plus non-trackable address handling (`0x00`, `0xFF`).
- Lease manager tests must cover `Acquire`/`Renew`/`Release`/`Expire` lifecycle behavior, including `ExpiresAt <= now` boundary behavior.
- Lease conflict tests must assert stable conflict codes for address contention, duplicate owner acquire, missing owner lease, and expired owner lease.
- Lease concurrency tests must cover simultaneous lease contention (single address and multi-address pools) and assert one active winner per address.
- Compatibility harness tests must prove identical direct-vs-proxy request/response exchanges for representative ebusd command sets under deterministic local topology.
- Prefer deterministic assertions (stable counters/order, injected clocks) over sleep-based timing assertions.

## M4 smoke marker expectations

- `scripts/run-ebusd-compat-harness.sh` must leave `.verify/issue14/ebusd-compat-harness.log` with deterministic terminal marker `RESULT: PASS ...` on success (or `RESULT: FAIL ...` on mismatch).
- `scripts/run-gateway-direct-proxy-smoke.sh` must leave `.verify/issue15/gateway-smoke-<profile>.log` and emit deterministic readiness lines `PASS: gateway path readiness profile=<enh|ens> endpoint=<enh|ens>://...` or `FAIL: gateway path readiness ...`.
- HA add-on linkage checks (issue #16) run in `../helianthus-ha-addon/scripts/smoke_addon_checklist.py`; proxy topology validation must assert `[PASS] CHECK_LOG_PROXY_PROFILE :: ...` and `[PASS] CHECK_LOG_PROXY_ENDPOINT :: ...`.
- `scripts/run-ha-integration-dual-topology-smoke.sh` must leave `.verify/issue17/ha-dual-topology-<profile>.log`, parse `CHECK_DUAL_TOPOLOGY_PATH`, and emit deterministic wrapper markers `PASS|FAIL: gateway readiness dual-topology path ...`.
- Dual-topology verification must require both marker layers: checklist marker (`[PASS] CHECK_DUAL_TOPOLOGY_PATH :: ...`) and overall marker (`OVERALL PASS`) from `../helianthus-ha-integration/scripts/run-ha-dual-topology-smoke.sh`.

## M6 docs gate expectations

- `OPERATIONS_RUNBOOK.md` must document:
  - dual-active coexistence topology (`ebusd` direct path + Helianthus proxy path),
  - fail-closed behavior with deterministic failure markers, and
  - recovery steps plus operator smoke procedures with expected PASS markers.
- `./scripts/verify_issue21_runbook.sh` should be used as a lightweight docs gate for required runbook sections/markers.

## Terminology

- Preferred terms: `initiator`, `target`, `allow`, `block`.
- If legacy external wording must be referenced for compatibility, scope it to the external context and clarify preferred repository terms nearby.
- Terminology policy is enforced by `./scripts/terminology-gate.sh`.

## Validation

Before pushing changes, run:

1. `GOWORK=off go test ./...`
2. `GOWORK=off go vet ./...`
3. `./scripts/terminology-gate.sh`
