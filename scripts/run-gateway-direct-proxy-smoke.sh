#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${ROOT_DIR}"

GATEWAY_REPO="${ROOT_DIR}/../helianthus-ebusgateway"
PROFILE=""
PROXY_HOST="127.0.0.1"
PROXY_PORT=""
SOURCE_ADDRESS=""
DEFAULT_EBUSD_SOURCE_ADDRESS="0x31"
ENH_TIMEOUT_SEC="10"
SCAN_TIMEOUT_SEC="8"
METHOD_TIMEOUT_SEC="10"
LOG_DIR=".verify/issue15"
ALLOW_NON_MAIN=0
KEEP_AGENT_LOCAL=0

usage() {
	cat <<'USAGE'
usage: run-gateway-direct-proxy-smoke.sh --profile <enh|ens> [options]

Options:
  --gateway-repo <path>      Path to helianthus-ebusgateway repo (default: ../helianthus-ebusgateway)
  --profile <enh|ens>        Smoke profile to run (required)
  --proxy-host <host>        Adapter-proxy host (default: 127.0.0.1)
  --proxy-port <port>        Adapter-proxy port (default: 19001 for enh, 19002 for ens)
  --source-address <hex>     smoke.source_address value (default: 0xF0 for enh, 0xF1 for ens)
  --enh-timeout-sec <sec>    enh.timeout_sec value (default: 10)
  --scan-timeout-sec <sec>   smoke.scan_timeout_sec value (default: 8)
  --method-timeout-sec <sec> smoke.method_timeout_sec value (default: 10)
  --log-dir <dir>            Log output directory (default: .verify/issue15)
  --allow-non-main           Allow gateway repo branch other than main
  --keep-agent-local         Keep generated AGENT-local.md instead of restoring previous file
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
		--gateway-repo)
			require_value "$1" "$@"
			GATEWAY_REPO="$2"
			shift 2
			;;
		--gateway-repo=*)
			GATEWAY_REPO="${1#*=}"
			shift
			;;
		--profile)
			require_value "$1" "$@"
			PROFILE="$2"
			shift 2
			;;
		--profile=*)
			PROFILE="${1#*=}"
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
		--source-address)
			require_value "$1" "$@"
			SOURCE_ADDRESS="$2"
			shift 2
			;;
		--source-address=*)
			SOURCE_ADDRESS="${1#*=}"
			shift
			;;
		--enh-timeout-sec)
			require_value "$1" "$@"
			ENH_TIMEOUT_SEC="$2"
			shift 2
			;;
		--enh-timeout-sec=*)
			ENH_TIMEOUT_SEC="${1#*=}"
			shift
			;;
		--scan-timeout-sec)
			require_value "$1" "$@"
			SCAN_TIMEOUT_SEC="$2"
			shift 2
			;;
		--scan-timeout-sec=*)
			SCAN_TIMEOUT_SEC="${1#*=}"
			shift
			;;
		--method-timeout-sec)
			require_value "$1" "$@"
			METHOD_TIMEOUT_SEC="$2"
			shift 2
			;;
		--method-timeout-sec=*)
			METHOD_TIMEOUT_SEC="${1#*=}"
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
		--keep-agent-local)
			KEEP_AGENT_LOCAL=1
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

PROFILE="$(printf '%s' "${PROFILE}" | tr '[:upper:]' '[:lower:]')"
if [[ "${PROFILE}" != "enh" && "${PROFILE}" != "ens" ]]; then
	echo "profile must be enh or ens"
	usage
	exit 2
fi

if [[ -z "${PROXY_PORT}" ]]; then
	if [[ "${PROFILE}" == "enh" ]]; then
		PROXY_PORT="19001"
	else
		PROXY_PORT="19002"
	fi
fi

if [[ -z "${SOURCE_ADDRESS}" ]]; then
	if [[ "${PROFILE}" == "enh" ]]; then
		SOURCE_ADDRESS="0xF0"
	else
		SOURCE_ADDRESS="0xF1"
	fi
fi

source_address_normalized="$(printf '%s' "${SOURCE_ADDRESS}" | tr '[:upper:]' '[:lower:]')"
ebusd_source_normalized="$(printf '%s' "${DEFAULT_EBUSD_SOURCE_ADDRESS}" | tr '[:upper:]' '[:lower:]')"
if [[ "${source_address_normalized}" == "${ebusd_source_normalized}" ]]; then
	echo "WARN: source-address ${SOURCE_ADDRESS} overlaps ebusd default ${DEFAULT_EBUSD_SOURCE_ADDRESS}; choose a dedicated gateway source when running in parallel"
fi

if [[ ! -d "${GATEWAY_REPO}" ]]; then
	echo "gateway repo path not found: ${GATEWAY_REPO}"
	exit 1
