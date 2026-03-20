//! Counterfactual SDM replay support for op-reth.

#![cfg_attr(not(test), warn(unused_crate_dependencies))]

mod jsonl;
mod replay;
mod types;

pub use jsonl::{SdmReplayJsonlRecord, write_jsonl_record};
pub use replay::{SdmReplayError, replay_block, strip_sdm_tx_for_replay};
pub use types::{
    ReplaySdmBlockOptions, ReplaySdmBlockRequest, SdmMode, SdmReplayBlock, SdmReplayConfig,
    SdmReplayMismatch, SdmReplayMismatchKind, SdmReplayPayload, SdmReplayPayloadEntry,
    SdmReplayRunConfig, SdmReplaySummary, SdmReplayTx,
};
