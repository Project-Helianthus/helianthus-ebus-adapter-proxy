#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

HA_REPO="${ROOT_DIR}/../helianthus-ha-integration"
URL=""
HOST="127.0.0.1"
PORT="8080"
PATH_VALUE="/graphql"
TRANSPORT="http"
TIMEOUT="10"
EBUSD_HOST="127.0.0.1"
EBUSD_PORT="8888"
PROXY_PROFILE="enh"
PROXY_HOST="127.0.0.1"
PROXY_PORT=""
LOG_DIR=".verify/issue17"
ALLOW_NON_MAIN=0

usage() {
	cat <<'USAGE'
usage: run-ha-integration-dual-topology-smoke.sh [options]

Options:
  --ha-repo <path>           Path to helianthus-ha-integration repo (default: ../helianthus-ha-integration)
  --url <url>                Full GraphQL URL (overrides host/port/path/transport)
  --host <host>              GraphQL host (default: 127.0.0.1)
  --port <port>              GraphQL port (default: 8080)
  --path <path>              GraphQL path (default: /graphql)
  --transport <http|https>   GraphQL transport (default: http)
  --timeout <seconds>        Request/probe timeout (default: 10)
  --ebusd-host <host>        ebusd host probe target (default: 127.0.0.1)
  --ebusd-port <port>        ebusd port probe target (default: 8888)
  --proxy-profile <enh|ens>  Adapter-proxy profile (default: enh)
  --proxy-host <host>        Adapter-proxy host probe target (default: 127.0.0.1)
  --proxy-port <port>        Adapter-proxy port (default: 19001 for enh, 19002 for ens)
  --log-dir <dir>            Log output directory (default: .verify/issue17)
  --allow-non-main           Allow HA integration repo branch other than main
USAGE
}

require_value() {
	local option="$1"
	if [[ $# -lt 2 ]]; then
		echo "missing value for ${option}"
		usage
		exit 2
	fi
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--ha-repo)
			require_value "$1" "$@"
			HA_REPO="$2"
			shift 2
			;;
		--ha-repo=*)
			HA_REPO="${1#*=}"
			shift
			;;
		--url)
			require_value "$1" "$@"
			URL="$2"
			shift 2
			;;
		--url=*)
			URL="${1#*=}"
			shift
			;;
		--host)
			require_value "$1" "$@"
			HOST="$2"
			shift 2
			;;
		--host=*)
			HOST="${1#*=}"
			shift
			;;
		--port)
			require_value "$1" "$@"
			PORT="$2"
			shift 2
			;;
		--port=*)
			PORT="${1#*=}"
			shift
			;;
		--path)
			require_value "$1" "$@"
			PATH_VALUE="$2"
			shift 2
			;;
		--path=*)
			PATH_VALUE="${1#*=}"
			shift
			;;
		--transport)
			require_value "$1" "$@"
			TRANSPORT="$2"
			shift 2
			;;
		--transport=*)
			TRANSPORT="${1#*=}"
			shift
			;;
		--timeout)
			require_value "$1" "$@"
			TIMEOUT="$2"
			shift 2
			;;
		--timeout=*)
			TIMEOUT="${1#*=}"
			shift
			;;
		--ebusd-host)
			require_value "$1" "$@"
			EBUSD_HOST="$2"
			shift 2
			;;
		--ebusd-host=*)
			EBUSD_HOST="${1#*=}"
			shift
			;;
		--ebusd-port)
			require_value "$1" "$@"
			EBUSD_PORT="$2"
			shift 2
			;;
		--ebusd-port=*)
			EBUSD_PORT="${1#*=}"
			shift
			;;
		--proxy-profile)
			require_value "$1" "$@"
			PROXY_PROFILE="$2"
			shift 2
			;;
		--proxy-profile=*)
			PROXY_PROFILE="${1#*=}"
			shift
			;;
		--proxy-host)
			require_value "$1" "$@"
			PROXY_HOST="$2"
			shift 2
			;;
		--proxy-host=*)
			PROXY_HOST="${1#*=}"
			shift
			;;
		--proxy-port)
			require_value "$1" "$@"
			PROXY_PORT="$2"
			shift 2
			;;
		--proxy-port=*)
			PROXY_PORT="${1#*=}"
			shift
			;;
		--log-dir)
			require_value "$1" "$@"
			LOG_DIR="$2"
			shift 2
			;;
		--log-dir=*)
			LOG_DIR="${1#*=}"
			shift
			;;
		--allow-non-main)
			ALLOW_NON_MAIN=1
			shift
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			echo "unknown argument: $1"
			usage
			exit 2
			;;
	esac
