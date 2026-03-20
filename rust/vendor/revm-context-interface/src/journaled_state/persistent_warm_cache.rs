use primitives::{Address, HashSet, StorageKey};

/// Tracks addresses and storage slots that have been accessed.
/// Persists across transactions for the lifetime of the EVM instance.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
pub struct PersistentWarmCache {
    warm_addresses: HashSet<Address>,
    warm_storage: HashSet<(Address, StorageKey)>,
}

impl PersistentWarmCache {
    /// Creates a new empty persistent warm cache.
    pub fn new() -> Self {
        Self::default()
    }

    /// Marks an account as warm.
    #[inline]
    pub fn warm_account(&mut self, address: Address) {
        self.warm_addresses.insert(address);
    }

    /// Marks a storage slot as warm for the given address.
    /// Also marks the address as warm if it was not already.
    #[inline]
    pub fn warm_storage(&mut self, address: Address, key: StorageKey) {
        self.warm_addresses.insert(address);
        self.warm_storage.insert((address, key));
    }

    /// Returns true if the address has been warmed by a prior transaction.
    #[inline]
    pub fn is_address_warm(&self, address: &Address) -> bool {
        self.warm_addresses.contains(address)
    }

    /// Returns true if the slot has been warmed by a prior transaction.
    #[inline]
    pub fn is_storage_warm(&self, address: &Address, key: &StorageKey) -> bool {
        self.warm_storage.contains(&(*address, *key))
    }
}
