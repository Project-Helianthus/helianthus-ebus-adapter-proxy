# helianthus-ebus-adapter-proxy

`helianthus-ebus-adapter-proxy` provides a shared proxy path between one southbound eBUS adapter connection and multiple northbound client sessions (ENH/ENS), including deterministic write arbitration for pass-through and emulated traffic.

## Purpose and Scope

### What belongs in this repository

- Southbound ENH/ENS adapter drivers (`internal/southbound/*`).
- Northbound ENH/ENS multi-session listeners (`internal/northbound/*`).
- Proxy orchestration and domain contracts (`internal/proxy`, `internal/domain/*`).
- Shared write scheduler/arbitration (`internal/scheduler/write`).
- Source-address policy and lease lifecycle (`internal/sourcepolicy`).
- Emulation target registry/profile wiring (`internal/emulation/targets`).
- Compatibility/smoke tooling and operations runbook (`scripts/*`, `OPERATIONS_RUNBOOK.md`).

### What does not belong in this repository

- Gateway API/runtime serving (belongs to `helianthus-ebusgateway`).
- Home Assistant integration entity model (belongs to `helianthus-ha-integration`).
- Home Assistant add-on packaging (belongs to `helianthus-ha-addon`).

## Status and Maturity

- Active proxy foundation with deterministic test coverage.
- Includes issue-aligned smoke helpers for compatibility, gateway direct-proxy checks, and HA dual-topology checks.
- Built-in VR90 emulation profile wiring is available and disabled by default.

## Helianthus Dependency Chain

```text
adapter endpoint/ebusd -> helianthus-ebus-adapter-proxy -> helianthus-ebusgateway -> helianthus-ha-integration/ha-addon
        (io edge)                 (arbitration + proxy)         (api runtime)            (operator surfaces)
```

## Quickstart (copy/paste)

### 1) Clone and baseline validation

```bash
git clone https://github.com/d3vi1/helianthus-ebus-adapter-proxy.git
cd helianthus-ebus-adapter-proxy
./scripts/ci_local.sh
```

### 2) Local compatibility harness (simulated, no external hardware)

```bash
go run ./cmd/ebusd-compat-harness --timeout 10s
```

Or via wrapper:

```bash
./scripts/run-ebusd-compat-harness.sh --timeout 10s --log-dir .verify/issue14
```

### 3) Run proxy against adapter endpoint

```bash
go run ./cmd/helianthus-ebus-adapter-proxy \
  --listen 0.0.0.0:19001 \
  --upstream enh://203.0.113.10:9999
```

Enable a northbound raw UDP endpoint for plain-byte clients:

```bash
go run ./cmd/helianthus-ebus-adapter-proxy \
  --listen 0.0.0.0:19001 \
  --listen-udp-plain 0.0.0.0:19002 \
  --upstream enh://203.0.113.10:9999
```

For raw UDP adapters (no ENH framing), use:

```bash
go run ./cmd/helianthus-ebus-adapter-proxy \
  --listen 0.0.0.0:19001 \
  --upstream udp-plain://203.0.113.10:9999 \
  --wire-log /tmp/helianthus-ebus-wire.log
```

`--wire-log` stores timestamped TX/RX bytes only (no client addresses/credentials).

## Local Smoke-Test Configuration Examples

### A) Gateway direct-proxy smoke profile (issue #15)

Use when `../helianthus-ebusgateway` is available and pointed at this proxy:

```bash
./scripts/run-gateway-direct-proxy-smoke.sh \
  --gateway-repo ../helianthus-ebusgateway \
  --profile enh \
  --proxy-host 127.0.0.1 \
  --proxy-port 19001 \
  --source-address 0xF0
```

### B) HA integration dual-topology smoke profile (issue #17)

Use when `../helianthus-ha-integration` is available for coexistence checks:

```bash
./scripts/run-ha-integration-dual-topology-smoke.sh \
  --ha-repo ../helianthus-ha-integration \
  --proxy-profile enh \
  --proxy-port 19001 \
  --ebusd-host 127.0.0.1 \
  --ebusd-port 8888
```

### C) Profile templates for generated gateway `AGENT-local.md`

- `profiles/gateway-direct-proxy/agent-local.enh.md`
- `profiles/gateway-direct-proxy/agent-local.ens.md`

## Validation Commands

| Area | Command |
|---|---|
| format (repo expectation) | `find . -name '*.go' -type f -print0 \| xargs -0 gofmt -w` |
| tests | `GOWORK=off go test ./...` |
| vet | `GOWORK=off go vet ./...` |
| terminology gate | `./scripts/terminology-gate.sh` |
| operations runbook gate | `./scripts/verify_issue21_runbook.sh` |
| compatibility harness | `go run ./cmd/ebusd-compat-harness --timeout 10s` |
| gateway smoke CLI help | `./scripts/run-gateway-direct-proxy-smoke.sh --help` |
| HA dual-topology smoke CLI help | `./scripts/run-ha-integration-dual-topology-smoke.sh --help` |

## Link Map

### Local repository docs

- Architecture: `ARCHITECTURE.md`
- Conventions: `CONVENTIONS.md`
- Operations runbook: `OPERATIONS_RUNBOOK.md`
- Agent instructions: `AGENTS.md`

### Related repos/docs

- Gateway runtime: https://github.com/d3vi1/helianthus-ebusgateway
- HA integration: https://github.com/d3vi1/helianthus-ha-integration
- HA add-on: https://github.com/d3vi1/helianthus-ha-addon
- eBUS docs hub: https://github.com/d3vi1/helianthus-docs-ebus

### Issue workflow conventions

- Keep one issue-focused branch per change (example: `issue-51-readme-refresh`).
- Keep PR scope aligned to issue acceptance criteria.
- Include closing keyword in PR body (example: `Fixes #51`).
