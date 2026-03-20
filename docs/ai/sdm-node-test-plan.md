# SDM Node Test And Replay Utility Plan

## Scope

This document proposes:

1. A node-level test strategy for the SDM feature.
2. A new CLI utility that replays a block or block range and calculates SDM gas savings at block granularity.

This is a planning document only. It does not propose implementation in this change.

## Goals

- Validate SDM against a real node, not only synthetic per-tx traces.
- Measure SDM savings per transaction and per block using the same core accounting model as `op-chain-ops/cmd/sdm-profiler`.
- Support replaying one block or a contiguous block range from an RPC endpoint.
- Produce machine-readable output that can be diffed across node versions and replay modes.
- Reuse existing repo patterns where possible instead of creating a parallel tracing stack.

## Non-Goals

- Shipping fork-gating changes for SDM itself.
- Building a generalized block debugger for arbitrary protocol features.
- Depending on ad hoc node patches or unstable private RPCs if existing replay/building code can be reused.
- Designing a full dashboard or visualization layer in the first pass.

## Existing Anchors In Repo

- `op-chain-ops/cmd/sdm-profiler`
  - Already defines the output model for per-tx SDM savings.
  - Already resolves block ranges and writes JSONL summaries.
  - Today it depends on `debug_traceBlockByNumber` and a custom tracer.
- `op-chain-ops/cmd/op-run-block`
  - Already fetches remote state, chain config, parent headers, and replays a block locally.
  - This is the right base for deterministic block re-execution.
- `op-acceptance-tests/tests/sdm_reth`
  - Already contains node-oriented SDM smoke tests for presence of the SDM tx, receipt refunds, and multi-tx batching behavior.
- `rust/op-alloy/crates/consensus/src/sdm.rs`
  - Defines `SDMPayload`, `SDMGasEntry`, `SDM_TX_TYPE_ID`, and payload extraction semantics.
- `rust/op-reth/crates/rpc/src/eth/receipt.rs`
  - Maps block-level SDM payload entries back onto receipts via `opGasRefund`.

## Recommended Deliverables

### 1. New CLI utility

Add a new command under `op-chain-ops/cmd/sdm-replay`.

Reasoning:

- `sdm-profiler` is profiler-first and RPC-tracer-first.
- `op-run-block` is replay-first and state-transition-first.
- SDM block replay is a different workflow from both, and deserves a distinct entrypoint.
- The new tool can still reuse types and helpers from both commands after some internal refactoring.

### 2. Shared replay/profiling package

Create a small internal package, likely under `op-chain-ops/pkg/sdmreplay` or `op-chain-ops/cmd/sdm-replay/internal`, for:

- block selection and range iteration
- RPC data loading
- SDM payload extraction
- per-tx and per-block aggregation
- JSONL output structs

Do not duplicate the logic currently buried inside `sdm-profiler/main.go` and `op-run-block/main.go`.

### 3. Node-level acceptance coverage

Expand node-oriented SDM validation so it covers:

- SDM-inactive behavior
- SDM-active behavior
- block replay agreement with node RPC receipts
- range replay summaries over several consecutive blocks

## CLI Design

### Command name

`sdm-replay`

### Primary usage

```bash
go run ./op-chain-ops/cmd/sdm-replay \
  --rpc http://127.0.0.1:8545 \
  --from-block 12345 \
  --to-block 12360 \
  --out /tmp/sdm-replay.jsonl
```

### Required flags

- `--rpc`
  - L2 RPC endpoint used for block, receipt, header, state, and chain config access.
- `--from-block`
  - Start block. Support decimal, hex, `latest`, and `latest-N`.
- `--to-block`
  - End block. Same syntax as `--from-block`.
- `--out`
  - JSONL output path.

### Optional flags

- `--compare-rpc-receipts`
  - Compare replay-computed refunds against receipt `opGasRefund` values when present.
- `--fail-on-mismatch`
  - Exit non-zero if replay disagrees with node receipts or SDM payload contents.
- `--skip-empty-blocks`
  - Skip blocks with no user txs after deposits.
- `--include-trace`
  - Emit extra per-access or per-op debug detail only for targeted debugging.
- `--summary-only`
  - Write block/range summaries without per-tx rows.
