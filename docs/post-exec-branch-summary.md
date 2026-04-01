# Post-Exec Branch Summary

## Goal

This branch ports the SDM PoC to a **post-exec** implementation without vendoring upstream transaction/codec crates.

The key architectural choice is to use a **local wrapper transaction type** rather than adding a new variant directly to `OpTxEnvelope`.

## Core design

- Add a synthetic post-exec transaction type `0x7d`
- Keep upstream OP envelope types unchanged
- Extend OP transaction support locally through a wrapper / `#[envelope(flatten)]` pattern
- Preserve SDM PoC behavior while avoiding upstream vendoring

## What was implemented

### 1. Consensus and primitive support

- Added post-exec transaction and payload types
- Added helpers to build, detect, and decode post-exec payloads
- Propagated the wrapper transaction type through op-reth primitives

### 2. Execution-layer support

- Added post-exec warming inspector support
- Added executor plumbing for post-exec gas accounting
- Added receipt plumbing so `opGasRefund` is surfaced correctly

### 3. Payload builder and node wiring

- Added synthetic post-exec tx injection during block building
- Added builder config support
- Added node/CLI support via `--rollup.sdm-enabled`

### 4. Replay and RPC support

- Added a dedicated post-exec replay crate
- Exposed replay through op-reth debug RPC
- Wired replay output and receipt handling to current post-exec naming

### 5. Acceptance coverage

- Vendored the replay helper package used by the tests
- Restored the PoC acceptance tests
- Updated them to current RPC naming and current `post_exec_*` response fields
- Verified the focused post-exec acceptance coverage passes

## Why this architecture

The older PoC-style approach required widening upstream OP transaction enums, which in turn required patching vendored upstream crates for:

- tx/env conversion
- codecs / compact encoding
- memory-size accounting

This branch avoids that by keeping upstream types closed and extending behavior locally.

## Behavior vs. the PoC

Behaviorally, this branch aims to preserve the PoC’s important properties:

- post-exec tx inclusion in produced blocks
- canonical gas accounting via embedded refund payloads
- receipt-level `opGasRefund`
- replay-based validation of refund accounting

The main difference is architectural, not intended runtime behavior:

- **PoC:** modify upstream OP tx family directly
- **This branch:** wrap and extend locally

## In short

This branch:

- converts SDM into a **post-exec** implementation direction
- implements it using a **non-vendored wrapper architecture**
- adds replay and RPC support
- restores and updates end-to-end acceptance coverage
- documents the remaining downstream limitation succinctly
