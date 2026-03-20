# OP-Reth SDM Counterfactual Replay Plan

## Goal

Add a native `op-reth` replay path that can take historical blocks from a chain where SDM was not live and compute counterfactual SDM refunds as if SDM had been enabled during execution.

This is the correct approach for historical analysis because the SDM accounting logic already lives in the Rust execution stack, not in the Go tooling.

## Recommendation

Implement option 2 as an `op-reth`-native replay engine with two frontends:

1. `debug_replaySdmBlock` RPC for single-block counterfactual replay
2. `op-reth sdm-replay` CLI for block ranges and JSONL output

The core logic should live once in Rust and be shared by both frontends.

The existing Go `sdm-replay` can remain as a validator or external helper, but it should not be the authoritative historical replay path.

## Why This Shape

The SDM accounting logic already exists in the Rust execution path:

- `rust/alloy-op-evm/src/block/mod.rs`
- `rust/op-reth/crates/payload/src/builder.rs`
- `rust/op-reth/crates/rpc/src/eth/receipt.rs`

The existing custom debug RPC path already knows how to:

- fetch a recovered historical block
- reconstruct parent state
- execute through the OP EVM

That pattern exists in `rust/op-reth/crates/rpc/src/debug.rs` and is the right insertion point for a single-block replay RPC.

## Execution Model

For a historical block `N`:

1. Load the original block from local DB by number or hash.
2. Build parent state from block `N-1`.
3. Strip any existing synthetic SDM tx from the block body before replay.
4. Re-execute the original tx list through the OP EVM with SDM forced on.
5. Read the executor’s computed SDM entries.
6. Build:
   - per-tx replay refund rows
   - block summary
   - synthetic payload bytes that would have been appended
7. If the source block already contains SDM data, compare:
   - replay entries vs embedded payload
   - replay entries vs receipt `opGasRefund`

The replay result is authoritative. Payload and receipts are comparison targets.

## Core Refactor Needed First

Today the executor can already accumulate SDM entries via:

- `enable_sdm`
- `enable_sdm_verifier`
- `take_sdm_entries`

Those methods already exist in `rust/alloy-op-evm/src/block/mod.rs`.

The first job is to expose them cleanly from a replay path instead of leaving them implicitly tied to payload building.

Create a small reusable replay crate, likely:

- `rust/op-reth/crates/sdm-replay`

This crate should provide:

- block loading and SDM-tx stripping
- parent-state execution harness
- forced-SDM replay mode
- result structs
- payload synthesis and comparison helpers

Do not bury this logic inside RPC handlers.

## Proposed Result Types

Use Rust-native structs that mirror the JSONL contract expected by tooling:

- `SdmReplayTx`
- `SdmReplayBlock`
- `SdmReplaySummary`
- `SdmReplayMismatch`
- `SdmReplayRunConfig`

Important fields:

- original block number and hash
- tx index in original block
- tx type
- deposit and SDM flags
- gas used
- replay refund
- payload refund
- receipt refund
- effective gas
- mismatch reason
- synthesized payload bytes and synthesized entries

## RPC Plan

Extend `rust/op-reth/crates/rpc/src/debug.rs` with:

- `debug_replaySdmBlock(block, options) -> SdmReplayBlockResult`

Recommended options:

- `force_sdm: bool` default `true`
- `compare_payload: bool`
- `compare_receipts: bool`
- `include_trace_detail: bool`

Do not add range replay to RPC in phase 1. Range jobs are long-running and are better handled by a local CLI over direct DB access.

Wire the method through `rust/op-reth/crates/node/src/node.rs` alongside the existing debug extension.

## CLI Plan

Add a new subcommand under `rust/op-reth/crates/cli/src/commands/mod.rs`:

- `op-reth sdm-replay`

Example:

```bash
op-reth sdm-replay \
  --datadir /path/to/datadir \
  --from-block 12345 \
  --to-block 12360 \
  --out /tmp/sdm-replay.jsonl \
  --compare-receipts
```

The CLI should:

