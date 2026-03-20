//! Module containing the [`JournalInner`] that is part of [`crate::Journal`].
use super::warm_addresses::WarmAddresses;
use bytecode::Bytecode;
use context_interface::{
    context::{SStoreResult, SelfDestructResult, StateLoad},
    journaled_state::{
        AccountLoad, JournalCheckpoint, JournalLoadError, TransferError,
        account::{JournaledAccount, JournaledAccountTr},
        entry::{JournalEntryTr, SelfdestructionRevertStatus},
        persistent_warm_cache::PersistentWarmCache,
    },
};
use core::mem;
use database_interface::Database;
use primitives::{
    Address, B256, HashMap, KECCAK_EMPTY, Log, StorageKey, StorageValue, U256,
    hardfork::SpecId::{self, *},
    hash_map::Entry,
    hints_util::unlikely,
};
use state::{Account, EvmState, TransientStorage};
use std::vec::Vec;
/// Inner journal state that contains journal and state changes.
///
/// Spec Id is a essential information for the Journal.
#[derive(Debug, Clone, PartialEq, Eq)]
#[cfg_attr(feature = "serde", derive(serde::Serialize, serde::Deserialize))]
pub struct JournalInner<ENTRY> {
    /// The current state
    pub state: EvmState,
    /// Transient storage that is discarded after every transaction.
    ///
    /// See [EIP-1153](https://eips.ethereum.org/EIPS/eip-1153).
    pub transient_storage: TransientStorage,
    /// Emitted logs
    pub logs: Vec<Log>,
    /// The current call stack depth
    pub depth: usize,
    /// The journal of state changes, one for each transaction
    pub journal: Vec<ENTRY>,
    /// Global transaction id that represent number of transactions executed (Including reverted ones).
    /// It can be different from number of `journal_history` as some transaction could be
    /// reverted or had a error on execution.
    ///
    /// This ID is used in `Self::state` to determine if account/storage is touched/warm/cold.
    pub transaction_id: usize,
    /// The spec ID for the EVM. Spec is required for some journal entries and needs to be set for
    /// JournalInner to be functional.
    ///
    /// If spec is set it assumed that precompile addresses are set as well for this particular spec.
    ///
    /// This spec is used for two things:
    ///
    /// - [EIP-161]: Prior to this EIP, Ethereum had separate definitions for empty and non-existing accounts.
    /// - [EIP-6780]: `SELFDESTRUCT` only in same transaction
    ///
    /// [EIP-161]: https://eips.ethereum.org/EIPS/eip-161
    /// [EIP-6780]: https://eips.ethereum.org/EIPS/eip-6780
    pub spec: SpecId,
    /// Warm addresses containing both coinbase and current precompiles.
    pub warm_addresses: WarmAddresses,
    /// Cross-transaction warming cache that persists for the EVM instance lifetime.
    pub persistent_warm_cache: Option<PersistentWarmCache>,
    /// Savings accumulated during the current transaction.
    pub tx_warming_savings: u64,
    /// Savings accumulated during the most recently committed transaction.
    pub last_tx_warming_savings: u64,
}

impl<ENTRY: JournalEntryTr> Default for JournalInner<ENTRY> {
    fn default() -> Self {
        Self::new()
    }
}

impl<ENTRY: JournalEntryTr> JournalInner<ENTRY> {
    /// Creates new [`JournalInner`].
    ///
    /// `warm_preloaded_addresses` is used to determine if address is considered warm loaded.
    /// In ordinary case this is precompile or beneficiary.
    pub fn new() -> JournalInner<ENTRY> {
        Self {
            state: HashMap::default(),
            transient_storage: TransientStorage::default(),
            logs: Vec::new(),
            journal: Vec::default(),
            transaction_id: 0,
            depth: 0,
            spec: SpecId::default(),
            warm_addresses: WarmAddresses::new(),
            persistent_warm_cache: None,
            tx_warming_savings: 0,
            last_tx_warming_savings: 0,
        }
    }

    /// Enable persistent warming for the EVM instance lifetime.
    #[inline]
    pub fn enable_persistent_warming(&mut self) {
        self.persistent_warm_cache = Some(PersistentWarmCache::new());
        self.tx_warming_savings = 0;
        self.last_tx_warming_savings = 0;
    }

    /// Disable persistent warming.
    #[inline]
    pub fn disable_persistent_warming(&mut self) {
        self.persistent_warm_cache = None;
        self.tx_warming_savings = 0;
        self.last_tx_warming_savings = 0;
    }

    /// Returns true if persistent warming is enabled.
    #[inline]
    pub fn is_persistent_warming_enabled(&self) -> bool {
        self.persistent_warm_cache.is_some()
    }