fi
if [[ ! -f "${GATEWAY_REPO}/go.mod" ]]; then
	echo "gateway repo missing go.mod: ${GATEWAY_REPO}"
	exit 1
fi
if ! git -C "${GATEWAY_REPO}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "gateway repo is not a git working tree: ${GATEWAY_REPO}"
	exit 1
fi

gateway_branch="$(git -C "${GATEWAY_REPO}" rev-parse --abbrev-ref HEAD)"
if [[ "${ALLOW_NON_MAIN}" -eq 0 && "${gateway_branch}" != "main" ]]; then
	echo "gateway repo must be on main for issue #15 verification (current: ${gateway_branch})"
	echo "run: git -C '${GATEWAY_REPO}' checkout main && git -C '${GATEWAY_REPO}' pull --ff-only origin main"
	exit 1
fi

mkdir -p "${LOG_DIR}"
SMOKE_LOG="${LOG_DIR}/gateway-smoke-${PROFILE}.log"
REPORT_JSON_OUTPUT="artifacts/smoke-report.${PROFILE}.json"
PROXY_ENDPOINT_MARKER="${PROFILE}://${PROXY_HOST}:${PROXY_PORT}"

AGENT_LOCAL_PATH="${GATEWAY_REPO}/AGENT-local.md"
AGENT_LOCAL_BACKUP=""
AGENT_LOCAL_EXISTED=0

if [[ -f "${AGENT_LOCAL_PATH}" ]]; then
	AGENT_LOCAL_BACKUP="$(mktemp "${TMPDIR:-/tmp}/gateway-agent-local.XXXXXX")"
	cp "${AGENT_LOCAL_PATH}" "${AGENT_LOCAL_BACKUP}"
	AGENT_LOCAL_EXISTED=1
fi

restore_agent_local() {
	if [[ "${KEEP_AGENT_LOCAL}" -eq 1 ]]; then
		if [[ -n "${AGENT_LOCAL_BACKUP}" && -f "${AGENT_LOCAL_BACKUP}" ]]; then
			rm -f "${AGENT_LOCAL_BACKUP}"
		fi
		return
	fi

	if [[ "${AGENT_LOCAL_EXISTED}" -eq 1 ]]; then
		cp "${AGENT_LOCAL_BACKUP}" "${AGENT_LOCAL_PATH}"
	else
		rm -f "${AGENT_LOCAL_PATH}"
	fi

	if [[ -n "${AGENT_LOCAL_BACKUP}" && -f "${AGENT_LOCAL_BACKUP}" ]]; then
		rm -f "${AGENT_LOCAL_BACKUP}"
	fi
}

trap restore_agent_local EXIT

cat >"${AGENT_LOCAL_PATH}" <<EOF
# Generated by helianthus-ebus-adapter-proxy/scripts/run-gateway-direct-proxy-smoke.sh
# Profile: ${PROFILE}
# Template source: profiles/gateway-direct-proxy/agent-local.${PROFILE}.md

\`\`\`yaml
enh:
  type: tcp
  host: "${PROXY_HOST}"
  port: ${PROXY_PORT}
  timeout_sec: ${ENH_TIMEOUT_SEC}

smoke:
  profile: ${PROFILE}
  source_address: ${SOURCE_ADDRESS}
  scan_timeout_sec: ${SCAN_TIMEOUT_SEC}
  method_timeout_sec: ${METHOD_TIMEOUT_SEC}
  report_json_output: "${REPORT_JSON_OUTPUT}"
\`\`\`
EOF

printf 'Issue #15 gateway direct proxy smoke\n'
printf 'Gateway repo: %s\n' "${GATEWAY_REPO}"
printf 'Gateway branch: %s\n' "${gateway_branch}"
printf 'Profile: %s\n' "${PROFILE}"
printf 'Proxy target: %s:%s\n' "${PROXY_HOST}" "${PROXY_PORT}"
printf 'Proxy profile: %s\n' "${PROFILE}"
printf 'Proxy endpoint: %s\n' "${PROXY_ENDPOINT_MARKER}"
printf 'Gateway source address: %s\n' "${SOURCE_ADDRESS}"
printf 'Dual topology separation: keep ebusd on %s, gateway on dedicated source address\n' "${DEFAULT_EBUSD_SOURCE_ADDRESS}"
printf 'Smoke log: %s/%s\n' "${ROOT_DIR}" "${SMOKE_LOG}"
printf '\n'

(
	cd "${GATEWAY_REPO}"
	export GOWORK=off
	EBUS_SMOKE=1 go run ./cmd/smoke
) | tee "${SMOKE_LOG}"

printf '\nPASS: gateway smoke profile %s completed against %s:%s\n' "${PROFILE}" "${PROXY_HOST}" "${PROXY_PORT}"
