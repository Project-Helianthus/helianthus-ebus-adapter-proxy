# AGENTS

This repository is part of the **Helianthus Multi-Protocol HVAC Gateway Platform**.

## Dual-AI Operating Model

All development follows the dual-AI orchestrator protocol defined in the workspace-root [`AGENTS.md`](../AGENTS.md):

- **Orchestrator:** Claude Code — orchestration, hard dev (complexity 7–10), angry tester, deep consultant
- **Co-Pilot:** Codex — adversarial planning, easy dev (complexity 1–6), code review, second opinions
- Phases: Adversarial Planning → Smart Routing → Dual Code Review
- Hard rules: one issue/PR per repo, squash+merge only, doc-gate, transport-gate, MCP-first

See the root AGENTS.md for the full protocol, routing tables, system prompts, and invariants.

---

## Repo-Specific Rules

These instructions apply to the entire repository.

1. Keep changes targeted to the requested issue.
2. Prefer small, composable packages under `internal/`.
3. Always run `gofmt ./...` before finishing.
4. Validate with `./scripts/ci_local.sh` (local CI is authoritative if GitHub Actions minutes are unavailable).
5. React (emoji) to every review comment and reply with status when actioned.
6. Keep repository terminology inclusive and consistent.
7. If a change modifies externally visible behavior, update `helianthus-docs-ebus` alongside the code change (doc-gate).
8. Transport/protocol changes require a full 88-case runtime matrix pass (`TRANSPORT_MATRIX_REPORT=<index.json>`), unless explicitly overridden by owner approval (`TRANSPORT_GATE_OWNER_OVERRIDE=OVERRIDE_TRANSPORT_GATE_BY_OWNER` with a reason).