    /// Returns the current transaction's warming savings without resetting them.
    #[inline]
    pub const fn current_tx_warming_savings(&self) -> u64 {
        self.tx_warming_savings
    }

    /// Take the savings recorded for the most recently committed transaction.
    #[inline]
    pub fn take_last_tx_warming_savings(&mut self) -> u64 {
        mem::take(&mut self.last_tx_warming_savings)
    }

    /// Returns the logs
    #[inline]
    pub fn take_logs(&mut self) -> Vec<Log> {
        mem::take(&mut self.logs)
    }

    /// Prepare for next transaction, by committing the current journal to history, incrementing the transaction id
    /// and returning the logs.
    ///
    /// This function is used to prepare for next transaction. It will save the current journal
    /// and clear the journal for the next transaction.
    ///
    /// `commit_tx` is used even for discarding transactions so transaction_id will be incremented.
    pub fn commit_tx(&mut self) {
        // Clears all field from JournalInner. Doing it this way to avoid
        // missing any field.
        let Self {
            state,
            transient_storage,
            logs,
            depth,
            journal,
            transaction_id,
            spec,
            warm_addresses,
            persistent_warm_cache: _,
            tx_warming_savings,
            last_tx_warming_savings,
        } = self;
        // Spec, precompiles, BAL and state are not changed. It is always set again execution.
        let _ = spec;
        let _ = state;
        transient_storage.clear();
        *depth = 0;

        // Do nothing with journal history so we can skip cloning present journal.
        journal.clear();

        // Clear coinbase address warming for next tx
        warm_addresses.clear_coinbase_and_access_list();
        // increment transaction id.
        *transaction_id += 1;
        *last_tx_warming_savings = mem::take(tx_warming_savings);

        logs.clear();
    }

    /// Discard the current transaction, by reverting the journal entries and incrementing the transaction id.
    pub fn discard_tx(&mut self) {
        // if there is no journal entries, there has not been any changes.
        let Self {
            state,
            transient_storage,
            logs,
            depth,
            journal,
            transaction_id,
            spec,
            warm_addresses,
            persistent_warm_cache: _,
            tx_warming_savings,
            last_tx_warming_savings,
        } = self;
        let is_spurious_dragon_enabled = spec.is_enabled_in(SPURIOUS_DRAGON);
        // iterate over all journals entries and revert our global state
        journal.drain(..).rev().for_each(|entry| {
            entry.revert(state, None, is_spurious_dragon_enabled);
        });
        transient_storage.clear();
        *depth = 0;
        logs.clear();
        *transaction_id += 1;
        *tx_warming_savings = 0;
        *last_tx_warming_savings = 0;

        // Clear coinbase address warming for next tx
        warm_addresses.clear_coinbase_and_access_list();
    }

    /// Take the [`EvmState`] and clears the journal by resetting it to initial state.
    ///
    /// Note: Precompile addresses and spec are preserved and initial state of
    /// warm_preloaded_addresses will contain precompiles addresses.
    #[inline]
    pub fn finalize(&mut self) -> EvmState {
        // Clears all field from JournalInner. Doing it this way to avoid
        // missing any field.
        let Self {
            state,
            transient_storage,
            logs,
            depth,
            journal,
            transaction_id,
            spec,
            warm_addresses,
            persistent_warm_cache: _,
            tx_warming_savings,
            last_tx_warming_savings,
        } = self;
        // Spec is not changed. And it is always set again in execution.
        let _ = spec;
        // Clear coinbase address warming for next tx
        warm_addresses.clear_coinbase_and_access_list();
        *tx_warming_savings = 0;
        *last_tx_warming_savings = 0;

        let state = mem::take(state);
        logs.clear();
        transient_storage.clear();

        // clear journal and journal history.
        journal.clear();
        *depth = 0;
        // reset transaction id.
        *transaction_id = 0;

        state
    }

    /// Return reference to state.
    #[inline]
    pub fn state(&mut self) -> &mut EvmState {
        &mut self.state
    }

    /// Sets SpecId.
    #[inline]
    pub fn set_spec_id(&mut self, spec: SpecId) {
        self.spec = spec;
    }

    /// Mark account as touched as only touched accounts will be added to state.
    /// This is especially important for state clear where touched empty accounts needs to
    /// be removed from state.
    #[inline]
    pub fn touch(&mut self, address: Address) {
        if let Some(account) = self.state.get_mut(&address) {
            Self::touch_account(&mut self.journal, address, account);
        }
    }

