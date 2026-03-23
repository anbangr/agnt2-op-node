#!/usr/bin/env bash
set -euo pipefail

# Replay a single block with sdm-replay and generate a markdown inspection report.
#
# Examples:
#   op-acceptance-tests/scripts/run-sdm-replay-inspect-block.sh --block latest
#   op-acceptance-tests/scripts/run-sdm-replay-inspect-block.sh --block 12345678 \
#     --rpc http://100.126.224.125:8545 \
#     --jsonl-out /tmp/sdm-replay-block-12345678.jsonl \
#     --report-out /tmp/sdm-replay-block-12345678.md
#   op-acceptance-tests/scripts/run-sdm-replay-inspect-block.sh --block 12345678 -- --compare-rpc-receipts

RPC_URL="${RPC_URL:-http://100.126.224.125:8545}"
BLOCK_SELECTOR="${BLOCK_SELECTOR:-latest}"
JSONL_OUT="${JSONL_OUT:-}"
REPORT_OUT="${REPORT_OUT:-}"
GOCACHE_DIR="${GOCACHE_DIR:-/tmp/optimism-codex-gocache}"
GO_BIN="${GO_BIN:-go}"
PYTHON_BIN="${PYTHON_BIN:-python3}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"

REPLAY_EXTRA_ARGS=()

usage() {
  cat <<EOF
Usage: $0 [options] [-- <extra sdm-replay args>]

Options:
  --rpc URL             RPC endpoint (default: ${RPC_URL})
  --block SELECTOR      Block to inspect (default: ${BLOCK_SELECTOR})
                        Accepts decimal, hex, latest, or latest-N
  --jsonl-out PATH      JSONL output path (default: /tmp/sdm-replay-inspect-<block>.jsonl)
  --report-out PATH     Markdown report path (default: /tmp/sdm-replay-inspect-<block>.md)
  --gocache PATH        GOCACHE directory for go run (default: ${GOCACHE_DIR})
  --go-bin PATH         Go binary to use (default: ${GO_BIN})
  --python-bin PATH     Python binary to use (default: ${PYTHON_BIN})
  -h, --help            Show this help

Any arguments after -- are passed through to ./op-chain-ops/cmd/sdm-replay.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --rpc)
      RPC_URL="$2"
      shift 2
      ;;
    --block)
      BLOCK_SELECTOR="$2"
      shift 2
      ;;
    --jsonl-out)
      JSONL_OUT="$2"
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
    --python-bin)
      PYTHON_BIN="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      REPLAY_EXTRA_ARGS=("$@")
      break
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ ! -d "$REPO_ROOT/op-chain-ops/cmd/sdm-replay" ]]; then
  echo "Error: could not locate repo root from script path: $REPO_ROOT" >&2
  exit 1
fi

safe_block_selector="${BLOCK_SELECTOR//[^A-Za-z0-9._-]/_}"
if [[ -z "$JSONL_OUT" ]]; then
  JSONL_OUT="/tmp/sdm-replay-inspect-${safe_block_selector}.jsonl"
fi
if [[ -z "$REPORT_OUT" ]]; then
  REPORT_OUT="/tmp/sdm-replay-inspect-${safe_block_selector}.md"
fi

mkdir -p "$GOCACHE_DIR"
mkdir -p "$(dirname "$JSONL_OUT")"
mkdir -p "$(dirname "$REPORT_OUT")"

echo "RPC endpoint: ${RPC_URL}"
echo "Block selector: ${BLOCK_SELECTOR}"
echo "JSONL output: ${JSONL_OUT}"
echo "Report output: ${REPORT_OUT}"
echo "Repo root: ${REPO_ROOT}"

replay_cmd=(
  env GOCACHE="$GOCACHE_DIR"
  "$GO_BIN" run ./op-chain-ops/cmd/sdm-replay
  --rpc "$RPC_URL"
  --from-block "$BLOCK_SELECTOR"
  --to-block "$BLOCK_SELECTOR"
  --out "$JSONL_OUT"
)

if (( ${#REPLAY_EXTRA_ARGS[@]} > 0 )); then
  replay_cmd+=("${REPLAY_EXTRA_ARGS[@]}")
fi

(
  cd "$REPO_ROOT"
  "${replay_cmd[@]}"
  "$PYTHON_BIN" op-acceptance-tests/tests/sdm/inspect_block.py \
    --input "$JSONL_OUT" \
    --output "$REPORT_OUT" \
    --rpc "$RPC_URL"
)

echo "Done."
echo "vim ${REPORT_OUT}"
