# OP-Reth SDM Counterfactual Replay Implementation Checklist

## Objective

Build a native Rust replay path in `op-reth` that can compute counterfactual SDM refunds for historical blocks, expose single-block replay through RPC, and expose range replay through a local CLI.

## Development Rule

Prioritize the first runnable integration test over broad unit coverage.

Until `debug_replaySdmBlock` can be exercised from `op-acceptance-tests/tests/sdm_reth`, keep unit tests to the bare minimum needed to support refactoring safely.

## Phase 1: Shared Replay Crate

### 1. Create the crate

- Add a new crate:
  - `rust/op-reth/crates/sdm-replay`
- Add it to the relevant Cargo workspace manifests.
- Keep the crate focused on replay logic, result types, and JSONL output support.

### 2. Define public result types

- Add Rust structs for:
  - `SdmReplayConfig`
  - `SdmReplayTx`
  - `SdmReplayBlock`
  - `SdmReplaySummary`
  - `SdmReplayMismatch`
  - `SdmReplayRunConfig`
- Derive `Serialize` and `Deserialize` where useful for RPC and CLI output.
- Make field names align with existing Go JSONL naming where practical.

### 3. Add block normalization helpers

- Add a helper that scans a block body and:
  - detects whether an SDM tx exists
  - decodes any embedded SDM payload
  - strips the SDM tx from the tx list used for replay
- Preserve original tx ordering metadata so comparisons still use original block indexes.

### 4. Add payload helpers

- Reuse the existing `SDMPayload`, `SDMGasEntry`, and `TxSdm` types from `op-alloy`.
- Add helpers to:
  - decode embedded SDM payload
  - synthesize the payload that replay would generate
  - compare payload entries by original tx index

### 5. Add replay executor wrapper

- Build a replay function that accepts:
  - block reference
  - parent state provider
  - replay config
- Re-execute the normalized block through the OP EVM with SDM forced on.
- Return:
  - receipts from replay
  - executor-produced SDM entries
  - synthesized payload
  - per-tx replay accounting rows

### 6. Expose SDM entries cleanly from executor flow

- Audit `rust/alloy-op-evm/src/block/mod.rs`.
- Ensure the replay path can:
  - enable SDM explicitly
  - execute without payload-builder coupling
  - retrieve `sdm_entries` after block execution
- If needed, add a small execution result wrapper that carries:
  - receipts
  - gas used
  - `sdm_entries`

### 7. Add counterfactual mode semantics

- Introduce explicit replay configuration instead of relying on PoC always-on behavior.
- Add a mode like:
  - `SdmMode::Disabled`
  - `SdmMode::CounterfactualEnabled`
  - `SdmMode::Verifier(payload)`
- For historical analysis, use `CounterfactualEnabled`.

## Phase 2: Single-Block RPC

### 8. Add RPC request and response types

- In `rust/op-reth/crates/rpc`, add types for:
  - `ReplaySdmBlockRequest`
  - `ReplaySdmBlockOptions`
  - `ReplaySdmBlockResponse`
- Support lookup by block number or hash.

### 9. Extend debug RPC trait

- Update `rust/op-reth/crates/rpc/src/debug.rs`.
- Add:
  - `debug_replaySdmBlock`
- Keep it in the `debug` namespace.

### 10. Implement RPC handler

- Use the existing debug RPC pattern:
  - load recovered historical block
  - build state provider rooted at parent block
  - run the shared replay crate
- Return:
  - per-tx rows
  - block summary
  - synthesized payload
  - mismatch rows

### 11. Register the new RPC

- Wire the new method through:
  - `rust/op-reth/crates/node/src/node.rs`
- Keep the method behind the existing debug RPC module config.

## Phase 3: CLI Range Replay

### 12. Add CLI command

- Extend:
  - `rust/op-reth/crates/cli/src/commands/mod.rs`
- Add:
  - `sdm-replay`

### 13. Add CLI arguments