    /// Mark account as touched.
    #[inline]
    fn touch_account(journal: &mut Vec<ENTRY>, address: Address, account: &mut Account) {
        if !account.is_touched() {
            journal.push(ENTRY::account_touched(address));
            account.mark_touch();
        }
    }

    /// Returns the _loaded_ [Account] for the given address.
    ///
    /// This assumes that the account has already been loaded.
    ///
    /// # Panics
    ///
    /// Panics if the account has not been loaded and is missing from the state set.
    #[inline]
    pub fn account(&self, address: Address) -> &Account {
        self.state.get(&address).expect("Account expected to be loaded") // Always assume that acc is already loaded
    }

    /// Set code and its hash to the account.
    ///
    /// Note: Assume account is warm and that hash is calculated from code.
    #[inline]
    pub fn set_code_with_hash(&mut self, address: Address, code: Bytecode, hash: B256) {
        let account = self.state.get_mut(&address).unwrap();
        Self::touch_account(&mut self.journal, address, account);

        self.journal.push(ENTRY::code_changed(address));

        account.info.code_hash = hash;
        account.info.code = Some(code);
    }

    /// Use it only if you know that acc is warm.
    ///
    /// Assume account is warm.
    ///
    /// In case of EIP-7702 code with zero address, the bytecode will be erased.
    #[inline]
    pub fn set_code(&mut self, address: Address, code: Bytecode) {
        if let Bytecode::Eip7702(eip7702_bytecode) = &code {
            if eip7702_bytecode.address().is_zero() {
                self.set_code_with_hash(address, Bytecode::default(), KECCAK_EMPTY);
                return;
            }
        }

        let hash = code.hash_slow();
        self.set_code_with_hash(address, code, hash)
    }

    /// Add journal entry for caller accounting.
    #[inline]
    pub fn caller_accounting_journal_entry(
        &mut self,
        address: Address,
        old_balance: U256,
        bump_nonce: bool,
    ) {
        // account balance changed.
        self.journal.push(ENTRY::balance_changed(address, old_balance));
        // account is touched.
        self.journal.push(ENTRY::account_touched(address));

        if bump_nonce {
            // nonce changed.
            self.journal.push(ENTRY::nonce_bumped(address));
        }
    }

    /// Increments the balance of the account.
    ///
    /// Mark account as touched.
    #[inline]
    pub fn balance_incr<DB: Database>(
        &mut self,
        db: &mut DB,
        address: Address,
        balance: U256,
    ) -> Result<(), DB::Error> {
        let mut account = self.load_account_mut(db, address)?.data;
        account.incr_balance(balance);
        Ok(())
    }

    /// Increments the nonce of the account.
    #[inline]
    pub fn nonce_bump_journal_entry(&mut self, address: Address) {
        self.journal.push(ENTRY::nonce_bumped(address));
    }

    /// Transfers balance from two accounts. Returns error if sender balance is not enough.
    ///
    /// # Panics
    ///
    /// Panics if from or to are not loaded.
    #[inline]
    pub fn transfer_loaded(
        &mut self,
        from: Address,
        to: Address,
        balance: U256,
    ) -> Option<TransferError> {
        if from == to {
            let from_balance = self.state.get_mut(&to).unwrap().info.balance;
            // Check if from balance is enough to transfer the balance.
            if balance > from_balance {
                return Some(TransferError::OutOfFunds);
            }
            return None;
        }

        if balance.is_zero() {
            Self::touch_account(&mut self.journal, to, self.state.get_mut(&to).unwrap());
            return None;
        }

        // sub balance from
        let from_account = self.state.get_mut(&from).unwrap();
        Self::touch_account(&mut self.journal, from, from_account);
        let from_balance = &mut from_account.info.balance;
        let Some(from_balance_decr) = from_balance.checked_sub(balance) else {
            return Some(TransferError::OutOfFunds);
        };
        *from_balance = from_balance_decr;

        // add balance to
        let to_account = self.state.get_mut(&to).unwrap();
        Self::touch_account(&mut self.journal, to, to_account);
        let to_balance = &mut to_account.info.balance;
        let Some(to_balance_incr) = to_balance.checked_add(balance) else {
            // Overflow of U256 balance is not possible to happen on mainnet. We don't bother to return funds from from_acc.
            return Some(TransferError::OverflowPayment);
        };
        *to_balance = to_balance_incr;

        // add journal entry
        self.journal.push(ENTRY::balance_transfer(from, to, balance));

        None
    }

