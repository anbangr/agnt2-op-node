//! SDM (Subjective/Sequencer-Defined Metering) types for Block-Level Warming.
//!
//! This module defines the SDM payload types and helpers for building/extracting
//! SDM data from blocks. The SDM transaction is a dedicated synthetic
//! transaction (type `0x7D`) that carries per-transaction warming refund
//! metadata computed by the sequencer.

use alloc::vec::Vec;
use alloy_consensus::{Sealable, Transaction, Typed2718};
use alloy_eips::{
    eip2718::{Decodable2718, Eip2718Error, Eip2718Result, Encodable2718, IsTyped2718},
    eip2930::AccessList,
};
use alloy_primitives::{B256, Bytes, ChainId, TxHash, TxKind, U256, keccak256};
use alloy_rlp::{BufMut, Decodable, Encodable, Header};
use core::mem;

/// Type byte for the synthetic SDM transaction.
pub const SDM_TX_TYPE_ID: u8 = 0x7D;

/// Position constant used by SDM-related block assembly logic.
pub const SDM_TX_POSITION: usize = 1;

/// Per-transaction gas refund entry within an [`SDMPayload`].
#[derive(Debug, Clone, PartialEq, Eq, Hash, Default)]
#[cfg_attr(feature = "arbitrary", derive(arbitrary::Arbitrary))]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
#[cfg_attr(feature = "serde", serde(rename_all = "camelCase"))]
pub struct SDMGasEntry {
    /// Transaction index within the block (final position, 0-based).
    pub index: u64,
    /// Gas refund from block-level warming savings.
    pub gas_refund: u64,
}

impl Encodable for SDMGasEntry {
    fn encode(&self, out: &mut dyn BufMut) {
        let list_len = self.index.length() + self.gas_refund.length();
        Header { list: true, payload_length: list_len }.encode(out);
        self.index.encode(out);
        self.gas_refund.encode(out);
    }

    fn length(&self) -> usize {
        let list_len = self.index.length() + self.gas_refund.length();
        Header { list: true, payload_length: list_len }.length() + list_len
    }
}

impl Decodable for SDMGasEntry {
    fn decode(buf: &mut &[u8]) -> alloy_rlp::Result<Self> {
        let header = Header::decode(buf)?;
        if !header.list {
            return Err(alloy_rlp::Error::UnexpectedString);
        }
        let remaining = buf.len();
        let this = Self { index: Decodable::decode(buf)?, gas_refund: Decodable::decode(buf)? };
        if buf.len() + header.payload_length != remaining {
            return Err(alloy_rlp::Error::UnexpectedLength);
        }
        Ok(this)
    }
}

/// SDM (Sequencer-Defined Metering) payload.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Default)]
#[cfg_attr(feature = "arbitrary", derive(arbitrary::Arbitrary))]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
#[cfg_attr(feature = "serde", serde(rename_all = "camelCase"))]
pub struct SDMPayload {
    /// Format version (1 = block-level warming).
    pub version: u64,
    /// Per-transaction refund entries.
    pub entries: Vec<SDMGasEntry>,
}

impl SDMPayload {
    /// Look up refund for a given tx index.
    pub fn gas_refund_for_idx(&self, index: u64) -> Option<u64> {
        self.entries.iter().find(|e| e.index == index).map(|e| e.gas_refund)
    }

    /// RLP-encode the payload into bytes.
    pub fn to_rlp_bytes(&self) -> Bytes {
        let mut buf = Vec::new();
        self.encode(&mut buf);
        buf.into()
    }

    /// Decode an SDM payload from RLP bytes.
    pub fn from_rlp_bytes(data: &[u8]) -> Option<Self> {
        Self::decode(&mut &data[..]).ok()
    }
}

impl Encodable for SDMPayload {
    fn encode(&self, out: &mut dyn BufMut) {
        let list_len = self.version.length() + self.entries.length();
        Header { list: true, payload_length: list_len }.encode(out);
        self.version.encode(out);
        self.entries.encode(out);
    }

    fn length(&self) -> usize {
        let list_len = self.version.length() + self.entries.length();
        Header { list: true, payload_length: list_len }.length() + list_len
    }
}

