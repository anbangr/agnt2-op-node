# SDM Initial Implementation Summary

This note summarizes only the implementation changes in commit `5a94182d8f` relative to `develop`, focusing on:

0. EVM execution and payload building
1. Consensus and transaction plumbing
2. Receipt and RPC surface

The current branch points directly at `5a94182d8f`, so the branch diff and the commit diff are the same change set.

## 0. EVM Execution and Payload Building

Files: [rust/alloy-op-evm/src/block/mod.rs](../../rust/alloy-op-evm/src/block/mod.rs), [rust/alloy-op-evm/src/sdm/cache.rs](../../rust/alloy-op-evm/src/sdm/cache.rs), [rust/alloy-op-evm/src/sdm/mod.rs](../../rust/alloy-op-evm/src/sdm/mod.rs), [rust/alloy-op-evm/src/block/receipt_builder.rs](../../rust/alloy-op-evm/src/block/receipt_builder.rs), [rust/alloy-op-evm/src/lib.rs](../../rust/alloy-op-evm/src/lib.rs), [rust/op-reth/crates/evm/src/lib.rs](../../rust/op-reth/crates/evm/src/lib.rs), [rust/op-reth/crates/evm/src/receipts.rs](../../rust/op-reth/crates/evm/src/receipts.rs), [rust/op-reth/crates/payload/Cargo.toml](../../rust/op-reth/crates/payload/Cargo.toml), [rust/op-reth/crates/payload/src/traits.rs](../../rust/op-reth/crates/payload/src/traits.rs), [rust/op-reth/crates/payload/src/builder.rs](../../rust/op-reth/crates/payload/src/builder.rs)

- The block executor now carries SDM-specific execution state: a block-scoped warm cache, a vector of per-transaction refund entries, and an optional verifier payload.
- SDM is enabled during block pre-execution for the PoC, and the thread-local SDM channel is reset at the start of each block so the payload builder can collect refund entries later in the same block build.
- Synthetic SDM transactions are treated specially during execution: instead of entering the normal EVM path, they short-circuit to a successful zero-gas/zero-log result.
- After each non-deposit, non-SDM transaction commits, the executor computes block-level warming savings by checking whether accessed accounts and storage slots were already warmed by earlier transactions in the same block, then records the resulting refund as an `SDMGasEntry`.
- Deposit transactions do not earn refunds, but they still warm accounts and slots so later transactions can benefit.
- The payload builder consumes the accumulated SDM entries at the end of transaction selection, builds a synthetic `0x7d` transaction, seals it, wraps it as a recovered transaction with `Address::ZERO`, and appends it to the block.
- Receipt builders in both alloy-op-evm and op-reth were extended so `OpTxType::Sdm` produces an OP SDM receipt variant instead of falling through as an unsupported case.
- The new `PersistentWarmCache` is the primitive that makes block-level warming possible; it tracks warmed addresses and `(address, slot)` pairs across transactions and exposes helpers for warming and accounting.

### Key Snippet: Executor SDM State and Block Startup

```rust
pub struct OpBlockExecutor<Evm, R: OpReceiptBuilder, Spec> {
    pub warming_cache: Option<PersistentWarmCache>,
    pub sdm_entries: Vec<SDMGasEntry>,
    pub sdm_payload: Option<SDMPayload>,
}

pub fn enable_sdm(&mut self) {
    self.warming_cache = Some(PersistentWarmCache::new());
}

// SDM Block-Level Warming: always enable for PoC (no fork gating).
self.warming_cache = Some(PersistentWarmCache::new());

#[cfg(feature = "std")]
crate::sdm::channel::reset();
```

Source: [rust/alloy-op-evm/src/block/mod.rs](../../rust/alloy-op-evm/src/block/mod.rs)

### Key Snippet: Refund Computation During Commit