    /// Transfers balance from two accounts. Returns error if sender balance is not enough.
    #[inline]
    pub fn transfer<DB: Database>(
        &mut self,
        db: &mut DB,
        from: Address,
        to: Address,
        balance: U256,
    ) -> Result<Option<TransferError>, DB::Error> {
        self.load_account(db, from)?;
        self.load_account(db, to)?;
        Ok(self.transfer_loaded(from, to, balance))
    }

    /// Creates account or returns false if collision is detected.
    ///
    /// There are few steps done:
    /// 1. Make created account warm loaded (AccessList) and this should
    ///    be done before subroutine checkpoint is created.
    /// 2. Check if there is collision of newly created account with existing one.
    /// 3. Mark created account as created.
    /// 4. Add fund to created account
    /// 5. Increment nonce of created account if SpuriousDragon is active
    /// 6. Decrease balance of caller account.
    ///
    /// # Panics
    ///
    /// Panics if the caller is not loaded inside the EVM state.
    /// This should have been done inside `create_inner`.
    #[inline]
    pub fn create_account_checkpoint(
        &mut self,
        caller: Address,
        target_address: Address,
        balance: U256,
        spec_id: SpecId,
    ) -> Result<JournalCheckpoint, TransferError> {
        // Enter subroutine
        let checkpoint = self.checkpoint();

        // Newly created account is present, as we just loaded it.
        let target_acc = self.state.get_mut(&target_address).unwrap();
        let last_journal = &mut self.journal;

        // New account can be created if:
        // Bytecode is not empty.
        // Nonce is not zero
        // Account is not precompile.
        if target_acc.info.code_hash != KECCAK_EMPTY || target_acc.info.nonce != 0 {
            self.checkpoint_revert(checkpoint);
            return Err(TransferError::CreateCollision);
        }

        // set account status to create.
        let is_created_globally = target_acc.mark_created_locally();

        // this entry will revert set nonce.
        last_journal.push(ENTRY::account_created(target_address, is_created_globally));
        target_acc.info.code = None;
        // EIP-161: State trie clearing (invariant-preserving alternative)
        if spec_id.is_enabled_in(SPURIOUS_DRAGON) {
            // nonce is going to be reset to zero in AccountCreated journal entry.
            target_acc.info.nonce = 1;
        }

        // touch account. This is important as for pre SpuriousDragon account could be
        // saved even empty.
        Self::touch_account(last_journal, target_address, target_acc);

        // Add balance to created account, as we already have target here.
        let Some(new_balance) = target_acc.info.balance.checked_add(balance) else {
            self.checkpoint_revert(checkpoint);
            return Err(TransferError::OverflowPayment);
        };
        target_acc.info.balance = new_balance;

        // safe to decrement for the caller as balance check is already done.
        let caller_account = self.state.get_mut(&caller).unwrap();
        caller_account.info.balance -= balance;

        // add journal entry of transferred balance
        last_journal.push(ENTRY::balance_transfer(caller, target_address, balance));

        Ok(checkpoint)
    }

    /// Makes a checkpoint that in case of Revert can bring back state to this point.
    #[inline]
    pub fn checkpoint(&mut self) -> JournalCheckpoint {
        let checkpoint =
            JournalCheckpoint { log_i: self.logs.len(), journal_i: self.journal.len() };
        self.depth += 1;
        checkpoint
    }

    /// Commits the checkpoint.
    #[inline]
    pub fn checkpoint_commit(&mut self) {
        self.depth = self.depth.saturating_sub(1);
    }

    /// Reverts all changes to state until given checkpoint.
    #[inline]
    pub fn checkpoint_revert(&mut self, checkpoint: JournalCheckpoint) {
        let is_spurious_dragon_enabled = self.spec.is_enabled_in(SPURIOUS_DRAGON);
        let state = &mut self.state;
        let transient_storage = &mut self.transient_storage;
        self.depth = self.depth.saturating_sub(1);
        self.logs.truncate(checkpoint.log_i);

        // iterate over last N journals sets and revert our global state
        if checkpoint.journal_i < self.journal.len() {
            self.journal.drain(checkpoint.journal_i..).rev().for_each(|entry| {
                entry.revert(state, Some(transient_storage), is_spurious_dragon_enabled);
            });
        }
    }

