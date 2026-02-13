#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

RUNBOOK_FILE="OPERATIONS_RUNBOOK.md"
if [[ ! -f "${RUNBOOK_FILE}" ]]; then
	echo "FAIL: runbook file missing (${RUNBOOK_FILE})"
	exit 1
fi

required_patterns=(
	"## Deployment topology \\(dual-active coexistence\\)"
	"## Fail-closed behavior"
	"## Recovery steps"
	"## Operator smoke procedures"
	"RESULT: PASS \\(config-only migration verified; no ebusd patch required\\)"
	"PASS: gateway path readiness profile=<enh\\|ens> endpoint=<enh\\|ens>://127.0.0.1:<port>"
	"\\[PASS\\] CHECK_DUAL_TOPOLOGY_PATH :: mode=coexistence_ready ..."
	"PASS: gateway readiness dual-topology path ebusd_endpoint=tcp://127.0.0.1:8888 proxy_endpoint=<enh\\|ens>://127.0.0.1:<port>"
)

for pattern in "${required_patterns[@]}"; do
	if ! rg -q -n "${pattern}" "${RUNBOOK_FILE}"; then
		echo "FAIL: runbook marker missing -> ${pattern}"
		exit 1
	fi
done

echo "PASS: issue #21 runbook markers verified"
