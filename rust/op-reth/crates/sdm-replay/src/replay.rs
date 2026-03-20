use crate::types::{
    SdmMode, SdmReplayBlock, SdmReplayConfig, SdmReplayMismatch, SdmReplayMismatchKind,
    SdmReplayPayload, SdmReplayPayloadEntry, SdmReplaySummary, SdmReplayTx,
};
use alloy_consensus::{Block as AlloyBlock, BlockBody, BlockHeader, TxReceipt, Typed2718};
use op_alloy_consensus::sdm::{SDM_TX_TYPE_ID, SDMGasEntry, SDMPayload};
use reth_evm::{Database, execute::BlockExecutor};
use reth_execution_errors::BlockExecutionError;
use reth_optimism_evm::{ConfigureSdmEvm, sdm::SdmExecutorExt};
use reth_optimism_primitives::{OpBlock, OpPrimitives};
use reth_primitives_traits::{Block, RecoveredBlock};
use revm::database::{State, states::bundle_state::BundleRetention};
use std::collections::{BTreeMap, BTreeSet};

/// Replay error.
#[derive(Debug, thiserror::Error)]
pub enum SdmReplayError {
    /// Unsupported replay configuration.
    #[error("unsupported replay mode: {0:?}")]
    UnsupportedMode(SdmMode),
    /// Execution failed.
    #[error(transparent)]
    Execution(#[from] BlockExecutionError),
}

#[derive(Debug, Clone)]
struct NormalizedBlock {
    replay_block: RecoveredBlock<OpBlock>,
    original_indexes: Vec<u64>,
    embedded_payload: Option<SDMPayload>,
    sdm_tx_index: Option<u64>,
}

/// Strip the synthetic SDM tx from a block before replay while preserving original indexes.
pub fn strip_sdm_tx_for_replay(
    block: &RecoveredBlock<OpBlock>,
) -> (RecoveredBlock<OpBlock>, Vec<u64>) {
    let normalized = normalize_block(block);
    (normalized.replay_block, normalized.original_indexes)
}

fn normalize_block(block: &RecoveredBlock<OpBlock>) -> NormalizedBlock {
    let (raw_block, senders) = block.clone().split();
    let (header, body) = raw_block.split();
    let BlockBody { transactions, ommers, withdrawals } = body;

    let mut replay_transactions = Vec::with_capacity(transactions.len());
    let mut replay_senders = Vec::with_capacity(senders.len());
    let mut original_indexes = Vec::with_capacity(transactions.len());
    let mut embedded_payload = None;
    let mut sdm_tx_index = None;

    for (idx, (tx, sender)) in transactions.into_iter().zip(senders.into_iter()).enumerate() {
        if tx.ty() == SDM_TX_TYPE_ID {
            sdm_tx_index = Some(idx as u64);
            embedded_payload = tx.as_sdm().map(|sdm| sdm.payload.clone());
            continue;
        }

        original_indexes.push(idx as u64);
        replay_transactions.push(tx);
        replay_senders.push(sender);
    }

    let replay_block = RecoveredBlock::new_unhashed(
        AlloyBlock::new(
            header,
            BlockBody { transactions: replay_transactions, ommers, withdrawals },
        ),
        replay_senders,
    );

    NormalizedBlock { replay_block, original_indexes, embedded_payload, sdm_tx_index }
}

fn build_payload_map(
    block_number: u64,
    block: &RecoveredBlock<OpBlock>,
    payload: &SDMPayload,
    mismatches: &mut Vec<SdmReplayMismatch>,
) -> BTreeMap<u64, u64> {
    let mut refunds = BTreeMap::new();
    let mut seen = BTreeSet::new();
    let tx_count = block.body().transactions.len() as u64;

    for entry in &payload.entries {
        if !seen.insert(entry.index) {
            mismatches.push(SdmReplayMismatch {
                category: SdmReplayMismatchKind::DuplicatePayloadIndex,
                block_num: block_number,
                tx_index: Some(entry.index),
                expected: None,
                actual: Some(entry.gas_refund),
                message: format!("duplicate payload entry for tx index {}", entry.index),
            });
            continue;
        }

        if entry.index >= tx_count {
            mismatches.push(SdmReplayMismatch {
                category: SdmReplayMismatchKind::PayloadIndexOutOfRange,
                block_num: block_number,
                tx_index: Some(entry.index),
                expected: Some(tx_count.saturating_sub(1)),
                actual: Some(entry.index),
                message: format!("payload entry targets out-of-range tx index {}", entry.index),
            });
            continue;
        }

        let tx = &block.body().transactions[entry.index as usize];
        if tx.is_deposit() {
            mismatches.push(SdmReplayMismatch {
                category: SdmReplayMismatchKind::PayloadTargetsDeposit,
                block_num: block_number,
                tx_index: Some(entry.index),
                expected: Some(0),
                actual: Some(entry.gas_refund),
                message: format!("payload entry targets deposit tx index {}", entry.index),
            });
            continue;
        }

        if tx.ty() == SDM_TX_TYPE_ID {
            mismatches.push(SdmReplayMismatch {
                category: SdmReplayMismatchKind::PayloadTargetsSdm,
                block_num: block_number,
                tx_index: Some(entry.index),
                expected: Some(0),
                actual: Some(entry.gas_refund),
                message: format!("payload entry targets SDM tx index {}", entry.index),
            });
            continue;
        }

        refunds.insert(entry.index, entry.gas_refund);
    }

    refunds
}

fn into_replay_payload(payload: SDMPayload) -> SdmReplayPayload {
    SdmReplayPayload {
        version: payload.version,
        entries: payload
            .entries
            .into_iter()
            .map(|entry| SdmReplayPayloadEntry { index: entry.index, gas_refund: entry.gas_refund })
            .collect(),
    }
}

/// Replay a historical block with SDM enabled counterfactually.
pub fn replay_block<DB, EvmConfig>(
    evm_config: &EvmConfig,
    db: DB,
    block: &RecoveredBlock<OpBlock>,
    config: SdmReplayConfig,
) -> Result<SdmReplayBlock, SdmReplayError>
where
    DB: Database,
    EvmConfig: ConfigureSdmEvm<Primitives = OpPrimitives>,
{
    if config.mode != SdmMode::CounterfactualEnabled {
        return Err(SdmReplayError::UnsupportedMode(config.mode));
    }

    let normalized = normalize_block(block);

    let mut state =
        State::builder().with_database(db).with_bundle_update().without_state_clear().build();
    let mut executor = evm_config
        .sdm_executor_for_block(&mut state, normalized.replay_block.sealed_block())
        .map_err(BlockExecutionError::other)?;

    executor.apply_pre_execution_changes()?;
    for tx in normalized.replay_block.transactions_recovered() {
        executor.execute_transaction(tx)?;
    }
    let replay_entries: Vec<SDMGasEntry> = executor.take_sdm_entries();
    let execution = executor.apply_post_execution_changes()?;

    state.merge_transitions(BundleRetention::Reverts);

    let replay_payload = SDMPayload { version: 1, entries: replay_entries.clone() };
    let replay_refunds: BTreeMap<u64, u64> =
        replay_entries.iter().map(|entry| (entry.index, entry.gas_refund)).collect();

    let mut mismatches = Vec::new();
    let payload_refunds = normalized
        .embedded_payload
        .as_ref()
        .map(|payload| build_payload_map(block.header().number(), block, payload, &mut mismatches))
        .unwrap_or_default();

    let receipt_refunds = payload_refunds.clone();
    let mut txs = Vec::with_capacity(normalized.replay_block.body().transactions.len());
    let mut previous_cumulative_gas = 0_u64;

    for (replay_idx, tx) in normalized.replay_block.body().transactions.iter().enumerate() {
        let tx_index = normalized.original_indexes[replay_idx];
        let cumulative_gas_used = execution.receipts[replay_idx].cumulative_gas_used();
        let gas_used = cumulative_gas_used.saturating_sub(previous_cumulative_gas);
        previous_cumulative_gas = cumulative_gas_used;

        let replay_refund = replay_refunds.get(&tx_index).copied().unwrap_or_default();
        let payload_refund = payload_refunds.get(&tx_index).copied();
        let receipt_refund = receipt_refunds.get(&tx_index).copied();
        let mut mismatch = false;

        if config.compare_payload && payload_refund.unwrap_or_default() != replay_refund {
            mismatch = true;
            mismatches.push(SdmReplayMismatch {
                category: SdmReplayMismatchKind::PayloadRefundMismatch,
                block_num: block.header().number(),
                tx_index: Some(tx_index),
                expected: payload_refund,
                actual: Some(replay_refund),
                message: format!("payload refund mismatch for tx index {}", tx_index),
            });
        }

        if config.compare_receipts && receipt_refund.unwrap_or_default() != replay_refund {
            mismatch = true;
            mismatches.push(SdmReplayMismatch {
                category: SdmReplayMismatchKind::ReceiptRefundMismatch,
                block_num: block.header().number(),
                tx_index: Some(tx_index),
                expected: receipt_refund,
                actual: Some(replay_refund),
                message: format!("receipt refund mismatch for tx index {}", tx_index),
            });
        }

        txs.push(SdmReplayTx {
            tx_index,
            replay_tx_index: replay_idx as u64,
            tx_hash: tx.tx_hash(),
            tx_type: tx.ty(),
            is_deposit_tx: tx.is_deposit(),
            gas_used,
            op_gas_refund_replay: replay_refund,
            op_gas_refund_payload: payload_refund,
            op_gas_refund_receipt: receipt_refund,
            effective_gas: gas_used.saturating_sub(replay_refund),
            mismatch,
        });
    }

    let tx_count_user = txs.iter().filter(|tx| !tx.is_deposit_tx).count();
    let replay_refund_total = txs.iter().map(|tx| tx.op_gas_refund_replay).sum::<u64>();
    let payload_refund_total =
        txs.iter().map(|tx| tx.op_gas_refund_payload.unwrap_or_default()).sum::<u64>();
    let receipt_refund_total =
        txs.iter().map(|tx| tx.op_gas_refund_receipt.unwrap_or_default()).sum::<u64>();
    let block_gas_used = txs.iter().map(|tx| tx.gas_used).sum::<u64>();

    let summary = SdmReplaySummary {
        block_num: block.header().number(),
        block_hash: block.hash(),
        tx_count_total: txs.len(),
        tx_count_user,
        sdm_tx_present: normalized.sdm_tx_index.is_some(),
        sdm_payload_entry_count: replay_entries.len(),
        block_gas_used,
        replay_refund_total,
        payload_refund_total,
        node_receipt_refund_total: receipt_refund_total,
        block_effective_gas: block_gas_used.saturating_sub(replay_refund_total),
        mismatch_count: mismatches.len(),
        replay_mode: config.mode,
    };

    Ok(SdmReplayBlock {
        config,
        block_num: block.header().number(),
        block_hash: block.hash(),
        parent_hash: block.header().parent_hash().clone(),
        sdm_tx_present: normalized.sdm_tx_index.is_some(),
        sdm_tx_index: normalized.sdm_tx_index,
        embedded_payload: normalized.embedded_payload.map(into_replay_payload),
        synthesized_payload_bytes: replay_payload.to_rlp_bytes(),
        synthesized_payload: into_replay_payload(replay_payload),
        txs,
        mismatches,
        summary,
    })
}

#[cfg(test)]
mod tests {
    use super::strip_sdm_tx_for_replay;
    use alloy_consensus::{BlockBody, Header, Sealable};
    use alloy_primitives::Address;
    use op_alloy_consensus::{TxDeposit, sdm::build_sdm_tx};
    use reth_optimism_primitives::OpTransactionSigned;
    use reth_primitives_traits::RecoveredBlock;

    #[test]
    fn strips_sdm_tx_and_preserves_original_indexes() {
        let deposit: OpTransactionSigned = TxDeposit::default().into();
        let user = deposit.clone();
        let sdm: OpTransactionSigned = build_sdm_tx(vec![]).seal_slow().into();

        let block = RecoveredBlock::new_unhashed(
            alloy_consensus::Block::new(
                Header::default(),
                BlockBody {
                    transactions: vec![deposit, user, sdm],
                    ommers: vec![],
                    withdrawals: None,
                },
            ),
            vec![Address::ZERO, Address::ZERO, Address::ZERO],
        );

        let (replay_block, original_indexes) = strip_sdm_tx_for_replay(&block);
        assert_eq!(replay_block.body().transactions.len(), 2);
        assert_eq!(original_indexes, vec![0, 1]);
    }
}
