# Conventions

## Language and layout

- Use Go for runtime and tooling.
- Keep domain contracts in `internal/domain/*`.
- Keep orchestration in `internal/proxy`.
- Keep executable wiring in `cmd/*`.

## Code quality

- Format with `gofmt`.
- Keep interfaces small and domain-focused.
- Avoid introducing dependencies without clear need.

## Validation

Before pushing changes, run:

1. `go test ./...`
2. `go vet ./...`
3. `./scripts/terminology-gate.sh`
