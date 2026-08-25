# AGENTS

## Repository status

**DEPRECATED — read-only historical compatibility reference.** Do not add new
deployments, runtime features, or protocol capabilities here. Current Helianthus
multiplexing belongs in
[helianthus-ebusgateway/internal/adaptermux](https://github.com/Project-Helianthus/helianthus-ebusgateway/tree/main/internal/adaptermux),
built on [helianthus-ebusgo](https://github.com/Project-Helianthus/helianthus-ebusgo)
transports.

This repository may receive only narrowly scoped maintenance, security, or
historical-compatibility fixes. Preserve its archived behavior unless the issue
states a concrete compatibility or security requirement.

## Working rules

1. Work from an issue-specific branch named `issue/<number>-<slug>`; keep one active issue and PR per repository.
2. Keep each change small, reviewable, and limited to its issue acceptance criteria. Do not use this repository as a starting point for active runtime work.
3. Do not perform deployments, live-adapter activity, credential changes, or irreversible operations without explicit operator approval at the time of action.
4. Keep terminology inclusive and avoid unrelated rewrites.
5. For externally visible behavior, update the public [eBUS documentation repository](https://github.com/Project-Helianthus/helianthus-docs-ebus) in the same change cycle.
6. For transport or protocol behavior, run the full T01..T88 transport matrix and record its report. An owner-approved exception must state its reason.
7. Address blocking review findings against the exact PR head before merge consideration. Use squash merge only when all required checks and reviews pass.

## Validation

Run the checks applicable to the changed files. For Go or operational changes,
the repository CI entry point is `./scripts/ci_local.sh`; it includes the
project's formatting, terminology, build, test, lint, and applicable transport
gates. Documentation-only changes require at least Markdown/link validation and
`git diff --check`.

These instructions are self-contained: they require no workspace-root file,
parent checkout, local path convention, or private URL.