- open the local DB directly
- iterate sequentially over a block range
- call the shared replay crate
- emit JSONL in stable order

This is the correct tool for large historical studies.

## Key Implementation Details

- Replay the original block without injecting the synthetic SDM tx into execution.
  The synthetic tx should be synthesized after replay from computed entries.
- Preserve original tx indexes.
  If a block already has an SDM tx, exclude it from replay but keep index mapping against the original block layout.
- Deposit txs warm state but never receive refunds.
- Receipt comparison for historical non-SDM blocks will usually be absent, which is expected.
- The replay path must be explicitly "counterfactual SDM enabled", independent of canonical fork activation.

That last point likely needs a small replay config object rather than relying on the current PoC always-on behavior.

## Development Priority

Bias toward the first runnable integration test over broad unit coverage.

The preferred order is:

1. Build the minimum shared replay path needed to replay one block.
2. Expose it through a single-block debug RPC.
3. Add one end-to-end devnet test that proves the replay path works.
4. Only then add the smallest set of unit tests needed to keep the replay core from regressing.
5. Add the range CLI after the single-block integration path is proven.

Avoid spending time on broad fixture matrices before there is a concrete integration path you can run locally.

## Phased Delivery

### Phase 1. Minimum Replay Core

- Expose a reusable "replay block and return SDM entries" API from the Rust execution stack.
- Keep this as narrow as possible: one block in, replay result out.
- Do not build full range or JSONL support yet.
- Add only enough internal assertions or one narrow unit test to keep the core compile-safe if needed.

### Phase 2. Single-Block RPC

- Add `debug_replaySdmBlock`.
- Return replay entries, synthesized payload, and mismatch diagnostics.
- Validate it through a runnable devnet integration test, not through a large unit-test matrix.

### Phase 3. First Runnable Integration Test

- Extend `op-acceptance-tests/tests/sdm_reth`.
- Spin up the local op-reth-backed devnet.
- Submit repeated-slot transactions.
- Capture the target block.
- Call `debug_replaySdmBlock`.
- Assert:
  - replay refund is non-zero for later repeated-slot txs
  - synthesized payload entries match receipt refunds
  - tx index mapping is exact

At this point the feature is considered real enough for iterative development.

### Phase 4. Minimum Unit Coverage

- Add only a small number of unit tests for the replay core.
- Focus on the logic that is hardest to debug through the devnet test:
  - stripping an existing SDM tx before replay
  - preserving original tx indexes
  - decoding and comparing embedded payload entries

Do not build a large matrix yet.

### Phase 5. Range CLI

- Add `op-reth sdm-replay`.
- Emit JSONL.
- Add `--summary-only`
- Add `--fail-on-mismatch`
- Add `--skip-empty-blocks`

### Phase 6. Go Tool Integration

- Either deprecate the current Go `sdm-replay` for counterfactual work, or make it a thin client for the new RPC.
- Keep the current Go payload and receipt validator as a lightweight external tool if useful.

## Test Plan

### First Required Integration Test

Extend `op-acceptance-tests/tests/sdm_reth` to:

- run a tx burst
- capture the block
- call the new debug RPC
- assert:
  - replay refund is non-zero for later repeated-slot txs
  - synthesized payload entries match receipt refunds
  - tx index mapping is exact

### Minimal Unit Tests After Integration

Once the integration test exists, add only a few unit tests:

- existing SDM tx is stripped before replay
- original tx indexes are preserved
- malformed SDM payload produces the expected mismatch

### Later Integration Coverage

After the first end-to-end path is stable, add:

- comparison against receipt `opGasRefund` on SDM-active blocks
- replay of historical non-SDM blocks and stability across reruns

## Main Risks

- The hardest part is extracting SDM entries from executor output cleanly without coupling to payload-builder-only code.
- Index mapping gets tricky for blocks that already include a synthetic SDM tx.
- Range replay over RPC will be too slow, so keep that local-only in the CLI.

## Success Criteria

You can point `op-reth sdm-replay` at a local datadir containing ordinary historical blocks, and it will compute counterfactual SDM refunds even when SDM was never live on-chain.
