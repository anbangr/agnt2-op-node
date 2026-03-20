use alloy_eips::BlockNumberOrTag;
use alloy_primitives::{B256, Bytes};
use serde::{Deserialize, Serialize};

/// Single-block replay request, accepting either a block tag/number or a block hash.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(untagged)]
pub enum ReplaySdmBlockRequest {
    /// A block number or tag like `latest`.
    Number(BlockNumberOrTag),
    /// A block hash.
    Hash(B256),
}

/// Options for `debug_replaySdmBlock`.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReplaySdmBlockOptions {
    /// Compare replay refunds against any embedded SDM payload in the source block.
    #[serde(default)]
    pub compare_payload: bool,
    /// Compare replay refunds against the receipt-level `opGasRefund` projection.
    #[serde(default)]
    pub compare_receipts: bool,
}

impl Default for ReplaySdmBlockOptions {
    fn default() -> Self {
        Self { compare_payload: true, compare_receipts: true }
    }
}

/// SDM replay mode.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case")]
pub enum SdmMode {
    /// Run block execution without SDM.
    Disabled,
    /// Re-execute a historical block as if SDM had been enabled.
    #[default]
    CounterfactualEnabled,
    /// Re-execute while also validating against an already-existing SDM payload.
    Verifier,
}

/// Replay configuration.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplayConfig {
    /// Replay mode.
    pub mode: SdmMode,
    /// Compare replay refunds against an embedded payload when present.
    pub compare_payload: bool,
    /// Compare replay refunds against receipt-level `opGasRefund` projection when present.
    pub compare_receipts: bool,
}

impl Default for SdmReplayConfig {
    fn default() -> Self {
        Self { mode: SdmMode::CounterfactualEnabled, compare_payload: true, compare_receipts: true }
    }
}

/// Per-transaction replay row.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplayTx {
    /// Original transaction index in the source block.
    pub tx_index: u64,
    /// Replay-local transaction index after stripping any SDM tx.
    pub replay_tx_index: u64,
    /// Transaction hash.
    pub tx_hash: B256,
    /// Raw transaction type byte.
    pub tx_type: u8,
    /// Whether this is a deposit transaction.
    pub is_deposit_tx: bool,
    /// Gas used by the transaction during replay.
    pub gas_used: u64,
    /// Counterfactual SDM refund from replay.
    pub op_gas_refund_replay: u64,
    /// Refund projected from the source block's SDM payload.
    pub op_gas_refund_payload: Option<u64>,
    /// Refund projected into receipts (`opGasRefund`).
    pub op_gas_refund_receipt: Option<u64>,
    /// Gas used minus the replay refund.
    pub effective_gas: u64,
    /// Whether any mismatch affected this row.
    pub mismatch: bool,
}

/// Replay mismatch category.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SdmReplayMismatchKind {
    /// The source block contained duplicate payload indexes.
    DuplicatePayloadIndex,
    /// The source block payload referenced an out-of-range index.
    PayloadIndexOutOfRange,
    /// The source payload referenced a deposit tx.
    PayloadTargetsDeposit,
    /// The source payload referenced the SDM tx itself.
    PayloadTargetsSdm,
    /// Replay refund disagreed with the embedded payload.
    PayloadRefundMismatch,
    /// Replay refund disagreed with the projected receipt refund.
    ReceiptRefundMismatch,
    /// Replay mode is not implemented yet.
    UnsupportedMode,
}

/// Replay mismatch row.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplayMismatch {
    /// Category.
    pub category: SdmReplayMismatchKind,
    /// Block number.
    pub block_num: u64,
    /// Transaction index, if applicable.
    pub tx_index: Option<u64>,
    /// Expected value.
    pub expected: Option<u64>,
    /// Actual value.
    pub actual: Option<u64>,
    /// Human-readable explanation.
    pub message: String,
}

/// Block-level summary.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplaySummary {
    /// Block number.
    pub block_num: u64,
    /// Block hash.
    pub block_hash: B256,
    /// Number of transactions replayed after stripping the SDM tx.
    pub tx_count_total: usize,
    /// Number of non-deposit transactions replayed.
    pub tx_count_user: usize,
    /// Whether the original block contained an SDM tx.
    pub sdm_tx_present: bool,
    /// Number of non-zero replay refund entries.
    pub sdm_payload_entry_count: usize,
    /// Total block gas used across replayed transactions.
    pub block_gas_used: u64,
    /// Total counterfactual replay refund.
    pub replay_refund_total: u64,
    /// Total embedded-payload refund.
    pub payload_refund_total: u64,
    /// Total projected receipt refund.
    pub node_receipt_refund_total: u64,
    /// Effective gas after replay refunds.
    pub block_effective_gas: u64,
    /// Total mismatches.
    pub mismatch_count: usize,
    /// Replay mode.
    pub replay_mode: SdmMode,
}

/// Single-block replay response.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplayBlock {
    /// Replay config used for the block.
    pub config: SdmReplayConfig,
    /// Block number.
    pub block_num: u64,
    /// Block hash.
    pub block_hash: B256,
    /// Parent hash.
    pub parent_hash: B256,
    /// Whether the original block contained an SDM tx.
    pub sdm_tx_present: bool,
    /// Original SDM tx index, if present.
    pub sdm_tx_index: Option<u64>,
    /// Embedded payload from the original block, if present.
    pub embedded_payload: Option<SdmReplayPayload>,
    /// Replay-synthesized payload.
    pub synthesized_payload: SdmReplayPayload,
    /// RLP bytes for the replay-synthesized payload.
    pub synthesized_payload_bytes: Bytes,
    /// Per-transaction replay rows.
    pub txs: Vec<SdmReplayTx>,
    /// Mismatch rows.
    pub mismatches: Vec<SdmReplayMismatch>,
    /// Block summary.
    pub summary: SdmReplaySummary,
}

/// Run-level configuration for JSONL output.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplayRunConfig {
    /// Record discriminator.
    #[serde(rename = "type")]
    pub record_type: &'static str,
    /// First replayed block.
    pub from_block: u64,
    /// Last replayed block.
    pub to_block: u64,
    /// Replay mode.
    pub replay_mode: SdmMode,
    /// Whether payload comparisons were enabled.
    pub compare_payload: bool,
    /// Whether receipt comparisons were enabled.
    pub compare_receipts: bool,
}
/// Serializable replay payload entry.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplayPayloadEntry {
    /// Original transaction index.
    pub index: u64,
    /// Gas refund.
    pub gas_refund: u64,
}

/// Serializable replay payload.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SdmReplayPayload {
    /// Payload format version.
    pub version: u64,
    /// Payload entries.
    pub entries: Vec<SdmReplayPayloadEntry>,
}
