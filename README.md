# helianthus-ebus-adapter-proxy

> **Deprecated — read-only historical reference.** The standalone multiplexing/proxy
> function is now integrated into the eBUS runtime transport path:
> [`helianthus-ebusgateway/internal/adaptermux`](https://github.com/Project-Helianthus/helianthus-ebusgateway/tree/main/internal/adaptermux)
> wraps transports provided by
> [`helianthus-ebusgo`](https://github.com/Project-Helianthus/helianthus-ebusgo).
> Do not start new deployments or feature work from this repository.

This repository is retained to preserve the standalone proxy's historical
implementation, operational material, and compatibility evidence. For active
runtime work, use
[`helianthus-ebusgateway`](https://github.com/Project-Helianthus/helianthus-ebusgateway);
for eBUS transport/protocol work, use
[`helianthus-ebusgo`](https://github.com/Project-Helianthus/helianthus-ebusgo).

## Historical Scope

The following describes the archived standalone implementation; it is not a
placement guide for new work.

- Southbound ENH/ENS adapter drivers (`internal/southbound/*`).
- Northbound ENH/ENS multi-session listeners (`internal/northbound/*`).
- Proxy orchestration and domain contracts (`internal/proxy`, `internal/domain/*`).
- Shared write scheduler/arbitration (`internal/scheduler/write`).
- Source-address policy and lease lifecycle (`internal/sourcepolicy`).
- Emulation target registry/profile wiring (`internal/emulation/targets`).
- Compatibility/smoke tooling and operations runbook (`scripts/*`, `OPERATIONS_RUNBOOK.md`).

## Current Status

- **Read-only / historical:** no new users, deployments, or feature work belong here.
- The maintained multiplexing implementation is the gateway's internal
  `adaptermux` runtime package, built on `helianthus-ebusgo` transports.
- Historical smoke helpers, the VR90 emulation profile wiring, and technical
  documents remain available for reference and migration research.

## Active Runtime Path

```text
adapter endpoint/ebusd -> helianthus-ebusgo transports -> helianthus-ebusgateway/internal/adaptermux -> gateway runtime
        (io edge)                (transport implementation)       (multiplexing)            (operator surface)
```

For current user-facing integrations and packaging, continue from the gateway
to [`helianthus-ha-integration`](https://github.com/Project-Helianthus/helianthus-ha-integration)
and [`helianthus-ha-addon`](https://github.com/Project-Helianthus/helianthus-ha-addon).

## Historical Quickstart and Validation

The commands below are preserved solely to reproduce or inspect the archived
standalone proxy. They are not supported deployment instructions.

### 1) Clone and baseline validation

```bash
git clone https://github.com/Project-Helianthus/helianthus-ebus-adapter-proxy.git
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

For unstable plain-wire links (Wi-Fi/VPN jitter), tune START handling:

```bash
go run ./cmd/helianthus-ebus-adapter-proxy \
  --listen 0.0.0.0:19001 \
  --upstream udp-plain://203.0.113.10:9999 \
  --udp-plain-start-wait 8s \
  --udp-plain-disable-start-fallback=false
```

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

### D) Proxy semantics matrix adjunct (issue #91)

- `profiles/proxy-wire-semantics/px-cases.md`
- `profiles/proxy-wire-semantics/proxy-semantics-matrix-example.json`

## Validation Commands

| Area | Command |
|---|---|
| format (repo expectation) | `find . -name '*.go' -type f -print0 \| xargs -0 gofmt -w` |
| tests | `GOWORK=off go test ./...` |
| vet | `GOWORK=off go vet ./...` |
| terminology gate | `./scripts/terminology-gate.sh` |
| proxy semantics matrix | `./scripts/run_proxy_semantics_matrix.py --output-dir artifacts/proxy-semantics/<run-id>` |
| transport + proxy semantics matrix gate | `TRANSPORT_MATRIX_REPORT=<transport-index.json> PROXY_SEMANTICS_MATRIX_REPORT=<proxy-semantics-index.json> ./scripts/transport_gate.sh` |
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

- Gateway runtime: https://github.com/Project-Helianthus/helianthus-ebusgateway
- HA integration: https://github.com/Project-Helianthus/helianthus-ha-integration
- HA add-on: https://github.com/Project-Helianthus/helianthus-ha-addon
- eBUS docs hub: https://github.com/Project-Helianthus/helianthus-docs-ebus

### Issue workflow conventions

- Keep one issue-focused branch per change (example: `issue-51-readme-refresh`).
- Keep PR scope aligned to issue acceptance criteria.
- Include closing keyword in PR body (example: `Fixes #51`).
