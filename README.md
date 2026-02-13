# helianthus-ebus-adapter-proxy

Bootstrap repository for an eBUS adapter proxy service.

## What is included

- Go module initialization.
- Domain package skeletons for downstream, upstream, and routing concerns.
- Proxy service skeleton with unit tests.
- CI workflow that runs tests, vet, and terminology checks.
- Repository guardrail documents.

## Quick start

```bash
go test ./...
go vet ./...
./scripts/terminology-gate.sh
```

## Repository documents

- `AGENTS.md`
- `ARCHITECTURE.md`
- `CONVENTIONS.md`