- Support:
  - `--from-block`
  - `--to-block`
  - `--out`
  - `--summary-only`
  - `--compare-payload`
  - `--compare-receipts`
  - `--fail-on-mismatch`
  - `--skip-empty-blocks`
- Keep replay sequential in the first pass.

### 14. Open local DB directly

- Reuse existing `op-reth` CLI patterns for opening provider and chain context.
- Do not route the CLI through JSON-RPC.

### 15. Emit JSONL

- Add a JSONL writer in the shared replay crate or CLI crate.
- Emit stable order:
  - `run_config`
  - `tx`
  - `block`
  - `mismatch`
  - `summary`

## Phase 4: Comparisons and Mismatch Semantics

### 16. Compare against embedded SDM payload

- If source block contains an SDM tx:
  - compare replay entries to payload entries
  - compare synthesized payload bytes to expected structure if useful
- Emit mismatch rows for:
  - duplicate indexes
  - out-of-range indexes
  - non-user tx targets
  - refund value disagreements

### 17. Compare against receipt `opGasRefund`

- If receipts expose `opGasRefund`:
  - compare replay refund to receipt refund
- Treat replay result as authoritative.
- Make failure behavior configurable with `--fail-on-mismatch`.

## Phase 5: Tests

### 18. Add the first runnable integration test

- Extend `op-acceptance-tests/tests/sdm_reth`.
- Add one test that:
  - submits repeated-slot txs
  - captures the target block
  - calls `debug_replaySdmBlock`
  - asserts replay refunds are non-zero where expected
  - asserts synthesized payload entries match receipt refunds
  - asserts tx index mapping is exact

Do this before expanding unit coverage.

### 19. Add only the minimum unit tests

- Add a small number of focused tests in the shared replay crate:
  - existing SDM tx is stripped before replay
  - original tx indexes are preserved
  - malformed SDM payload produces the expected mismatch

Skip the larger scenario matrix until after the integration path is proven.

### 20. Add RPC tests

- Add tests around:
  - request parsing
  - block lookup by number and hash
  - mismatch generation
  - successful replay response encoding

### 21. Add CLI tests

- Add smoke tests for:
  - single block replay
  - small range replay
  - summary-only mode
  - fail-on-mismatch mode

## Phase 6: Tooling Integration

### 22. Decide how Go tooling should interact

- Either:
  - make Go `sdm-replay` a thin client over `debug_replaySdmBlock`
- Or:
  - document that Go `sdm-replay` is for payload and receipt validation only

### 23. Update docs

- Add a short `op-reth` README section with:
  - CLI usage
  - RPC usage
  - meaning of counterfactual mode
  - mismatch semantics

## Suggested File Targets

- `rust/op-reth/crates/sdm-replay/src/lib.rs`
- `rust/op-reth/crates/sdm-replay/src/types.rs`
- `rust/op-reth/crates/sdm-replay/src/replay.rs`
- `rust/op-reth/crates/sdm-replay/src/payload.rs`
- `rust/op-reth/crates/sdm-replay/src/jsonl.rs`
- `rust/op-reth/crates/rpc/src/debug.rs`
- `rust/op-reth/crates/rpc/src/lib.rs`
- `rust/op-reth/crates/node/src/node.rs`
- `rust/op-reth/crates/cli/src/commands/mod.rs`
- `rust/op-reth/crates/cli/src/commands/sdm_replay.rs`
- `rust/alloy-op-evm/src/block/mod.rs`
- `op-acceptance-tests/tests/sdm_reth/...`

## Exit Criteria

- A single historical block can be replayed through `debug_replaySdmBlock`.
- A block range can be replayed through `op-reth sdm-replay`.
- Counterfactual replay works even for blocks from periods where SDM was never live on-chain.
- Replay output includes synthesized payload and mismatch diagnostics.
- Automated tests cover the replay engine, RPC surface, and at least one devnet end-to-end path.