```rust
if let Some(cache) = &mut self.warming_cache {
    if !is_deposit && !is_sdm {
        let mut savings: u64 = 0;
        let tx_index = self.receipts.len() as u64;

        for (addr, account) in &state {
            if cache.is_address_warm(addr) {
                savings += 2500;
            }

            for key in account.storage.keys() {
                let key_b256 = B256::from(key.to_be_bytes::<32>());
                if cache.is_storage_warm(addr, &key_b256) {
                    savings += 2000;
                }
            }
        }

        for (addr, account) in &state {
            cache.warm_account(*addr);
            for key in account.storage.keys() {
                let key_b256 = B256::from(key.to_be_bytes::<32>());
                cache.warm_storage(*addr, key_b256);
            }
        }

        if savings > 0 {
            let entry = SDMGasEntry { index: tx_index, gas_refund: savings };
            self.sdm_entries.push(entry.clone());
            #[cfg(feature = "std")]
            crate::sdm::channel::append_sdm_entry(entry);
        }
    }
}
```

Source: [rust/alloy-op-evm/src/block/mod.rs](../../rust/alloy-op-evm/src/block/mod.rs)

### Key Snippet: Synthetic SDM Transaction Injection

```rust
if reth_optimism_evm::sdm::channel::is_active() {
    use alloy_consensus::Sealable;
    let entries = reth_optimism_evm::sdm::channel::take_sdm_entries();
    let sdm_tx = op_alloy_consensus::sdm::build_sdm_tx(entries);
    let sdm_sealed = sdm_tx.seal_slow();
    let sdm_signed: N::SignedTx = sdm_sealed.into();
    let sdm_recovered =
        alloy_consensus::transaction::Recovered::new_unchecked(sdm_signed, Address::ZERO);

    match builder.execute_transaction(sdm_recovered) {
        Ok(_gas_used) => {
            debug!(target: "payload_builder", "SDM tx included in block");
        }
        Err(err) => {
            warn!(target: "payload_builder", %err, "SDM tx execution failed, skipping");
        }
    }
}
```

Source: [rust/op-reth/crates/payload/src/builder.rs](../../rust/op-reth/crates/payload/src/builder.rs)

## 1. Consensus and Transaction Plumbing

Files: [rust/op-alloy/crates/consensus/src/sdm.rs](../../rust/op-alloy/crates/consensus/src/sdm.rs), [rust/op-alloy/crates/consensus/src/lib.rs](../../rust/op-alloy/crates/consensus/src/lib.rs), [rust/op-alloy/crates/consensus/src/alloy_compat.rs](../../rust/op-alloy/crates/consensus/src/alloy_compat.rs), [rust/op-alloy/crates/consensus/src/transaction/tx_type.rs](../../rust/op-alloy/crates/consensus/src/transaction/tx_type.rs), [rust/op-alloy/crates/consensus/src/transaction/typed.rs](../../rust/op-alloy/crates/consensus/src/transaction/typed.rs), [rust/op-alloy/crates/consensus/src/transaction/envelope.rs](../../rust/op-alloy/crates/consensus/src/transaction/envelope.rs), [rust/op-alloy/crates/network/src/lib.rs](../../rust/op-alloy/crates/network/src/lib.rs), [rust/op-alloy/crates/rpc-types/src/transaction/request.rs](../../rust/op-alloy/crates/rpc-types/src/transaction/request.rs), [rust/op-reth/crates/primitives/src/transaction/signed.rs](../../rust/op-reth/crates/primitives/src/transaction/signed.rs), [rust/op-reth/crates/cli/src/ovm_file_codec.rs](../../rust/op-reth/crates/cli/src/ovm_file_codec.rs)