impl Decodable for SDMPayload {
    fn decode(buf: &mut &[u8]) -> alloy_rlp::Result<Self> {
        let header = Header::decode(buf)?;
        if !header.list {
            return Err(alloy_rlp::Error::UnexpectedString);
        }
        let remaining = buf.len();
        let this = Self { version: Decodable::decode(buf)?, entries: Decodable::decode(buf)? };
        if buf.len() + header.payload_length != remaining {
            return Err(alloy_rlp::Error::UnexpectedLength);
        }
        Ok(this)
    }
}

/// Synthetic SDM transaction carrying an [`SDMPayload`].
#[derive(Debug, Clone, PartialEq, Eq, Hash, Default)]
#[cfg_attr(feature = "arbitrary", derive(arbitrary::Arbitrary))]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
#[cfg_attr(feature = "serde", serde(into = "TxSdmSerdeHelper", try_from = "TxSdmSerdeHelper"))]
pub struct TxSdm {
    /// RLP payload for the synthetic transaction.
    pub payload: SDMPayload,
    input: Bytes,
}

impl TxSdm {
    /// Construct an SDM transaction from its decoded payload.
    pub fn new(payload: SDMPayload) -> Self {
        let input = payload.to_rlp_bytes();
        Self { payload, input }
    }

    /// Encoded length of the transaction body.
    pub fn rlp_encoded_length(&self) -> usize {
        self.input.len()
    }

    /// Encoded length including the type byte.
    pub fn eip2718_encoded_length(&self) -> usize {
        self.rlp_encoded_length() + 1
    }

    /// Encoded length including the network wrapper header.
    pub fn network_encoded_length(&self) -> usize {
        Header { list: false, payload_length: self.eip2718_encoded_length() }.length_with_payload()
    }

    /// Network encode the transaction.
    pub fn network_encode(&self, out: &mut dyn BufMut) {
        Header { list: false, payload_length: self.eip2718_encoded_length() }.encode(out);
        self.encode_2718(out);
    }

    /// Calculates a heuristic for the in-memory size of the transaction.
    pub fn size(&self) -> usize {
        mem::size_of::<SDMPayload>()
            + self.input.len()
            + self.payload.entries.len() * mem::size_of::<SDMGasEntry>()
    }

    /// Calculate the transaction hash.
    pub fn tx_hash(&self) -> TxHash {
        let mut buf = Vec::with_capacity(self.eip2718_encoded_length());
        self.encode_2718(&mut buf);
        keccak256(&buf)
    }
}

impl Typed2718 for TxSdm {
    fn ty(&self) -> u8 {
        SDM_TX_TYPE_ID
    }
}

impl IsTyped2718 for TxSdm {
    fn is_type(ty: u8) -> bool {
        ty == SDM_TX_TYPE_ID
    }
}

impl Transaction for TxSdm {
    fn chain_id(&self) -> Option<ChainId> {
        None
    }

    fn nonce(&self) -> u64 {
        0
    }

    fn gas_limit(&self) -> u64 {
        0
    }

    fn gas_price(&self) -> Option<u128> {
        None
    }

    fn max_fee_per_gas(&self) -> u128 {
        0
    }

    fn max_priority_fee_per_gas(&self) -> Option<u128> {
        None
    }

    fn max_fee_per_blob_gas(&self) -> Option<u128> {
        None
    }

    fn priority_fee_or_price(&self) -> u128 {
        0
    }

    fn effective_gas_price(&self, _: Option<u64>) -> u128 {
        0
    }

    fn is_dynamic_fee(&self) -> bool {
        false
    }

    fn kind(&self) -> TxKind {
        TxKind::Call(Default::default())
    }

    fn is_create(&self) -> bool {
        false
    }

    fn value(&self) -> U256 {
        U256::ZERO
    }

    fn input(&self) -> &Bytes {
        &self.input
    }

    fn access_list(&self) -> Option<&AccessList> {
        None
    }

    fn blob_versioned_hashes(&self) -> Option<&[B256]> {
        None
    }

    fn authorization_list(&self) -> Option<&[alloy_eips::eip7702::SignedAuthorization]> {
        None
    }
}