done

PROXY_PROFILE="$(printf '%s' "${PROXY_PROFILE}" | tr '[:upper:]' '[:lower:]')"
case "${PROXY_PROFILE}" in
	enh)
		DEFAULT_PROXY_PORT="19001"
		;;
	ens)
		DEFAULT_PROXY_PORT="19002"
		;;
	*)
		echo "proxy profile must be enh or ens"
		usage
		exit 2
		;;
esac

if [[ -z "${PROXY_PORT}" ]]; then
	PROXY_PORT="${DEFAULT_PROXY_PORT}"
fi

if [[ ! -d "${HA_REPO}" ]]; then
	echo "ha integration repo path not found: ${HA_REPO}"
	exit 1
fi
if [[ ! -f "${HA_REPO}/pyproject.toml" && ! -f "${HA_REPO}/requirements.txt" ]]; then
	echo "ha integration repo does not look valid: ${HA_REPO}"
	exit 1
fi
if ! git -C "${HA_REPO}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "ha integration repo is not a git working tree: ${HA_REPO}"
	exit 1
fi

ha_branch="$(git -C "${HA_REPO}" rev-parse --abbrev-ref HEAD)"
if [[ "${ALLOW_NON_MAIN}" -eq 0 && "${ha_branch}" != "main" ]]; then
	echo "ha integration repo must be on main for issue #17 verification (current: ${ha_branch})"
	echo "run: git -C '${HA_REPO}' checkout main && git -C '${HA_REPO}' pull --ff-only origin main"
	echo "or rerun with --allow-non-main when coordinating with d3vi1/helianthus-ha-integration#59"
	exit 1
fi

HA_SMOKE_SCRIPT="${HA_REPO}/scripts/run-ha-dual-topology-smoke.sh"
if [[ ! -f "${HA_SMOKE_SCRIPT}" ]]; then
	echo "ha dual-topology smoke script not found: ${HA_SMOKE_SCRIPT}"
	echo "requires d3vi1/helianthus-ha-integration#59 or newer"
	exit 1
fi

