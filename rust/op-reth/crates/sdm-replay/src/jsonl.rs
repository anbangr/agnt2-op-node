use crate::types::{
    SdmReplayBlock, SdmReplayMismatch, SdmReplayRunConfig, SdmReplaySummary, SdmReplayTx,
};
use serde::Serialize;
use std::io::{self, Write};

/// JSONL record for replay output.
#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum SdmReplayJsonlRecord<'a> {
    /// Run-level config.
    RunConfig(&'a SdmReplayRunConfig),
    /// Per-tx row.
    Tx(&'a SdmReplayTx),
    /// Per-block row.
    Block(&'a SdmReplayBlock),
    /// Mismatch row.
    Mismatch(&'a SdmReplayMismatch),
    /// Summary row.
    Summary(&'a SdmReplaySummary),
}

/// Write one JSONL record.
pub fn write_jsonl_record(
    mut writer: impl Write,
    record: &SdmReplayJsonlRecord<'_>,
) -> io::Result<()> {
    serde_json::to_writer(&mut writer, record)?;
    writer.write_all(b"\n")
}
