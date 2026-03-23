#!/usr/bin/env bash
set -euo pipefail

# Run sdm-replay over a recent block window and generate a PNG report.
#
# Examples:
#   op-acceptance-tests/scripts/run-sdm-replay-window.sh
#   op-acceptance-tests/scripts/run-sdm-replay-window.sh --count 100 \
#     --rpc http://100.126.224.125:8545 \
#     --jsonl-out /tmp/sdm-replay-latest-new.jsonl \
#     --png-out /tmp/sdm-replay-latest-new.png
#   BLOCK_COUNT=250 op-acceptance-tests/scripts/run-sdm-replay-window.sh
#   op-acceptance-tests/scripts/run-sdm-replay-window.sh -- --compare-rpc-receipts

RPC_URL="${RPC_URL:-http://100.126.224.125:8545}"
BLOCK_COUNT="${BLOCK_COUNT:-100}"
JSONL_OUT="${JSONL_OUT:-/tmp/sdm-replay-latest-${BLOCK_COUNT}.jsonl}"
PNG_OUT="${PNG_OUT:-/tmp/sdm-replay-latest-${BLOCK_COUNT}.png}"
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
  --count N             Number of latest blocks to replay (default: ${BLOCK_COUNT})
  --jsonl-out PATH      JSONL output path (default: ${JSONL_OUT})
  --png-out PATH        PNG output path (default: ${PNG_OUT})
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
    --count)
      BLOCK_COUNT="$2"
      shift 2
      ;;
    --jsonl-out)
      JSONL_OUT="$2"
      shift 2
      ;;
    --png-out)
      PNG_OUT="$2"
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

if ! [[ "$BLOCK_COUNT" =~ ^[0-9]+$ ]] || [[ "$BLOCK_COUNT" -lt 1 ]]; then
  echo "Error: --count must be a positive integer" >&2
  exit 1
fi

if [[ ! -d "$REPO_ROOT/op-chain-ops/cmd/sdm-replay" ]]; then
  echo "Error: could not locate repo root from script path: $REPO_ROOT" >&2
  exit 1
fi

mkdir -p "$GOCACHE_DIR"
mkdir -p "$(dirname "$JSONL_OUT")"
mkdir -p "$(dirname "$PNG_OUT")"

block_response=$(curl -fsS \
  -H 'Content-Type: application/json' \
  --data '{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}' \
  "$RPC_URL")

block_hex=$(
  "$PYTHON_BIN" - "$block_response" <<'PY'
import json
import sys

payload = json.loads(sys.argv[1])
if "error" in payload:
    raise SystemExit(f"RPC error: {payload['error']}")
result = payload.get("result")
if not isinstance(result, str) or not result.startswith("0x"):
    raise SystemExit(f"Unexpected eth_blockNumber response: {payload}")
print(result)
PY
)

latest_block=$((16#${block_hex#0x}))
from_block=$((latest_block - BLOCK_COUNT + 1))
if (( from_block < 0 )); then
  from_block=0
fi
actual_block_count=$((latest_block - from_block + 1))

echo "RPC endpoint: ${RPC_URL}"
echo "Latest block: ${latest_block} (${block_hex})"
echo "Replay range: ${from_block}..${latest_block} (${actual_block_count} blocks)"
echo "JSONL output: ${JSONL_OUT}"
echo "PNG output: ${PNG_OUT}"
echo "Repo root: ${REPO_ROOT}"

replay_cmd=(
  env GOCACHE="$GOCACHE_DIR"
  "$GO_BIN" run ./op-chain-ops/cmd/sdm-replay
  --rpc "$RPC_URL"
  --from-block "$from_block"
  --to-block "$latest_block"
  --out "$JSONL_OUT"
)

if (( ${#REPLAY_EXTRA_ARGS[@]} > 0 )); then
  replay_cmd+=("${REPLAY_EXTRA_ARGS[@]}")
fi

(
  cd "$REPO_ROOT"
  "${replay_cmd[@]}"
  "$PYTHON_BIN" op-acceptance-tests/tests/sdm/visualize.py \
    --input "$JSONL_OUT" \
    --output "$PNG_OUT"
)

echo "Done."
echo ""
echo "open ${PNG_OUT}"
