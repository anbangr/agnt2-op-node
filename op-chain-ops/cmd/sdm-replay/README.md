# sdm-replay

`sdm-replay` replays SDM accounting over one block or a contiguous block range and writes JSONL records for:

- `run_config`
- `tx`
- `block`
- `mismatch`
- `summary`

Primary usage:

```bash
go run ./op-chain-ops/cmd/sdm-replay \
  --rpc http://127.0.0.1:8545 \
  --from-block latest-10 \
  --to-block latest \
  --out /tmp/sdm-replay.jsonl \
  --compare-payload \
  --compare-rpc-receipts
```

Notes:

- The tool requires a modified `op-reth` node that exposes `debug_replaySdmBlock`.
- Refund accounting comes from the node's counterfactual replay result and records `replay_mode: "counterfactual_enabled"`.
- `--compare-payload` is usually off for historical pre-SDM blocks, and on when validating a block that already contains an SDM payload.
- `--include-trace` is reserved for the older tracer-based path and is not supported by the `debug_replaySdmBlock` flow.
- `--summary-only` suppresses `tx` rows but still emits `mismatch` rows.