- `--workers`
  - Parallelize block fetching or replay only if determinism and RPC pressure are controlled. Default should be `1` initially.
- `--format`
  - `jsonl` initially, with room for `json` later if needed.

### Exit behavior

- `0` if replay completed successfully.
- non-zero for invalid range, RPC failures, replay failures, or mismatch failures when `--fail-on-mismatch` is set.

## Output Model

Keep the output JSONL-first and intentionally similar to `sdm-profiler`.

### Record types

- `run_config`
  - command-line inputs, chain id, head block at start, client version
- `block`
  - one record per replayed block
- `tx`
  - one record per replayed user tx
- `summary`
  - aggregate over the whole requested range
- `mismatch`
  - explicit row when replay output does not match payload or receipt data

### Per-block fields

- `block_num`
- `block_hash`
- `parent_hash`
- `tx_count_total`
- `tx_count_user`
- `sdm_tx_present`
- `sdm_payload_entry_count`
- `block_gas_used`
- `block_op_gas_refund`
- `block_effective_gas`
- `avg_refund_ratio`
- `node_receipt_refund_total`
- `replay_refund_total`
- `payload_refund_total`

### Per-tx fields

- `block_num`
- `tx_index`
- `tx_hash`
- `tx_type`
- `from`
- `to`
- `gas_used`
- `op_gas_refund_replay`
- `op_gas_refund_receipt`
- `op_gas_refund_payload`
- `effective_gas`
- `refund_ratio`
- `status`
- `is_sdm_tx`
- `is_deposit_tx`
- `mismatch`

## Replay Algorithm

### High-level flow

1. Resolve `from` and `to` block numbers.
2. Fetch chain config and current client version from RPC.
3. For each block in the range:
4. Fetch the full block, parent header, and receipts.
5. Build local replay context from parent state, similar to `op-run-block`.
6. Re-execute the block locally with SDM-aware execution.
7. Extract replay-computed per-tx refunds and per-block totals.
8. Extract SDM payload from the synthetic `0x7d` tx if present.
9. Compare:
   - replay results vs payload entries
   - replay results vs receipt `opGasRefund`
10. Emit per-tx and per-block records.
11. Emit end-of-range summary.

### Important semantic rules

- Deposit txs and the synthetic SDM tx must be excluded from user-refund accounting.
- The synthetic SDM tx should be treated as metadata, not as a user transaction.
- The tool should preserve tx ordering exactly as seen in the block.
- Refund mapping must be by transaction index, matching `SDMGasEntry.index`.
- Blocks without an SDM tx should still produce valid output with zero refunds.

## Implementation Strategy

### Phase 1. Extract reusable pieces

Refactor existing code, without changing behavior, into shared helpers:

- block range parsing from `sdm-profiler`
- RPC transport and block-number resolution from `sdm-profiler`
- block replay setup from `op-run-block`
- JSONL writer and summary generation from `sdm-profiler`

Acceptance criteria:

- no behavior change in existing commands
- helper package has unit coverage for range parsing and output encoding

### Phase 2. Single-block replay

Implement replay for exactly one block:

- fetch block and receipts
- replay block locally
- emit per-tx records
- emit a single block summary
- compare with payload and receipts when available

Acceptance criteria:

- works on a known SDM-active block
- works on a known SDM-inactive block
- handles blocks with no user txs

### Phase 3. Block-range replay

Add range iteration and top-level summary:

- sequential replay over `from..to`
- aggregated totals and ratios
- stable JSONL output ordering

Acceptance criteria:

- results deterministic across repeated runs
- range summary equals sum of per-block summaries

### Phase 4. Hardening and ergonomics

- add clearer mismatch diagnostics
- add `--summary-only`
- add bounded parallel fetch if needed
- add README examples

Acceptance criteria:

- can be used in CI or ad hoc analysis without manual post-processing

## Node Test Plan

### A. Devstack smoke test

Use an in-process devstack node test as the fastest correctness loop.

Recommended topology:

- single-chain mixed runtime
- `op-reth` as the sequencer EL
- `op-node` as the CL
- batcher stopped to maximize control over tx ordering where needed

Scenarios:

1. SDM inactive block
   - replay block with ordinary txs
   - assert no synthetic SDM tx
   - assert all replay refunds are zero