normalized_path="${PATH_VALUE}"
if [[ "${normalized_path}" != /* ]]; then
	normalized_path="/${normalized_path}"
fi

GRAPHQL_ENDPOINT="${URL}"
if [[ -z "${GRAPHQL_ENDPOINT}" ]]; then
	GRAPHQL_ENDPOINT="${TRANSPORT}://${HOST}:${PORT}${normalized_path}"
fi

EBUSD_ENDPOINT="tcp://${EBUSD_HOST}:${EBUSD_PORT}"
PROXY_ENDPOINT="${PROXY_PROFILE}://${PROXY_HOST}:${PROXY_PORT}"

mkdir -p "${LOG_DIR}"
SMOKE_LOG="${LOG_DIR}/ha-dual-topology-${PROXY_PROFILE}.log"

printf 'Issue #17 HA integration dual-topology smoke\n'
printf 'HA integration repo: %s\n' "${HA_REPO}"
printf 'HA integration branch: %s\n' "${ha_branch}"
printf 'GraphQL endpoint: %s\n' "${GRAPHQL_ENDPOINT}"
printf 'ebusd endpoint: %s\n' "${EBUSD_ENDPOINT}"
printf 'Proxy endpoint: %s\n' "${PROXY_ENDPOINT}"
printf 'Gateway readiness marker: CHECK_DUAL_TOPOLOGY_PATH\n'
printf 'Smoke log: %s/%s\n' "${ROOT_DIR}" "${SMOKE_LOG}"
printf '\n'

HA_SMOKE_CMD=("${HA_SMOKE_SCRIPT}")
if [[ ! -x "${HA_SMOKE_SCRIPT}" ]]; then
	HA_SMOKE_CMD=(bash "${HA_SMOKE_SCRIPT}")
fi

CMD=(
	"${HA_SMOKE_CMD[@]}"
	--timeout "${TIMEOUT}"
	--ebusd-host "${EBUSD_HOST}"
	--ebusd-port "${EBUSD_PORT}"
	--proxy-profile "${PROXY_PROFILE}"
	--proxy-host "${PROXY_HOST}"
	--proxy-port "${PROXY_PORT}"
)

if [[ -n "${URL}" ]]; then
	CMD+=(--url "${URL}")
else
	CMD+=(--host "${HOST}" --port "${PORT}" --path "${normalized_path}" --transport "${TRANSPORT}")
fi

ha_exit=0
if "${CMD[@]}" | tee "${SMOKE_LOG}"; then
	ha_exit=0
else
	ha_exit=$?
fi

fail_prefix='[FAIL] CHECK_DUAL_TOPOLOGY_PATH :: '
dual_topology_pass_line="$(rg -m 1 -n "^\[PASS\] CHECK_DUAL_TOPOLOGY_PATH :: " "${SMOKE_LOG}" || true)"
dual_topology_fail_line="$(rg -m 1 -n "^\[FAIL\] CHECK_DUAL_TOPOLOGY_PATH :: " "${SMOKE_LOG}" || true)"

if [[ -n "${dual_topology_pass_line}" ]]; then
	printf '\nPASS: gateway readiness dual-topology path ebusd_endpoint=%s proxy_endpoint=%s\n' "${EBUSD_ENDPOINT}" "${PROXY_ENDPOINT}"
else
	reason=""
	if [[ -n "${dual_topology_fail_line}" ]]; then
		reason="${dual_topology_fail_line#*:}"
		if [[ "${reason}" == "${fail_prefix}"* ]]; then
			reason="${reason:${#fail_prefix}}"
		fi
	fi
	if [[ -z "${reason}" && "${ha_exit}" -ne 0 ]]; then
		reason="ha smoke exited with status ${ha_exit}"
	fi
	if [[ -z "${reason}" ]]; then
		reason="missing CHECK_DUAL_TOPOLOGY_PATH marker"
	fi

	printf '\nFAIL: gateway readiness dual-topology path ebusd_endpoint=%s proxy_endpoint=%s reason=%s (see %s/%s)\n' "${EBUSD_ENDPOINT}" "${PROXY_ENDPOINT}" "${reason}" "${ROOT_DIR}" "${SMOKE_LOG}"
	exit 1
fi

if [[ "${ha_exit}" -ne 0 ]]; then
	printf 'FAIL: ha integration smoke overall result was non-pass (exit=%s, see %s/%s)\n' "${ha_exit}" "${ROOT_DIR}" "${SMOKE_LOG}"
	exit "${ha_exit}"
fi

overall_line="$(rg -m 1 -n '^OVERALL PASS$' "${SMOKE_LOG}" || true)"
if [[ -z "${overall_line}" ]]; then
	printf 'FAIL: ha integration smoke missing OVERALL PASS marker (see %s/%s)\n' "${ROOT_DIR}" "${SMOKE_LOG}"
	exit 1
fi

printf 'PASS: ha integration dual-topology smoke completed for proxy profile %s\n' "${PROXY_PROFILE}"
