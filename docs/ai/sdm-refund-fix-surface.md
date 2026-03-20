# SDM Refund Fix Surface

## What Changed

The fix changes where SDM refunds are computed.

Before, [rust/alloy-op-evm/src/block/mod.rs](../../rust/alloy-op-evm/src/block/mod.rs) estimated savings after execution by walking the touched state and adding:

- `2500` for any account already warm from a prior tx
- `2000` for any storage slot already warm from a prior tx

That could over-refund storage-heavy txs, because a repeated storage access could effectively pick up both the account rebate and the slot rebate, and it could not distinguish `SLOAD` from `SSTORE`.

Now the refund is tracked at the actual journal access points inside the vendored `revm` context:

- repeated account access rebate: `2500`
- repeated `SLOAD` rebate: `2000`
- repeated `SSTORE` rebate: `2100`

Storage accesses no longer also get the extra account rebate.

The per-tx savings are accumulated during execution in the patched vendored files:

- [rust/vendor/revm-context/src/journal/inner.rs](../../rust/vendor/revm-context/src/journal/inner.rs)
- [rust/vendor/revm-context-interface/src/journaled_state/account.rs](../../rust/vendor/revm-context-interface/src/journaled_state/account.rs)
- [rust/vendor/revm-context/src/block_warming.rs](../../rust/vendor/revm-context/src/block_warming.rs)

`alloy-op-evm` consumes that exact per-tx savings value in:

- [rust/alloy-op-evm/src/block/mod.rs](../../rust/alloy-op-evm/src/block/mod.rs)

## Why `revm-context` And `revm-context-interface` Were Vendored

The fix had to happen inside the `revm` journal and account-loading path, and those crates are external dependencies rather than normal workspace crates.

Without patching them locally, the executor could not:

- detect cold vs block-warm access at the actual `load_account` / `sload` / `sstore` decision points
- distinguish account access from storage access
- distinguish `SLOAD` from `SSTORE`
- carry the exact per-tx savings value back out to `alloy-op-evm`

So the exact versions already locked by the workspace were vendored and overridden through `[patch.crates-io]` in [rust/Cargo.toml](../../rust/Cargo.toml).

This was not a version downgrade. It was a local patch of the versions already pinned in the workspace lockfile.

## Changes In `revm-context-interface`

Main file:

- [rust/vendor/revm-context-interface/src/journaled_state/account.rs](../../rust/vendor/revm-context-interface/src/journaled_state/account.rs)

Changes:

- `JournaledAccount` now carries block-level warming state:
  - prior-block warm addresses
  - prior-block warm storage slots
  - current transaction savings accumulator
- `sload` / `sstore` now:
  - check whether a slot is warm because of an earlier tx in the same block
  - add `2000` for repeated `SLOAD`
  - add `2100` for repeated `SSTORE`
  - mark the account and slot as warm for later txs in the block

## Changes In `revm-context`

Main file:

- [rust/vendor/revm-context/src/journal/inner.rs](../../rust/vendor/revm-context/src/journal/inner.rs)

Changes:

- `JournalInner` now carries block-level warming state:
  - `block_warming_enabled`
  - `block_warm_addresses`
  - `block_warm_storage`
  - `tx_warming_savings`
  - `last_tx_warming_savings`
- account loading now has a helper that can apply the `2500` rebate only for real account-style accesses
- storage paths call that helper with account rebate disabled
- that is what prevents the old double-counting of account rebate plus slot rebate for storage access
- `commit_tx` preserves the exact savings value for the finished tx
- unit tests were added for:
  - repeated account access rebates `2500`
  - repeated `SLOAD` rebates `2000`
  - repeated `SSTORE` rebates `2100`

## Bridge Back To `alloy-op-evm`

Files:

- [rust/vendor/revm-context/src/block_warming.rs](../../rust/vendor/revm-context/src/block_warming.rs)
- [rust/vendor/revm-context/src/lib.rs](../../rust/vendor/revm-context/src/lib.rs)

These provide a small channel for exposing the most recent transaction’s warming savings so [rust/alloy-op-evm/src/block/mod.rs](../../rust/alloy-op-evm/src/block/mod.rs) can emit SDM refunds from the exact metered value rather than estimating from post-execution state.

## Effect

- refunds now match the intended SDM pricing model more closely
- storage-heavy real blocks should show lower, more realistic refunds
- `SLOAD` and `SSTORE` are no longer conflated
- the refund amount is based on actual cold-to-warm transitions during execution, not a post-hoc approximation
