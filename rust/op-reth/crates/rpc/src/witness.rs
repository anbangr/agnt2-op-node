//! Support for optimism specific witness RPCs.

use alloy_consensus::BlockHeader;
use alloy_eips::BlockId;
use alloy_primitives::B256;
use alloy_rpc_types_debug::ExecutionWitness;
use jsonrpsee::proc_macros::rpc;
use jsonrpsee_core::{RpcResult, async_trait};
use reth_chainspec::ChainSpecProvider;
use reth_node_api::{BuildNextEnv, NodePrimitives};
use reth_optimism_evm::ConfigureSdmEvm;
use reth_optimism_forks::OpHardforks;
use reth_optimism_payload_builder::{OpAttributes, OpPayloadBuilder};
use reth_optimism_primitives::{OpBlock, OpPrimitives};
use reth_optimism_sdm_replay::{
    ReplaySdmBlockOptions, ReplaySdmBlockRequest, SdmReplayBlock, SdmReplayConfig, replay_block,
};
use reth_optimism_txpool::OpPooledTx;
use reth_primitives_traits::{SealedHeader, TxTy};
use reth_revm::database::StateProviderDatabase;
pub use reth_rpc_api::DebugExecutionWitnessApiServer;
use reth_rpc_server_types::{ToRpcResult, result::internal_rpc_err};
use reth_storage_api::{
    BlockReaderIdExt, NodePrimitivesProvider, StateProviderFactory, TransactionVariant,
    errors::{ProviderError, ProviderResult},
};
use reth_tasks::TaskSpawner;
use reth_transaction_pool::TransactionPool;
use std::{fmt::Debug, sync::Arc};
use tokio::sync::{Semaphore, oneshot};

/// An extension to the `debug_` namespace for SDM replay.
#[cfg_attr(not(test), rpc(server, namespace = "debug"))]
#[cfg_attr(test, rpc(server, client, namespace = "debug"))]
pub trait OpDebugSdmApi {
    /// Counterfactually replay a historical block with SDM enabled.
    #[method(name = "replaySdmBlock")]
    async fn replay_sdm_block(
        &self,
        block: ReplaySdmBlockRequest,
        options: Option<ReplaySdmBlockOptions>,
    ) -> RpcResult<SdmReplayBlock>;
}

/// An extension to the `debug_` namespace of the RPC API.
pub struct OpDebugWitnessApi<Pool, Provider, EvmConfig, Attrs> {
    inner: Arc<OpDebugWitnessApiInner<Pool, Provider, EvmConfig, Attrs>>,
}

impl<Pool, Provider, EvmConfig, Attrs> OpDebugWitnessApi<Pool, Provider, EvmConfig, Attrs> {
    /// Creates a new instance of the `OpDebugWitnessApi`.
    pub fn new(
        provider: Provider,
        task_spawner: Box<dyn TaskSpawner>,
        builder: OpPayloadBuilder<Pool, Provider, EvmConfig, (), Attrs>,
        evm_config: EvmConfig,
    ) -> Self {
        let semaphore = Arc::new(Semaphore::new(3));
        let inner =
            OpDebugWitnessApiInner { provider, builder, evm_config, task_spawner, semaphore };
        Self { inner: Arc::new(inner) }
    }
}

impl<Pool, Provider, EvmConfig, Attrs> OpDebugWitnessApi<Pool, Provider, EvmConfig, Attrs>
where
    EvmConfig: ConfigureSdmEvm,
    Provider: NodePrimitivesProvider<Primitives = OpPrimitives>
        + BlockReaderIdExt<Block = OpBlock, Header = <OpPrimitives as NodePrimitives>::BlockHeader>,
{
    /// Fetches the parent header by hash.
    fn parent_header(
        &self,
        parent_block_hash: B256,
    ) -> ProviderResult<SealedHeader<Provider::Header>> {
        self.inner
            .provider
            .sealed_header_by_hash(parent_block_hash)?
            .ok_or_else(|| ProviderError::HeaderNotFound(parent_block_hash.into()))
    }

    fn replay_block_by_request(
        &self,
        request: ReplaySdmBlockRequest,
    ) -> ProviderResult<reth_primitives_traits::RecoveredBlock<OpBlock>> {
        match request {
            ReplaySdmBlockRequest::Hash(hash) => self
                .inner
                .provider
                .recovered_block(hash.into(), TransactionVariant::NoHash)?
                .ok_or_else(|| ProviderError::HeaderNotFound(hash.into())),
            ReplaySdmBlockRequest::Number(block) => self
                .inner
                .provider
                .block_with_senders_by_id(BlockId::Number(block), TransactionVariant::NoHash)?
                .ok_or_else(|| ProviderError::HeaderNotFound(0_u64.into())),
        }
    }
}

