#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"
export GOWORK=off

TIMEOUT="10s"
LOG_DIR=".verify/issue14"

while [[ $# -gt 0 ]]; do
	case "$1" in
		--timeout)
			if [[ $# -lt 2 ]]; then
				echo "missing value for --timeout"
				exit 2
			fi
			TIMEOUT="$2"
			shift 2
			;;
		--timeout=*)
			TIMEOUT="${1#*=}"
			shift
			;;
		--log-dir)
			if [[ $# -lt 2 ]]; then
				echo "missing value for --log-dir"
				exit 2
			fi
			LOG_DIR="$2"
			shift 2
			;;
		--log-dir=*)
			LOG_DIR="${1#*=}"
			shift
			;;
		*)
			echo "usage: $0 [--timeout <duration>] [--log-dir <dir>]"
			exit 2
			;;
	esac
done

mkdir -p "${LOG_DIR}"
HARNESS_LOG="${LOG_DIR}/ebusd-compat-harness.log"

printf 'Issue #14 local smoke harness\n'
printf 'Repository: %s\n' "${ROOT_DIR}"
printf 'Timeout: %s\n' "${TIMEOUT}"
printf 'Log: %s\n' "${HARNESS_LOG}"
printf '\n'

if go run ./cmd/ebusd-compat-harness --timeout "${TIMEOUT}" | tee "${HARNESS_LOG}"; then
	if rg -q '^RESULT: PASS ' "${HARNESS_LOG}"; then
		printf '\nPASS: config-only endpoint switch validated (host/port only)\n'
		exit 0
	fi

	printf '\nFAIL: harness did not emit PASS result (see %s)\n' "${HARNESS_LOG}"
	exit 1
fi

printf '\nFAIL: harness execution failed (see %s)\n' "${HARNESS_LOG}"
exit 1