2. SDM active repeated-slot block
   - send repeated storage-writing txs in one block
   - assert synthetic SDM tx exists
   - assert replay refunds are non-zero for later txs
3. Mixed-category block
   - transfer + compute + event + storage writes
   - assert replay does not attribute refunds to unrelated txs incorrectly
4. Empty-ish block
   - deposit only or one user tx
   - assert zero or trivial refund behavior

### B. Golden block replay tests

Store a small set of captured block fixtures and expected summary JSON.

Fixture classes:

- no SDM tx
- SDM tx with several entries
- SDM tx present but zero effective savings
- malformed or unexpected SDM payload for negative coverage

Assertions:

- per-tx refund mapping
- block refund total
- payload-entry count
- mismatch detection

### C. RPC agreement tests

Against a live node exposing `opGasRefund` on receipts:

- replay block
- fetch receipts
- assert `op_gas_refund_replay == op_gas_refund_receipt`

If exact agreement is not possible because of known implementation gaps, document those gaps explicitly and fail only behind a strict flag at first.

### D. Range-level regression tests

Run replay over a small range such as 10 to 50 blocks and assert:

- total refund is stable across reruns
- no panics or malformed rows
- summary equals sum of tx rows
- mismatches remain zero for known-good fixtures

## Test Matrix

### Functional matrix

- SDM inactive / active
- single block / multi-block range
- zero refunds / non-zero refunds
- payload present / absent
- receipt `opGasRefund` present / absent

### Transaction mix matrix

- transfer-heavy
- storage-heavy repeated-slot
- compute-heavy
- mixed-category
- blocks containing deposits and the synthetic SDM tx

### Failure matrix

- RPC timeout
- missing block in range
- malformed receipt response
- malformed or undecodable SDM payload
- replay mismatch with receipt values
- replay mismatch with payload entries

## Suggested Reuse And Refactors

### Reuse from `sdm-profiler`

- block range syntax
- JSONL summary style
- user-facing output conventions

### Reuse from `op-run-block`

- remote chain config loading
- parent-state reconstruction
- local block execution harness

### Refactors worth doing first

- isolate JSON-RPC helpers into shared code
- isolate output record types into a shared package
- make replay engine injectable so tests can use fixtures without live RPC for every case

## Risks

### Replay fidelity risk

If the local replay path does not use the exact SDM-aware execution logic as the node, the tool may disagree with real receipts while still appearing internally consistent.

Mitigation:

- prefer reusing the exact same execution libraries already used by the node where possible
- add agreement tests against `opGasRefund` receipts

### RPC dependency risk

Range replay may be slow or flaky if it repeatedly fetches headers, blocks, receipts, and state over remote RPC.

Mitigation:

- keep initial version sequential and deterministic
- add caching only after correctness is established

### Semantic drift risk

`sdm-profiler` and `sdm-replay` can drift if they compute savings differently.

Mitigation:

- define a single shared savings calculator or shared output contract
- add a fixture-based equivalence test for per-tx accounting on the same block

## Acceptance Criteria For The Overall Effort

- A user can point `sdm-replay` at a node and replay one block or a block range.
- The tool emits per-tx and per-block SDM savings in JSONL.
- The tool identifies whether the block contains the synthetic SDM tx.
- The tool can compare replay output against payload entries and receipt `opGasRefund`.
- There are automated tests covering inactive and active SDM behavior.
- There is a short command README with example invocations and output semantics.

## Recommended Order Of Work

1. Refactor shared helpers out of `sdm-profiler` and `op-run-block`.
2. Implement single-block `sdm-replay`.
3. Add fixture and live-node agreement tests.
4. Add block-range replay and aggregate summaries.
5. Add README and optional strict mismatch mode.

## Open Questions To Resolve Before Implementation

- Should the tool replay using go-ethereum execution code only, or should it call into SDM-aware OP-specific execution libraries directly if available?
- Is the authoritative refund source the replay result, the SDM payload, or receipt `opGasRefund` when they disagree?
- Do we want the first version to support only `op-reth`, or any node that exposes enough RPC to reconstruct replay inputs?
- Should range replay stop on first mismatch, or emit mismatch records and continue by default?