- The branch introduces a brand new OP transaction type for SDM: `0x7d`, represented as a synthetic OP transaction carrying SDM metadata.
- `SDMPayload` is the canonical data format carried by the synthetic transaction; it stores a `version` plus a list of `(tx_index, gas_refund)` entries encoded in RLP.
- `TxSdm` is implemented as a synthetic, non-user transaction: zero gas, zero value, no chain id, no signature semantics, and a transaction hash derived directly from its EIP-2718 encoding.
- The consensus layer exposes helpers to build an SDM transaction from collected refund entries and to extract the payload back out of a decoded SDM transaction.
- `OpTxType`, `OpTypedTransaction`, and `OpTxEnvelope` were all extended with an `Sdm` variant so the new type is recognized everywhere normal OP transaction typing is used.
- Conversion logic was updated so SDM can round-trip through unknown typed transaction decoding, RPC transaction requests, signed transaction wrappers, and file codecs.
- The plumbing is intentionally asymmetric with normal user transactions: SDM cannot be pooled, cannot be converted to a standard Ethereum transaction, and wallet signing explicitly errors for it.
- Signed-transaction support in op-reth treats SDM the same way deposits are treated for signature purposes: a synthetic placeholder signature is used, signer recovery returns `Address::ZERO`, and the hash is taken from the transaction body rather than an ECDSA signing flow.

### Key Snippet: SDM Payload and Transaction Definition

```rust
pub const SDM_TX_TYPE_ID: u8 = 0x7D;
pub const SDM_TX_POSITION: usize = 1;

pub struct SDMGasEntry {
    pub index: u64,
    pub gas_refund: u64,
}

pub struct SDMPayload {
    pub version: u64,
    pub entries: Vec<SDMGasEntry>,
}

pub struct TxSdm {
    pub payload: SDMPayload,
    input: Bytes,
}

impl Typed2718 for TxSdm {
    fn ty(&self) -> u8 {
        SDM_TX_TYPE_ID
    }
}
```

Source: [rust/op-alloy/crates/consensus/src/sdm.rs](../../rust/op-alloy/crates/consensus/src/sdm.rs)

### Key Snippet: Build and Extract Helpers

```rust
pub fn build_sdm_tx(entries: Vec<SDMGasEntry>) -> TxSdm {
    TxSdm::new(SDMPayload { version: 1, entries })
}

pub fn is_sdm_tx(ty: u8) -> bool {
    ty == SDM_TX_TYPE_ID
}

pub fn extract_sdm_payload_from_tx(tx: &TxSdm) -> Option<SDMPayload> {
    Some(tx.payload.clone())
}
```

Source: [rust/op-alloy/crates/consensus/src/sdm.rs](../../rust/op-alloy/crates/consensus/src/sdm.rs)

### Key Snippet: SDM Added to the OP Envelope

```rust
pub enum OpTxEnvelope {
    Legacy(Signed<TxLegacy>),
    Eip2930(Signed<TxEip2930>),
    Eip1559(Signed<TxEip1559>),
    Eip7702(Signed<TxEip7702>),
    Sdm(Sealed<TxSdm>),
    Deposit(Sealed<TxDeposit>),
}
```

Source: [rust/op-alloy/crates/consensus/src/transaction/envelope.rs](../../rust/op-alloy/crates/consensus/src/transaction/envelope.rs)

## 2. Receipt and RPC Surface

Files: [rust/op-alloy/crates/consensus/src/receipts/envelope.rs](../../rust/op-alloy/crates/consensus/src/receipts/envelope.rs), [rust/op-alloy/crates/consensus/src/receipts/receipt.rs](../../rust/op-alloy/crates/consensus/src/receipts/receipt.rs), [rust/op-alloy/crates/rpc-types/src/receipt.rs](../../rust/op-alloy/crates/rpc-types/src/receipt.rs), [rust/op-alloy/crates/rpc-types/src/transaction/request.rs](../../rust/op-alloy/crates/rpc-types/src/transaction/request.rs), [rust/op-reth/crates/rpc/src/eth/receipt.rs](../../rust/op-reth/crates/rpc/src/eth/receipt.rs), [rust/op-reth/crates/primitives/src/receipt.rs](../../rust/op-reth/crates/primitives/src/receipt.rs), [rust/op-reth/crates/cli/src/receipt_file_codec.rs](../../rust/op-reth/crates/cli/src/receipt_file_codec.rs)

