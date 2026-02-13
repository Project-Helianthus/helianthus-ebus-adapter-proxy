# Operations runbook (M6)

This runbook documents deployment and day-2 operations for `helianthus-ebus-adapter-proxy` during dual-active coexistence (`ebusd` + Helianthus path in parallel).

## Deployment topology (dual-active coexistence)

Goal: keep the legacy `ebusd` path stable while onboarding Helianthus traffic through proxy profiles.

```text
                  +---------------------+
                  |  physical adapter   |
                  +----------+----------+
                             |
                             | southbound (ENH/ENS owner connection)
                             v
                +---------------------------+
                | helianthus-ebus-adapter-  |
                | proxy                      |
                +-------------+-------------+
                              |
             +----------------+----------------+
             |                                 |
             v                                 v
   ebusd direct path                    Helianthus path
 (stays on direct endpoint)      (gateway / HA use proxy profile)
```

### Coexistence invariants

- `ebusd` remains on its direct adapter endpoint during rollout.
- Helianthus clients use explicit proxy profile endpoints (`enh://...` or `ens://...`).
- Source-address separation is required between topologies (for example `ebusd` on `0x31`, proxy smoke on `0xF0`/`0xF1`).
- Shared arbitration path is required for proxy write behavior, so pass-through and emulated writes follow one deterministic scheduler path.

## Deployment procedure

1. **Pre-flight validation**
   - In this repository, run:
     - `GOWORK=off go test ./...`
     - `GOWORK=off go vet ./...`
     - `./scripts/terminology-gate.sh`
     - `./scripts/verify_issue21_runbook.sh`
2. **Cross-repo readiness**
   - Ensure dependencies are on `main` (or approved rollout branch):
     - `../helianthus-ebusgateway`
     - `../helianthus-ha-addon`
     - `../helianthus-ha-integration`
3. **Deploy proxy**
   - Start/upgrade proxy with intended profile endpoint(s) and unchanged `ebusd` direct endpoint.
4. **Run operator smoke procedures**
   - Execute commands in [Operator smoke procedures](#operator-smoke-procedures).
5. **Promote only on PASS markers**
   - Promote rollout only if expected PASS markers are present.

## Fail-closed behavior

Fail-closed means proxy readiness failures do not trigger auto-cutover of `ebusd` or topology blending.

### Required fail-closed outcomes

- If gateway direct-proxy smoke fails, deployment is blocked and gateway stays off proxy cutover.
- If HA dual-topology smoke fails, deployment is blocked and coexistence mode is not promoted.
- `ebusd` direct path remains unchanged during failure handling.

### Deterministic failure markers

- Gateway direct-proxy failure:
  - `FAIL: gateway path readiness profile=<enh|ens> endpoint=<enh|ens>://...`
- HA dual-topology failure:
  - `[FAIL] CHECK_DUAL_TOPOLOGY_PATH :: ...`
  - `FAIL: gateway readiness dual-topology path ...`

## Recovery steps

1. **Stop promotion**
   - Halt rollout immediately on any fail-closed marker.
2. **Collect logs**
   - Gather `.verify/issue15/gateway-smoke-<profile>.log`.
   - Gather `.verify/issue17/ha-dual-topology-<profile>.log`.
3. **Return to known-safe topology**
   - Keep `ebusd` on direct endpoint.
   - Set Helianthus profile to `disabled` or last known good profile/endpoints.
4. **Re-verify after fix**
   - Re-run operator smoke procedures and promote only on PASS markers.

## Operator smoke procedures

Run these in sequence during rollout windows.

### 1) Compatibility baseline (issue #14)

```bash
./scripts/run-ebusd-compat-harness.sh
```

Expected success marker:

```text
RESULT: PASS (config-only migration verified; no ebusd patch required)
```

### 2) Gateway direct proxy readiness (issue #15, ENH then ENS)

```bash
./scripts/run-gateway-direct-proxy-smoke.sh \
  --gateway-repo ../helianthus-ebusgateway \
  --profile enh \
  --proxy-host 127.0.0.1 \
  --proxy-port 19001 \
  --source-address 0xF0
```

```bash
./scripts/run-gateway-direct-proxy-smoke.sh \
  --gateway-repo ../helianthus-ebusgateway \
  --profile ens \
  --proxy-host 127.0.0.1 \
  --proxy-port 19002 \
  --source-address 0xF1
```

Expected readiness marker for each run:

```text
PASS: gateway path readiness profile=<enh|ens> endpoint=<enh|ens>://127.0.0.1:<port>
```

### 3) HA dual-topology coexistence readiness (issue #17)

```bash
./scripts/run-ha-integration-dual-topology-smoke.sh \
  --ha-repo ../helianthus-ha-integration \
  --allow-non-main \
  --proxy-profile enh \
  --proxy-port 19001 \
  --ebusd-host 127.0.0.1 \
  --ebusd-port 8888
```

```bash
./scripts/run-ha-integration-dual-topology-smoke.sh \
  --ha-repo ../helianthus-ha-integration \
  --allow-non-main \
  --proxy-profile ens \
  --proxy-port 19002 \
  --ebusd-host 127.0.0.1 \
  --ebusd-port 8888
```

Expected readiness markers:

```text
[PASS] CHECK_DUAL_TOPOLOGY_PATH :: mode=coexistence_ready ...
PASS: gateway readiness dual-topology path ebusd_endpoint=tcp://127.0.0.1:8888 proxy_endpoint=<enh|ens>://127.0.0.1:<port>
PASS: ha integration dual-topology smoke completed for proxy profile <enh|ens>
```

## Rollback quick steps

1. Keep `ebusd` on direct endpoint (no rollback needed for this path if unchanged).
2. Disable proxy profile for Helianthus path.
3. Confirm no proxy-path promotion remains in orchestrator settings.
4. Re-run compatibility baseline to reconfirm direct path stability.
