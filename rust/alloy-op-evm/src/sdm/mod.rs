//! SDM (Sequencer-Defined Metering) execution extensions.

use alloc::vec::Vec;
use alloy_evm::Database;
use op_alloy::consensus::sdm::{SDMGasEntry, SDMPayload};
use revm::context_interface::journaled_state::persistent_warm_cache::WarmingRefundEvent;

use crate::{
    OpEvm,
    block::{OpBlockExecutor, receipt_builder::OpReceiptBuilder},
};

/// Extension trait for EVMs that expose SDM warming savings for the last executed transaction.
pub trait SdmEvmExt {
    /// Take the warming savings recorded for the most recently executed transaction.
    fn take_last_tx_warming_savings(&mut self) -> u64;

    /// Take the exact warming refund attribution events for the most recently executed transaction.
    fn take_last_tx_warming_events(&mut self) -> Vec<WarmingRefundEvent>;

    /// Set the replay-local block tx index used for persistent warming provenance metadata.
    fn set_persistent_warming_tx_index(&mut self, tx_index: u64);
}

impl<DB: Database, I, P, Tx> SdmEvmExt for OpEvm<DB, I, P, Tx> {
    fn take_last_tx_warming_savings(&mut self) -> u64 {
        Self::take_last_tx_warming_savings(self)
    }

    fn take_last_tx_warming_events(&mut self) -> Vec<WarmingRefundEvent> {
        Self::take_last_tx_warming_events(self)
    }

    fn set_persistent_warming_tx_index(&mut self, tx_index: u64) {
        Self::set_persistent_warming_tx_index(self, tx_index)
    }
}

/// Extension trait for block executors that collect SDM payload entries.
pub trait SdmExecutorExt {
    /// Enable SDM verifier mode with a pre-extracted payload.
    fn enable_sdm_verifier(&mut self, payload: SDMPayload);

    /// Take the accumulated SDM entries for the current block.
    fn take_sdm_entries(&mut self) -> Vec<SDMGasEntry>;

    /// Take the exact per-transaction warming refund attribution events aligned with receipts.
    fn take_warming_events_by_tx(&mut self) -> Vec<Vec<WarmingRefundEvent>>;
}

impl<E, R, Spec> SdmExecutorExt for OpBlockExecutor<E, R, Spec>
where
    E: alloy_evm::Evm,
    R: OpReceiptBuilder,
    Spec: alloy_op_hardforks::OpHardforks + Clone,
{
    fn enable_sdm_verifier(&mut self, payload: SDMPayload) {
        OpBlockExecutor::enable_sdm_verifier(self, payload)
    }

    fn take_sdm_entries(&mut self) -> Vec<SDMGasEntry> {
        OpBlockExecutor::take_sdm_entries(self)
    }

    fn take_warming_events_by_tx(&mut self) -> Vec<Vec<WarmingRefundEvent>> {
        OpBlockExecutor::take_warming_events_by_tx(self)
    }
}
