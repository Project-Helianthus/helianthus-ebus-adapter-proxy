#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"
export GOWORK=off

PR_NUMBER=""
while [[ $# -gt 0 ]]; do
	case "$1" in
		--pr)
			if [[ $# -lt 2 ]]; then
				echo "missing value for --pr"
				exit 2
			fi
			PR_NUMBER="$2"
			shift 2
			;;
		--pr=*)
			PR_NUMBER="${1#*=}"
			shift
			;;
		*)
			echo "usage: $0 [--pr <number>]"
			exit 2
			;;
	esac
done

LOG_DIR=".verify/issue14"
rm -rf "${LOG_DIR}"
mkdir -p "${LOG_DIR}"

failures=0
warnings=0

pass() {
	printf 'PASS: %s\n' "$1"
}

fail() {
	printf 'FAIL: %s\n' "$1"
	failures=$((failures + 1))
}

warn() {
	printf 'WARN: %s\n' "$1"
	warnings=$((warnings + 1))
}

run_check() {
	local name="$1"
	shift
	local log_file="${LOG_DIR}/${name}.log"
	if "$@" >"${log_file}" 2>&1; then
		pass "${name}"
	else
		fail "${name} (see ${log_file})"
		tail -n 40 "${log_file}" | sed 's/^/  | /'
	fi
}

printf 'Issue #14 ebusd compatibility harness verification\n'
printf 'Repository: %s\n' "${ROOT_DIR}"
if [[ -n "${PR_NUMBER}" ]]; then
	printf 'PR: #%s\n' "${PR_NUMBER}"
fi
printf '\n'

printf '[0/8] Collect branch delta vs main\n'
run_check git-fetch-main git fetch origin main --quiet

changed_files="${LOG_DIR}/changed-files.log"
git diff --name-only origin/main...HEAD >"${changed_files}"

if [[ -s "${changed_files}" ]]; then
	pass "changed files detected vs origin/main"
else
	working_tree="${LOG_DIR}/working-tree.log"
	git status --porcelain >"${working_tree}"
	if [[ -s "${working_tree}" ]]; then
		pass "working tree changes detected (pre-commit verification path)"
	else
		fail "no changes detected vs origin/main or working tree (see ${changed_files})"
	fi
fi
printf '\n'

printf '[1/8] Harness artifacts present\n'
for required_file in cmd/ebusd-compat-harness/main.go internal/compat/ebusd/harness.go internal/compat/ebusd/harness_test.go scripts/run-ebusd-compat-harness.sh; do
	if [[ -f "${required_file}" ]]; then
		pass "artifact present: ${required_file}"
	else
		fail "artifact missing: ${required_file}"
	fi
done
printf '\n'

printf '[2/8] Representative command-set coverage\n'
command_set_log="${LOG_DIR}/command-set.log"
: >"${command_set_log}"
rg -n 'ENHReqInit|ENHReqStart|ENHReqInfo|ENHReqSend' internal/compat/ebusd/harness.go >>"${command_set_log}" || true
if [[ -s "${command_set_log}" ]]; then
	pass "representative ENH command set found in harness"
else
	fail "representative ENH command set missing (see ${command_set_log})"
fi
printf '\n'

printf '[3/8] Docs coverage for config-only migration\n'
docs_log="${LOG_DIR}/docs-coverage.log"
: >"${docs_log}"
rg -n -i 'config-only|host/port|compatibility harness|no ebusd patch required|issue #14' README.md ARCHITECTURE.md >>"${docs_log}" || true
if [[ -s "${docs_log}" ]]; then
	pass "docs include issue #14 migration semantics"
else
	fail "docs missing issue #14 migration semantics (see ${docs_log})"
fi
printf '\n'

printf '[4/8] Local smoke harness\n'
if [[ -x ./scripts/run-ebusd-compat-harness.sh ]]; then
	run_check issue14-local-smoke ./scripts/run-ebusd-compat-harness.sh --timeout 10s --log-dir "${LOG_DIR}"
elif [[ -f ./scripts/run-ebusd-compat-harness.sh ]]; then
	run_check issue14-local-smoke bash ./scripts/run-ebusd-compat-harness.sh --timeout 10s --log-dir "${LOG_DIR}"
else
	fail 'run-ebusd-compat-harness.sh missing'
fi
printf '\n'

printf '[5/8] Harness tests\n'
run_check go-test-harness go test -count=1 ./internal/compat/ebusd ./cmd/ebusd-compat-harness
printf '\n'

printf '[6/8] Full guardrails\n'
run_check go-test-all go test -count=1 ./...
run_check go-vet-all go vet ./...
if [[ -x ./scripts/terminology-gate.sh ]]; then
	run_check terminology-gate ./scripts/terminology-gate.sh
elif [[ -f ./scripts/terminology-gate.sh ]]; then
	run_check terminology-gate bash ./scripts/terminology-gate.sh
else
	fail 'terminology gate script missing'
fi
printf '\n'

printf '[7/8] CI checks\n'
if [[ -n "${PR_NUMBER}" ]]; then
	run_check gh-pr-checks gh pr checks "${PR_NUMBER}" --repo d3vi1/helianthus-ebus-adapter-proxy
else
	warn "CI check skipped (run with --pr <number>)"
fi
printf '\n'

if (( failures > 0 )); then
	printf 'RESULT: FAIL (%d issue(s), %d warning(s))\n' "${failures}" "${warnings}"
	exit 1
fi

if (( warnings > 0 )); then
	printf 'RESULT: PASS_WITH_WARNINGS (%d warning(s))\n' "${warnings}"
	exit 0
fi

printf 'RESULT: PASS (all issue #14 checks passed)\n'
