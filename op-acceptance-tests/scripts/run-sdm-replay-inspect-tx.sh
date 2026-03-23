#!/usr/bin/env bash
set -euo pipefail

# Inspect a single transaction with SDM replay context and storage access tracing.
#
# Examples:
#   op-acceptance-tests/scripts/run-sdm-replay-inspect-tx.sh --tx 0xabc...
#   op-acceptance-tests/scripts/run-sdm-replay-inspect-tx.sh \
#     --tx 0xabc... \
#     --rpc http://100.126.224.125:8545 \
#     --report-out /tmp/sdm-inspect-tx.md

RPC_URL="${RPC_URL:-http://100.126.224.125:8545}"
TX_HASH="${TX_HASH:-}"
REPORT_OUT="${REPORT_OUT:-}"
GOCACHE_DIR="${GOCACHE_DIR:-/tmp/optimism-codex-gocache}"
GO_BIN="${GO_BIN:-go}"
TRACE_PRIOR_BLOCK="${TRACE_PRIOR_BLOCK:-true}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

usage() {
  cat <<EOF
Usage: $0 [options]

Options:
  --rpc URL             RPC endpoint (default: ${RPC_URL})
  --tx HASH             Transaction hash to inspect (required)
  --report-out PATH     Markdown report path (default: /tmp/sdm-inspect-tx-<hash>.md)
  --gocache PATH        GOCACHE directory for go run (default: ${GOCACHE_DIR})
  --go-bin PATH         Go binary to use (default: ${GO_BIN})
  --no-prior-block      Skip tracing earlier block txs for overlap analysis
  --compare-payload     Compare replay refunds against embedded SDM payload entries
  --compare-rpc-receipts Compare replay refunds against receipt opGasRefund values
  -h, --help            Show this help
EOF
}

COMPARE_PAYLOAD=false
COMPARE_RPC_RECEIPTS=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --rpc)
      RPC_URL="$2"
      shift 2
      ;;
    --tx)
      TX_HASH="$2"
      shift 2
      ;;
    --report-out)
      REPORT_OUT="$2"
      shift 2
      ;;
    --gocache)
      GOCACHE_DIR="$2"
      shift 2
      ;;
    --go-bin)
      GO_BIN="$2"
      shift 2
      ;;
    --no-prior-block)
      TRACE_PRIOR_BLOCK=false
      shift
      ;;
    --compare-payload)
      COMPARE_PAYLOAD=true
      shift
      ;;
    --compare-rpc-receipts)
      COMPARE_RPC_RECEIPTS=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "$TX_HASH" ]]; then
  echo "Error: --tx is required" >&2
  usage >&2
  exit 1
fi

if [[ ! -d "$REPO_ROOT/op-chain-ops/cmd/sdm-inspect-tx" ]]; then
  echo "Error: could not locate repo root from script path: $REPO_ROOT" >&2
  exit 1
fi

safe_tx_hash="${TX_HASH//[^A-Za-z0-9._-]/_}"
if [[ -z "$REPORT_OUT" ]]; then
  REPORT_OUT="/tmp/sdm-inspect-tx-${safe_tx_hash}.md"
fi

mkdir -p "$GOCACHE_DIR"
mkdir -p "$(dirname "$REPORT_OUT")"

echo "RPC endpoint: ${RPC_URL}"
echo "Tx hash: ${TX_HASH}"
echo "Report output: ${REPORT_OUT}"
echo "Repo root: ${REPO_ROOT}"

cmd=(
  env GOCACHE="$GOCACHE_DIR"
  "$GO_BIN" run ./op-chain-ops/cmd/sdm-inspect-tx
  --rpc "$RPC_URL"
  --tx-hash "$TX_HASH"
  --out "$REPORT_OUT"
)

if [[ "$TRACE_PRIOR_BLOCK" == "true" ]]; then
  cmd+=(--trace-prior-block=true)
else
  cmd+=(--trace-prior-block=false)
fi
if [[ "$COMPARE_PAYLOAD" == "true" ]]; then
  cmd+=(--compare-payload)
fi
if [[ "$COMPARE_RPC_RECEIPTS" == "true" ]]; then
  cmd+=(--compare-rpc-receipts)
fi

(
  cd "$REPO_ROOT"
  "${cmd[@]}"
)

echo "Done."
echo ""
echo "vim ${REPORT_OUT}"
