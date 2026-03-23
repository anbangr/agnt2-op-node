use primitives::{Address, HashMap, StorageKey};

/// Provenance for the first transaction that warmed an account or storage slot.
#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
pub struct WarmAccessProvenance {
    /// Replay-local transaction index that first warmed this item.
    pub first_warmed_by_tx_index: u64,
    /// Whether the index above came from a real block transaction.
    ///
    /// Non-transaction system calls can participate in the persistent warm cache,
    /// but they should not permanently steal tx provenance from the first real tx
    /// that later touches the same item.
    pub has_transaction_provenance: bool,
}

impl WarmAccessProvenance {
    /// Provenance captured from a real block transaction.
    #[inline]
    pub const fn from_tx_index(first_warmed_by_tx_index: u64) -> Self {
        Self { first_warmed_by_tx_index, has_transaction_provenance: true }
    }

    /// Provenance with no corresponding block transaction yet.
    #[inline]
    pub const fn unknown() -> Self {
        Self { first_warmed_by_tx_index: 0, has_transaction_provenance: false }
    }
}

/// Exact refund categories for SDM block-level warming.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
#[cfg_attr(feature = "serde", serde(rename_all = "snake_case"))]
pub enum WarmingRefundKind {
    /// Warm account rebate (+2500).
    WarmAccount,
    /// Warm storage read rebate (+2000).
    WarmSload,
    /// Warm storage write rebate (+2100).
    WarmSstore,
}

/// Internal refund attribution event emitted exactly when a warming rebate is granted.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
pub struct WarmingRefundEvent {
    /// Replay-local transaction index that claimed the rebate.
    pub claiming_tx_index: u64,
    /// Refund kind.
    pub kind: WarmingRefundKind,
    /// Rebate amount in gas.
    pub amount: u64,
    /// Account touched by the rebate.
    pub address: Address,
    /// Storage slot touched by the rebate, when applicable.
    pub slot: Option<StorageKey>,
    /// Replay-local transaction index that first warmed this account or slot.
    pub first_warmed_by_tx_index: u64,
}

/// Tracks addresses and storage slots that have been accessed.
/// Persists across transactions for the lifetime of the EVM instance.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
pub struct PersistentWarmCache {
    warm_addresses: HashMap<Address, WarmAccessProvenance>,
    warm_storage: HashMap<(Address, StorageKey), WarmAccessProvenance>,
}

impl PersistentWarmCache {
    /// Creates a new empty persistent warm cache.
    pub fn new() -> Self {
        Self::default()
    }

    /// Marks an account as warm, preserving the first-warming provenance.
    #[inline]
    pub fn warm_account_with_provenance(
        &mut self,
        address: Address,
        provenance: WarmAccessProvenance,
    ) {
        self.warm_addresses
            .entry(address)
            .and_modify(|current| {
                if !current.has_transaction_provenance && provenance.has_transaction_provenance {
                    *current = provenance;
                }
            })
            .or_insert(provenance);
    }

    /// Marks a storage slot as warm for the given address, preserving the first-warming provenance.
    /// Also marks the address as warm if it was not already.
    #[inline]
    pub fn warm_storage_with_provenance(
        &mut self,
        address: Address,
        key: StorageKey,
        provenance: WarmAccessProvenance,
    ) {
        self.warm_account_with_provenance(address, provenance);
        self.warm_storage
            .entry((address, key))
            .and_modify(|current| {
                if !current.has_transaction_provenance && provenance.has_transaction_provenance {
                    *current = provenance;
                }
            })
            .or_insert(provenance);
    }

    /// Returns true if the address has been warmed by a prior transaction.
    #[inline]
    pub fn is_address_warm(&self, address: &Address) -> bool {
        self.warm_addresses.contains_key(address)
    }

    /// Returns true if the slot has been warmed by a prior transaction.
    #[inline]
    pub fn is_storage_warm(&self, address: &Address, key: &StorageKey) -> bool {
        self.warm_storage.contains_key(&(*address, *key))
    }

    /// Returns provenance for the warmed account, if any.
    #[inline]
    pub fn address_provenance(&self, address: &Address) -> Option<&WarmAccessProvenance> {
        self.warm_addresses.get(address)
    }

    /// Returns provenance for the warmed storage slot, if any.
    #[inline]
    pub fn storage_provenance(
        &self,
        address: &Address,
        key: &StorageKey,
    ) -> Option<&WarmAccessProvenance> {
        self.warm_storage.get(&(*address, *key))
    }
}
