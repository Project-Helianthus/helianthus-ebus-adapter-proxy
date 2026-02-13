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
- Session manager tests must cover bounded queue backpressure behavior, overflow error taxonomy, and rejected/dropped metric counters.
- Prefer deterministic assertions (stable counters/order) over timing-sensitive assertions.

## Terminology

- Preferred terms: `initiator`, `target`, `allow`, `block`.
- If legacy external wording must be referenced for compatibility, scope it to the external context and clarify preferred repository terms nearby.
- Terminology policy is enforced by `./scripts/terminology-gate.sh`.

## Validation

Before pushing changes, run:

1. `GOWORK=off go test ./...`
2. `GOWORK=off go vet ./...`
3. `./scripts/terminology-gate.sh`