impl Encodable2718 for TxSdm {
    fn type_flag(&self) -> Option<u8> {
        Some(SDM_TX_TYPE_ID)
    }

    fn encode_2718_len(&self) -> usize {
        self.eip2718_encoded_length()
    }

    fn encode_2718(&self, out: &mut dyn alloy_rlp::BufMut) {
        out.put_u8(SDM_TX_TYPE_ID);
        out.put_slice(self.input.as_ref());
    }
}

impl Decodable2718 for TxSdm {
    fn typed_decode(ty: u8, data: &mut &[u8]) -> Eip2718Result<Self> {
        if ty != SDM_TX_TYPE_ID {
            return Err(Eip2718Error::UnexpectedType(ty));
        }
        Ok(Self::new(SDMPayload::decode(data)?))
    }

    fn fallback_decode(data: &mut &[u8]) -> Eip2718Result<Self> {
        Ok(Self::new(SDMPayload::decode(data)?))
    }
}

impl Encodable for TxSdm {
    fn encode(&self, out: &mut dyn BufMut) {
        out.put_slice(self.input.as_ref());
    }

    fn length(&self) -> usize {
        self.rlp_encoded_length()
    }
}

impl Decodable for TxSdm {
    fn decode(data: &mut &[u8]) -> alloy_rlp::Result<Self> {
        Ok(Self::new(SDMPayload::decode(data)?))
    }
}

impl Sealable for TxSdm {
    fn hash_slow(&self) -> B256 {
        self.tx_hash()
    }
}

#[cfg(feature = "serde")]
#[derive(Debug, Clone, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct TxSdmSerdeHelper {
    #[serde(default, skip_serializing_if = "Option::is_none", with = "alloy_serde::quantity::opt")]
    gas: Option<u64>,
    #[serde(default, skip_serializing_if = "Option::is_none", with = "alloy_serde::quantity::opt")]
    value: Option<u128>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    input: Option<Bytes>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    payload: Option<SDMPayload>,
}

#[cfg(feature = "serde")]
impl From<TxSdm> for TxSdmSerdeHelper {
    fn from(value: TxSdm) -> Self {
        Self { gas: Some(0), value: Some(0), input: Some(value.input), payload: None }
    }
}

#[cfg(feature = "serde")]
impl TryFrom<TxSdmSerdeHelper> for TxSdm {
    type Error = &'static str;

    fn try_from(value: TxSdmSerdeHelper) -> Result<Self, Self::Error> {
        if value.gas.is_some_and(|gas| gas != 0) {
            return Err("sdm transaction gas must be 0");
        }
        if value.value.is_some_and(|value| value != 0) {
            return Err("sdm transaction value must be 0");
        }

        let payload = if let Some(input) = value.input {
            SDMPayload::from_rlp_bytes(input.as_ref()).ok_or("invalid SDM transaction input")?
        } else if let Some(payload) = value.payload {
            payload
        } else {
            return Err("missing SDM transaction input");
        };

        Ok(Self::new(payload))
    }
}

#[cfg(feature = "alloy-compat")]
impl From<TxSdm> for alloy_rpc_types_eth::TransactionRequest {
    fn from(tx: TxSdm) -> Self {
        Self {
            from: Some(alloy_primitives::Address::ZERO),
            transaction_type: Some(SDM_TX_TYPE_ID),
            gas: Some(0),
            nonce: Some(0),
            value: Some(U256::ZERO),
            input: tx.input.into(),
            ..Default::default()
        }
    }
}

/// Build an SDM transaction from warming refund entries.
pub fn build_sdm_tx(entries: Vec<SDMGasEntry>) -> TxSdm {
    TxSdm::new(SDMPayload { version: 1, entries })
}

/// Check if a transaction type byte identifies an SDM transaction.
pub fn is_sdm_tx(ty: u8) -> bool {
    ty == SDM_TX_TYPE_ID
}

/// Extract the SDM payload from an SDM transaction.
pub fn extract_sdm_payload_from_tx(tx: &TxSdm) -> Option<SDMPayload> {
    Some(tx.payload.clone())
}

#[cfg(test)]
mod tests {
    use super::*;
    use alloy_rlp::{BytesMut, Decodable, Encodable};