    /// Performs selfdestruct action.
    /// Transfers balance from address to target. Check if target exist/is_cold
    ///
    /// Note: Balance will be lost if address and target are the same BUT when
    /// current spec enables Cancun, this happens only when the account associated to address
    /// is created in the same tx
    ///
    /// # References:
    ///  * <https://github.com/ethereum/go-ethereum/blob/141cd425310b503c5678e674a8c3872cf46b7086/core/vm/instructions.go#L832-L833>
    ///  * <https://github.com/ethereum/go-ethereum/blob/141cd425310b503c5678e674a8c3872cf46b7086/core/state/statedb.go#L449>
    ///  * <https://eips.ethereum.org/EIPS/eip-6780>
    #[inline]
    pub fn selfdestruct<DB: Database>(
        &mut self,
        db: &mut DB,
        address: Address,
        target: Address,
        skip_cold_load: bool,
    ) -> Result<StateLoad<SelfDestructResult>, JournalLoadError<DB::Error>> {
        let spec = self.spec;
        let account_load = self.load_account_optional(db, target, false, skip_cold_load)?;
        let is_cold = account_load.is_cold;
        let is_empty = account_load.state_clear_aware_is_empty(spec);

        if address != target {
            // Both accounts are loaded before this point, `address` as we execute its contract.
            // and `target` at the beginning of the function.
            let acc_balance = self.state.get(&address).unwrap().info.balance;

            let target_account = self.state.get_mut(&target).unwrap();
            Self::touch_account(&mut self.journal, target, target_account);
            target_account.info.balance += acc_balance;
        }

        let acc = self.state.get_mut(&address).unwrap();
        let balance = acc.info.balance;

        let destroyed_status = if !acc.is_selfdestructed() {
            SelfdestructionRevertStatus::GloballySelfdestroyed
        } else if !acc.is_selfdestructed_locally() {
            SelfdestructionRevertStatus::LocallySelfdestroyed
        } else {
            SelfdestructionRevertStatus::RepeatedSelfdestruction
        };

        let is_cancun_enabled = spec.is_enabled_in(CANCUN);

        // EIP-6780 (Cancun hard-fork): selfdestruct only if contract is created in the same tx
        let journal_entry = if acc.is_created_locally() || !is_cancun_enabled {
            acc.mark_selfdestructed_locally();
            acc.info.balance = U256::ZERO;
            Some(ENTRY::account_destroyed(address, target, destroyed_status, balance))
        } else if address != target {
            acc.info.balance = U256::ZERO;
            Some(ENTRY::balance_transfer(address, target, balance))
        } else {
            // State is not changed:
            // * if we are after Cancun upgrade and
            // * Selfdestruct account that is created in the same transaction and
            // * Specify the target is same as selfdestructed account. The balance stays unchanged.
            None
        };

        if let Some(entry) = journal_entry {
            self.journal.push(entry);
        };

        Ok(StateLoad {
            data: SelfDestructResult {
                had_value: !balance.is_zero(),
                target_exists: !is_empty,
                previously_destroyed: destroyed_status
                    == SelfdestructionRevertStatus::RepeatedSelfdestruction,
            },
            is_cold,
        })
    }