#[async_trait]
impl<Pool, Provider, EvmConfig, Attrs> DebugExecutionWitnessApiServer<Attrs::RpcPayloadAttributes>
    for OpDebugWitnessApi<Pool, Provider, EvmConfig, Attrs>
where
    Pool: TransactionPool<
            Transaction: OpPooledTx<Consensus = <OpPrimitives as NodePrimitives>::SignedTx>,
        > + 'static,
    Provider: BlockReaderIdExt<Block = OpBlock, Header = <OpPrimitives as NodePrimitives>::BlockHeader>
        + NodePrimitivesProvider<Primitives = OpPrimitives>
        + StateProviderFactory
        + ChainSpecProvider<ChainSpec: OpHardforks>
        + Clone
        + 'static,
    EvmConfig: ConfigureSdmEvm<
            Primitives = OpPrimitives,
            NextBlockEnvCtx: BuildNextEnv<Attrs, Provider::Header, Provider::ChainSpec>,
        > + 'static,
    Attrs: OpAttributes<Transaction = TxTy<EvmConfig::Primitives>>,
{
    async fn execute_payload(
        &self,
        parent_block_hash: B256,
        attributes: Attrs::RpcPayloadAttributes,
    ) -> RpcResult<ExecutionWitness> {
        let _permit = self.inner.semaphore.acquire().await;

        let parent_header = self.parent_header(parent_block_hash).to_rpc_result()?;

        let (tx, rx) = oneshot::channel();
        let this = self.clone();
        self.inner.task_spawner.spawn_blocking_task(Box::pin(async move {
            let res = this.inner.builder.payload_witness(parent_header, attributes);
            let _ = tx.send(res);
        }));

        rx.await
            .map_err(|err| internal_rpc_err(err.to_string()))?
            .map_err(|err| internal_rpc_err(err.to_string()))
    }
}

#[async_trait]
impl<Pool, Provider, EvmConfig, Attrs> OpDebugSdmApiServer
    for OpDebugWitnessApi<Pool, Provider, EvmConfig, Attrs>
where
    Pool: TransactionPool<
            Transaction: OpPooledTx<Consensus = <OpPrimitives as NodePrimitives>::SignedTx>,
        > + 'static,
    Provider: BlockReaderIdExt<Block = OpBlock, Header = <OpPrimitives as NodePrimitives>::BlockHeader>
        + NodePrimitivesProvider<Primitives = OpPrimitives>
        + StateProviderFactory
        + ChainSpecProvider<ChainSpec: OpHardforks>
        + Clone
        + 'static,
    EvmConfig: ConfigureSdmEvm<
            Primitives = OpPrimitives,
            NextBlockEnvCtx: BuildNextEnv<Attrs, Provider::Header, Provider::ChainSpec>,
        > + Clone
        + 'static,
    Attrs: OpAttributes<Transaction = TxTy<EvmConfig::Primitives>>,
{
    async fn replay_sdm_block(
        &self,
        request: ReplaySdmBlockRequest,
        options: Option<ReplaySdmBlockOptions>,
    ) -> RpcResult<SdmReplayBlock> {
        let _permit = self.inner.semaphore.acquire().await;
        let block = self.replay_block_by_request(request).to_rpc_result()?;
        let config = {
            let options = options.unwrap_or_default();
            SdmReplayConfig {
                compare_payload: options.compare_payload,
                compare_receipts: options.compare_receipts,
                ..Default::default()
            }
        };

        let (tx, rx) = oneshot::channel();
        let this = self.clone();
        self.inner.task_spawner.spawn_blocking_task(Box::pin(async move {
            let res = (|| {
                let state_provider = this
                    .inner
                    .provider
                    .state_by_block_hash(block.header().parent_hash())
                    .map_err(|err| internal_rpc_err(err.to_string()))?;
                let db = StateProviderDatabase::new(&state_provider);
                replay_block(&this.inner.evm_config, db, &block, config)
                    .map_err(|err| internal_rpc_err(err.to_string()))
            })();
            let _ = tx.send(res);
        }));

        rx.await.map_err(|err| internal_rpc_err(err.to_string()))?
    }
}

impl<Pool, Provider, EvmConfig, Attrs> Clone
    for OpDebugWitnessApi<Pool, Provider, EvmConfig, Attrs>
{
    fn clone(&self) -> Self {
        Self { inner: Arc::clone(&self.inner) }
    }
}
impl<Pool, Provider, EvmConfig, Attrs> Debug
    for OpDebugWitnessApi<Pool, Provider, EvmConfig, Attrs>
{
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("OpDebugWitnessApi").finish_non_exhaustive()
    }
}

struct OpDebugWitnessApiInner<Pool, Provider, EvmConfig, Attrs> {
    provider: Provider,
    builder: OpPayloadBuilder<Pool, Provider, EvmConfig, (), Attrs>,
    evm_config: EvmConfig,
    task_spawner: Box<dyn TaskSpawner>,
    semaphore: Arc<Semaphore>,
}