    #[test]
    fn sdm_gas_entry_rlp_roundtrip() {
        let entry = SDMGasEntry { index: 2, gas_refund: 2500 };
        let mut buf = Vec::new();
        entry.encode(&mut buf);
        let decoded = SDMGasEntry::decode(&mut &buf[..]).unwrap();
        assert_eq!(entry, decoded);
    }

    #[test]
    fn sdm_payload_rlp_roundtrip() {
        let payload = SDMPayload {
            version: 1,
            entries: vec![
                SDMGasEntry { index: 2, gas_refund: 2500 },
                SDMGasEntry { index: 3, gas_refund: 4500 },
            ],
        };
        let mut buf = Vec::new();
        payload.encode(&mut buf);
        let decoded = SDMPayload::decode(&mut &buf[..]).unwrap();
        assert_eq!(payload, decoded);
    }

    #[test]
    fn sdm_payload_empty_entries() {
        let payload = SDMPayload { version: 1, entries: vec![] };
        let mut buf = Vec::new();
        payload.encode(&mut buf);
        let decoded = SDMPayload::decode(&mut &buf[..]).unwrap();
        assert_eq!(payload, decoded);
    }

    #[test]
    fn gas_refund_for_idx_found() {
        let payload = SDMPayload {
            version: 1,
            entries: vec![
                SDMGasEntry { index: 2, gas_refund: 2500 },
                SDMGasEntry { index: 3, gas_refund: 4500 },
            ],
        };
        assert_eq!(payload.gas_refund_for_idx(2), Some(2500));
        assert_eq!(payload.gas_refund_for_idx(3), Some(4500));
    }

    #[test]
    fn gas_refund_for_idx_not_found() {
        let payload =
            SDMPayload { version: 1, entries: vec![SDMGasEntry { index: 2, gas_refund: 2500 }] };
        assert_eq!(payload.gas_refund_for_idx(0), None);
        assert_eq!(payload.gas_refund_for_idx(99), None);
    }

    #[test]
    fn gas_refund_for_idx_empty() {
        let payload = SDMPayload { version: 1, entries: vec![] };
        assert_eq!(payload.gas_refund_for_idx(0), None);
    }

    #[test]
    fn sdm_payload_to_from_rlp_bytes() {
        let payload = SDMPayload {
            version: 1,
            entries: vec![
                SDMGasEntry { index: 2, gas_refund: 2500 },
                SDMGasEntry { index: 3, gas_refund: 4500 },
            ],
        };
        let bytes = payload.to_rlp_bytes();
        let decoded = SDMPayload::from_rlp_bytes(&bytes).unwrap();
        assert_eq!(payload, decoded);
    }

    #[test]
    fn build_sdm_tx_creates_valid_sdm() {
        let entries = vec![
            SDMGasEntry { index: 2, gas_refund: 2500 },
            SDMGasEntry { index: 3, gas_refund: 4500 },
        ];
        let tx = build_sdm_tx(entries.clone());

        assert_eq!(tx.ty(), SDM_TX_TYPE_ID);
        let extracted = extract_sdm_payload_from_tx(&tx).unwrap();
        assert_eq!(extracted.version, 1);
        assert_eq!(extracted.entries, entries);
    }

    #[test]
    fn is_sdm_tx_checks() {
        assert!(is_sdm_tx(SDM_TX_TYPE_ID));
        assert!(!is_sdm_tx(0x7E));
    }

    #[test]
    fn sdm_tx_eip2718_roundtrip() {
        let tx = build_sdm_tx(vec![SDMGasEntry { index: 1, gas_refund: 2500 }]);
        let encoded = tx.encoded_2718();
        let decoded = TxSdm::decode_2718(&mut encoded.as_ref()).unwrap();
        assert_eq!(decoded, tx);
    }

    #[test]
    fn sdm_tx_rlp_roundtrip() {
        let tx = build_sdm_tx(vec![SDMGasEntry { index: 1, gas_refund: 2500 }]);
        let mut buf = BytesMut::new();
        tx.encode(&mut buf);
        let decoded = TxSdm::decode(&mut buf.as_ref()).unwrap();
        assert_eq!(decoded, tx);
    }
}