    /// Loads account into memory. return if it is cold or warm accessed
    #[inline]
    pub fn load_account<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
    ) -> Result<StateLoad<&'a Account>, DB::Error>
    where
        'db: 'a,
    {
        self.load_account_optional(db, address, false, false)
            .map_err(JournalLoadError::unwrap_db_error)
    }

    /// Loads account into memory. If account is EIP-7702 type it will additionally
    /// load delegated account.
    ///
    /// It will mark both this and delegated account as warm loaded.
    ///
    /// Returns information about the account (If it is empty or cold loaded) and if present the information
    /// about the delegated account (If it is cold loaded).
    #[inline]
    pub fn load_account_delegated<DB: Database>(
        &mut self,
        db: &mut DB,
        address: Address,
    ) -> Result<StateLoad<AccountLoad>, DB::Error> {
        let spec = self.spec;
        let is_eip7702_enabled = spec.is_enabled_in(SpecId::PRAGUE);
        let account = self
            .load_account_optional(db, address, is_eip7702_enabled, false)
            .map_err(JournalLoadError::unwrap_db_error)?;
        let is_empty = account.state_clear_aware_is_empty(spec);

        let mut account_load = StateLoad::new(
            AccountLoad { is_delegate_account_cold: None, is_empty },
            account.is_cold,
        );

        // load delegate code if account is EIP-7702
        if let Some(Bytecode::Eip7702(code)) = &account.info.code {
            let address = code.address();
            let delegate_account = self
                .load_account_optional(db, address, true, false)
                .map_err(JournalLoadError::unwrap_db_error)?;
            account_load.data.is_delegate_account_cold = Some(delegate_account.is_cold);
        }

        Ok(account_load)
    }

    /// Loads account and its code. If account is already loaded it will load its code.
    ///
    /// It will mark account as warm loaded. If not existing Database will be queried for data.
    ///
    /// In case of EIP-7702 delegated account will not be loaded,
    /// [`Self::load_account_delegated`] should be used instead.
    #[inline]
    pub fn load_code<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
    ) -> Result<StateLoad<&'a Account>, DB::Error>
    where
        'db: 'a,
    {
        self.load_account_optional(db, address, true, false)
            .map_err(JournalLoadError::unwrap_db_error)
    }

    /// Loads account into memory. If account is already loaded it will be marked as warm.
    #[inline]
    pub fn load_account_optional<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
        load_code: bool,
        skip_cold_load: bool,
    ) -> Result<StateLoad<&'a Account>, JournalLoadError<DB::Error>>
    where
        'db: 'a,
    {
        let mut load = self.load_account_mut_optional(db, address, skip_cold_load)?;
        if load_code {
            load.data.load_code_preserve_error()?;
        }
        Ok(load.map(|i| i.into_account()))
    }

    /// Loads account into memory. If account is already loaded it will be marked as warm.
    #[inline]
    pub fn load_account_mut<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
    ) -> Result<StateLoad<JournaledAccount<'a, DB, ENTRY>>, DB::Error>
    where
        'db: 'a,
    {
        self.load_account_mut_optional(db, address, false)
            .map_err(JournalLoadError::unwrap_db_error)
    }

    /// Loads account. If account is already loaded it will be marked as warm.
    #[inline]
    pub fn load_account_mut_optional_code<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
        load_code: bool,
        skip_cold_load: bool,
    ) -> Result<StateLoad<JournaledAccount<'a, DB, ENTRY>>, JournalLoadError<DB::Error>>
    where
        'db: 'a,
    {
        let mut load = self.load_account_mut_optional(db, address, skip_cold_load)?;
        if load_code {
            load.data.load_code_preserve_error()?;
        }
        Ok(load)
    }

    /// Gets the account mut reference.
    ///
    /// # Load Unsafe
    ///
    /// Use this function only if you know what you are doing. It will not mark the account as warm or cold.
    /// It will not bump transition_id or return if it is cold or warm loaded. This function is useful
    /// when we know account is warm, touched and already loaded.
    ///
    /// It is useful when we want to access storage from account that is currently being executed.
    #[inline]
    pub fn get_account_mut<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
    ) -> Option<JournaledAccount<'a, DB, ENTRY>>
    where
        'db: 'a,
    {
        let account = self.state.get_mut(&address)?;
        Some(JournaledAccount::new(
            address,
            account,
            &mut self.journal,
            db,
            self.warm_addresses.access_list(),
            self.persistent_warm_cache.as_mut(),
            &mut self.tx_warming_savings,
            self.transaction_id,
        ))
    }

    #[inline]
    fn load_account_mut_optional_with_rebate<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
        skip_cold_load: bool,
        record_account_rebate: bool,
    ) -> Result<StateLoad<JournaledAccount<'a, DB, ENTRY>>, JournalLoadError<DB::Error>>
    where
        'db: 'a,
    {
        let (account, is_cold) = match self.state.entry(address) {
            Entry::Occupied(entry) => {
                let account = entry.into_mut();

                let mut is_cold = account.is_cold_transaction_id(self.transaction_id);

                if unlikely(is_cold) {
                    let tx_access_cold = self.warm_addresses.is_cold(&address);
                    let persistent_warm = self
                        .persistent_warm_cache
                        .as_ref()
                        .is_some_and(|cache| cache.is_address_warm(&address));

                    if tx_access_cold && persistent_warm && record_account_rebate {
                        self.tx_warming_savings += 2500;
                    }
                    is_cold = self.warm_addresses.check_is_cold(&address, skip_cold_load)?;

                    account.mark_warm_with_transaction_id(self.transaction_id);

                    if account.is_selfdestructed_locally() {
                        account.selfdestruct();
                        account.unmark_selfdestructed_locally();
                    }
                    *account.original_info = account.info.clone();
                    account.unmark_created_locally();
                    self.journal.push(ENTRY::account_warmed(address));

                    if let Some(cache) = &mut self.persistent_warm_cache {
                        cache.warm_account(address);
                    }
                }
                (account, is_cold)
            }
            Entry::Vacant(vac) => {
                let tx_access_cold = self.warm_addresses.is_cold(&address);
                let persistent_warm = self
                    .persistent_warm_cache
                    .as_ref()
                    .is_some_and(|cache| cache.is_address_warm(&address));
                if tx_access_cold && persistent_warm && record_account_rebate {
                    self.tx_warming_savings += 2500;
                }
                let is_cold = self.warm_addresses.check_is_cold(&address, skip_cold_load)?;

                let account = if let Some(account) = db.basic(address)? {
                    let mut account: Account = account.into();
                    account.transaction_id = self.transaction_id;
                    account
                } else {
                    Account::new_not_existing(self.transaction_id)
                };

                if is_cold {
                    self.journal.push(ENTRY::account_warmed(address));
                }

                if let Some(cache) = &mut self.persistent_warm_cache {
                    cache.warm_account(address);
                }

                (vac.insert(account), is_cold)
            }
        };

        Ok(StateLoad::new(
            JournaledAccount::new(
                address,
                account,
                &mut self.journal,
                db,
                self.warm_addresses.access_list(),
                self.persistent_warm_cache.as_mut(),
                &mut self.tx_warming_savings,
                self.transaction_id,
            ),
            is_cold,
        ))
    }

    /// Loads account. If account is already loaded it will be marked as warm.
    #[inline(never)]
    pub fn load_account_mut_optional<'a, 'db, DB: Database>(
        &'a mut self,
        db: &'db mut DB,
        address: Address,
        skip_cold_load: bool,
    ) -> Result<StateLoad<JournaledAccount<'a, DB, ENTRY>>, JournalLoadError<DB::Error>>
    where
        'db: 'a,
    {
        self.load_account_mut_optional_with_rebate(db, address, skip_cold_load, true)
    }

    /// Loads storage slot.
    #[inline]
    pub fn sload<DB: Database>(
        &mut self,
        db: &mut DB,
        address: Address,
        key: StorageKey,
        skip_cold_load: bool,
    ) -> Result<StateLoad<StorageValue>, JournalLoadError<DB::Error>> {
        self.load_account_mut_optional_with_rebate(db, address, false, false)?
            .sload_concrete_error(key, skip_cold_load)
            .map(|s| s.map(|s| s.present_value))
    }

    /// Loads storage slot.
    ///
    /// If account is not present it will return [`JournalLoadError::ColdLoadSkipped`] error.
    #[inline]
    pub fn sload_assume_account_present<DB: Database>(
        &mut self,
        db: &mut DB,
        address: Address,
        key: StorageKey,
        skip_cold_load: bool,
    ) -> Result<StateLoad<StorageValue>, JournalLoadError<DB::Error>> {
        let Some(mut account) = self.get_account_mut(db, address) else {
            return Err(JournalLoadError::ColdLoadSkipped);
        };

        account.sload_concrete_error(key, skip_cold_load).map(|s| s.map(|s| s.present_value))
    }

    /// Stores storage slot.
    ///
    /// If account is not present it will load from database
    #[inline]
    pub fn sstore<DB: Database>(
        &mut self,
        db: &mut DB,
        address: Address,
        key: StorageKey,
        new: StorageValue,
        skip_cold_load: bool,
    ) -> Result<StateLoad<SStoreResult>, JournalLoadError<DB::Error>> {
        self.load_account_mut_optional_with_rebate(db, address, false, false)?
            .sstore_concrete_error(key, new, skip_cold_load)
    }

    /// Stores storage slot.
    ///
    /// And returns (original,present,new) slot value.
    ///
    /// **Note**: Account should already be present in our state.
    #[inline]
    pub fn sstore_assume_account_present<DB: Database>(
        &mut self,
        db: &mut DB,
        address: Address,
        key: StorageKey,
        new: StorageValue,
        skip_cold_load: bool,
    ) -> Result<StateLoad<SStoreResult>, JournalLoadError<DB::Error>> {
        let Some(mut account) = self.get_account_mut(db, address) else {
            return Err(JournalLoadError::ColdLoadSkipped);
        };

        account.sstore_concrete_error(key, new, skip_cold_load)
    }

    /// Read transient storage tied to the account.
    ///
    /// EIP-1153: Transient storage opcodes
    #[inline]
    pub fn tload(&mut self, address: Address, key: StorageKey) -> StorageValue {
        self.transient_storage.get(&(address, key)).copied().unwrap_or_default()
    }

    /// Store transient storage tied to the account.
    ///
    /// If values is different add entry to the journal
    /// so that old state can be reverted if that action is needed.
    ///
    /// EIP-1153: Transient storage opcodes
    #[inline]
    pub fn tstore(&mut self, address: Address, key: StorageKey, new: StorageValue) {
        let had_value = if new.is_zero() {
            // if new values is zero, remove entry from transient storage.
            // if previous values was some insert it inside journal.
            // If it is none nothing should be inserted.
            self.transient_storage.remove(&(address, key))
        } else {
            // insert values
            let previous_value =
                self.transient_storage.insert((address, key), new).unwrap_or_default();

            // check if previous value is same
            if previous_value != new {
                // if it is different, insert previous values inside journal.
                Some(previous_value)
            } else {
                None
            }
        };

        if let Some(had_value) = had_value {
            // insert in journal only if value was changed.
            self.journal.push(ENTRY::transient_storage_changed(address, key, had_value));
        }
    }

    /// Pushes log into subroutine.
    #[inline]
    pub fn log(&mut self, log: Log) {
        self.logs.push(log);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use context_interface::journaled_state::entry::JournalEntry;
    use database_interface::EmptyDB;
    use primitives::{HashSet, U256, address};
    use state::AccountInfo;

    #[test]
    fn test_sload_skip_cold_load() {
        let mut journal = JournalInner::<JournalEntry>::new();
        let test_address = address!("1000000000000000000000000000000000000000");
        let test_key = U256::from(1);

        // Insert account into state
        let account_info = AccountInfo {
            balance: U256::from(1000),
            nonce: 1,
            code_hash: KECCAK_EMPTY,
            code: Some(Bytecode::default()),
            account_id: None,
        };
        journal.state.insert(test_address, Account::from(account_info));

        // Add storage slot to access list (make it warm)
        let mut access_list = HashMap::default();
        let mut storage_keys = HashSet::default();
        storage_keys.insert(test_key);
        access_list.insert(test_address, storage_keys);
        journal.warm_addresses.set_access_list(access_list);

        // Try to sload with skip_cold_load=true - should succeed because slot is in access list
        let mut db = EmptyDB::new();
        let result = journal.sload_assume_account_present(&mut db, test_address, test_key, true);

        // Should succeed and return as warm
        assert!(result.is_ok());
        let state_load = result.unwrap();
        assert!(!state_load.is_cold); // Should be warm
        assert_eq!(state_load.data, U256::ZERO); // Empty slot
    }

    #[test]
    fn test_block_warm_account_rebate_is_2500() {
        let mut journal = JournalInner::<JournalEntry>::new();
        journal.enable_persistent_warming();
        let test_address = address!("2000000000000000000000000000000000000000");

        let account_info = AccountInfo {
            balance: U256::from(1000),
            nonce: 1,
            code_hash: KECCAK_EMPTY,
            code: Some(Bytecode::default()),
            account_id: None,
        };
        journal.state.insert(test_address, Account::from(account_info));

        let mut db = EmptyDB::new();
        let first = journal.load_account_mut_optional(&mut db, test_address, false).unwrap();
        assert!(first.is_cold);
        drop(first);
        journal.commit_tx();

        let second = journal.load_account_mut_optional(&mut db, test_address, false).unwrap();
        assert!(second.is_cold);
        assert_eq!(journal.tx_warming_savings, 2500);
    }

    #[test]
    fn test_block_warm_sload_rebate_is_2000() {
        let mut journal = JournalInner::<JournalEntry>::new();
        journal.enable_persistent_warming();
        let test_address = address!("3000000000000000000000000000000000000000");
        let test_key = U256::from(1);

        let account_info = AccountInfo {
            balance: U256::from(1000),
            nonce: 1,
            code_hash: KECCAK_EMPTY,
            code: Some(Bytecode::default()),
            account_id: None,
        };
        journal.state.insert(test_address, Account::from(account_info));

        let mut db = EmptyDB::new();
        let first =
            journal.sload_assume_account_present(&mut db, test_address, test_key, false).unwrap();
        assert!(first.is_cold);
        journal.commit_tx();

        let second =
            journal.sload_assume_account_present(&mut db, test_address, test_key, false).unwrap();
        assert!(second.is_cold);
        assert_eq!(journal.tx_warming_savings, 2000);
    }

    #[test]
    fn test_block_warm_sstore_rebate_is_2100() {
        let mut journal = JournalInner::<JournalEntry>::new();
        journal.enable_persistent_warming();
        let test_address = address!("4000000000000000000000000000000000000000");
        let test_key = U256::from(1);

        let account_info = AccountInfo {
            balance: U256::from(1000),
            nonce: 1,
            code_hash: KECCAK_EMPTY,
            code: Some(Bytecode::default()),
            account_id: None,
        };
        journal.state.insert(test_address, Account::from(account_info));

        let mut db = EmptyDB::new();
        let first = journal
            .sstore_assume_account_present(&mut db, test_address, test_key, U256::from(1), false)
            .unwrap();
        assert!(first.is_cold);
        journal.commit_tx();

        let second = journal
            .sstore_assume_account_present(&mut db, test_address, test_key, U256::from(2), false)
            .unwrap();
        assert!(second.is_cold);
        assert_eq!(journal.tx_warming_savings, 2100);
    }
}