- Receipt typing was extended all the way down to consensus encoding: both `OpReceiptEnvelope` and `OpReceipt` now have an explicit `Sdm` variant, so SDM receipts serialize and deserialize as first-class OP receipts instead of piggybacking on another type.
- The RPC receipt model gained a new `opGasRefund` field, which is the user-visible output of the SDM block-level warming logic.
- The same RPC model also surfaces `depositNonce` and `depositReceiptVersion` at the top level, which is part of the same receipt-shape refactor done alongside SDM.
- The op-reth RPC receipt converter now scans each block for a `0x7d` transaction, decodes its `SDMPayload`, and maps refund entries back onto receipts by transaction index.
- `OpReceiptBuilder` was updated to accept an optional `op_gas_refund` input, carry it through the OP receipt fields builder, and emit it in the final `OpTransactionReceipt`.
- Codec and primitive layers were updated so SDM receipts remain valid through internal serialization, file import/export, and bincode-compatible formats.

### Key Snippet: SDM Receipt Variant

```rust
pub enum OpReceiptEnvelope<T = Log> {
    Legacy(ReceiptWithBloom<Receipt<T>>),
    Eip2930(ReceiptWithBloom<Receipt<T>>),
    Eip1559(ReceiptWithBloom<Receipt<T>>),
    Eip7702(ReceiptWithBloom<Receipt<T>>),
    Sdm(ReceiptWithBloom<Receipt<T>>),
    Deposit(ReceiptWithBloom<OpDepositReceipt<T>>),
}

match tx_type {
    OpTxType::Sdm => Self::Sdm(ReceiptWithBloom { receipt: inner_receipt, logs_bloom }),
    OpTxType::Deposit => { /* ... */ }
    _ => { /* existing receipt types */ }
}
```

Source: [rust/op-alloy/crates/consensus/src/receipts/envelope.rs](../../rust/op-alloy/crates/consensus/src/receipts/envelope.rs)

### Key Snippet: RPC Receipt Shape

```rust
pub struct OpTransactionReceipt {
    #[serde(flatten)]
    pub inner: alloy_rpc_types_eth::TransactionReceipt<ReceiptWithBloom<OpReceipt<Log>>>,
    #[serde(flatten)]
    pub l1_block_info: L1BlockInfo,
    #[serde(default, skip_serializing_if = "Option::is_none", with = "alloy_serde::quantity::opt")]
    pub op_gas_refund: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none", with = "alloy_serde::quantity::opt")]
    pub deposit_nonce: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none", with = "alloy_serde::quantity::opt")]
    pub deposit_receipt_version: Option<u64>,
}
```

Source: [rust/op-alloy/crates/rpc-types/src/receipt.rs](../../rust/op-alloy/crates/rpc-types/src/receipt.rs)

### Key Snippet: Mapping the SDM Payload Back onto RPC Receipts

```rust
let sdm_payload: Option<SDMPayload> = block.body().transactions().iter().find_map(|tx| {
    if tx.ty() != SDM_TX_TYPE_ID {
        return None;
    }

    let encoded = tx.encoded_2718();
    let mut encoded_slice = encoded.as_ref();
    let sdm_tx = TxSdm::decode_2718(&mut encoded_slice).ok()?;
    extract_sdm_payload_from_tx(&sdm_tx)
});

for input in inputs {
    l1_block_info.clear_tx_l1_cost();
    let op_gas_refund = sdm_payload
        .as_ref()
        .and_then(|payload: &SDMPayload| payload.gas_refund_for_idx(input.meta.index));

    receipts.push(
        OpReceiptBuilder::new(
            &self.provider.chain_spec(),
            input,
            &mut l1_block_info,
            op_gas_refund,
        )?
        .build(),
    );
}
```

Source: [rust/op-reth/crates/rpc/src/eth/receipt.rs](../../rust/op-reth/crates/rpc/src/eth/receipt.rs)
