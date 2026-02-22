# AGENTS

These instructions apply to the entire repository.

1. Keep changes targeted to the requested issue.
2. Prefer small, composable packages under `internal/`.
3. Always run `gofmt ./...` before finishing.
4. Validate with `./scripts/ci_local.sh` (local CI is authoritative if GitHub Actions minutes are unavailable).
5. React (emoji) to every review comment and reply with status when actioned.
6. Keep repository terminology inclusive and consistent.
7. If a change modifies externally visible behavior, update `helianthus-docs-ebus` alongside the code change (doc-gate).
8. Transport/protocol changes require a full 88-case runtime matrix pass (`TRANSPORT_MATRIX_REPORT=<index.json>`), unless explicitly overridden by owner approval (`TRANSPORT_GATE_OWNER_OVERRIDE=OVERRIDE_TRANSPORT_GATE_BY_OWNER` with a reason).
