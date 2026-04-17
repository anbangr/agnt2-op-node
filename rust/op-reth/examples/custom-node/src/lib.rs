//! This example shows how to implement a custom node.
//!
//! A node consists of:
//! - primitives: block,header,transactions
//! - components: network,pool,evm
//! - engine: advances the node
//!
//! The upstream OP payload builder is currently specialized for standard
//! `OpTransactionSigned` payloads. This example still demonstrates a fully custom
//! node type with custom primitives, executor, engine API, validator, and RPC,
//! while wiring a no-op payload service until a dedicated payload builder for the
//! custom transaction type is added.

#![cfg_attr(not(test), warn(unused_crate_dependencies))]

// Required for feature forwarding
use reth_ethereum as _;
use reth_payload_primitives as _;

use crate::{
    engine::{CustomEngineValidatorBuilder, CustomPayloadTypes},
    engine_api::CustomEngineApiBuilder,
    evm::CustomExecutorBuilder,
    pool::CustomPooledTransaction,
    primitives::CustomTransaction,
    rpc::CustomRpcTypes,
};
use chainspec::CustomChainSpec;
use primitives::CustomNodePrimitives;
use reth_node_api::FullNodeTypes;
use reth_node_builder::{
    Node, NodeAdapter, NodeTypes,
    components::{ComponentsBuilder, NodeComponentsBuilder, NoopPayloadServiceBuilder},
    rpc::{BasicEngineValidatorBuilder, RpcAddOns},
};
use reth_op::{
    node::{
        OpNode,
        node::{OpConsensusBuilder, OpNetworkBuilder, OpPoolBuilder},
        txpool,
    },
    rpc::OpEthApiBuilder,
};

pub mod chainspec;
pub mod engine;
pub mod engine_api;
pub mod evm;
pub mod pool;
pub mod primitives;
pub mod rpc;

#[derive(Debug, Clone, Default)]
pub struct CustomNode {
    inner: OpNode,
}

impl NodeTypes for CustomNode {
    type Primitives = CustomNodePrimitives;
    type ChainSpec = CustomChainSpec;
    type Storage = <OpNode as NodeTypes>::Storage;
    type Payload = CustomPayloadTypes;
}

impl<N> Node<N> for CustomNode
where
    N: FullNodeTypes<Types = Self>,
{
    type ComponentsBuilder = ComponentsBuilder<
        N,
        OpPoolBuilder<txpool::OpPooledTransaction<CustomTransaction, CustomPooledTransaction>>,
        NoopPayloadServiceBuilder,
        OpNetworkBuilder,
        CustomExecutorBuilder,
        OpConsensusBuilder,
    >;

    type AddOns = RpcAddOns<
        NodeAdapter<N, <Self::ComponentsBuilder as NodeComponentsBuilder<N>>::Components>,
        OpEthApiBuilder<CustomRpcTypes>,
        CustomEngineValidatorBuilder,
        CustomEngineApiBuilder,
        BasicEngineValidatorBuilder<CustomEngineValidatorBuilder>,
    >;

    fn components_builder(&self) -> Self::ComponentsBuilder {
        let args = &self.inner.args;
        ComponentsBuilder::default()
            .node_types::<N>()
            .pool(
                OpPoolBuilder::default()
                    .with_enable_tx_conditional(args.enable_tx_conditional)
                    .with_supervisor(args.supervisor_http.clone(), args.supervisor_safety_level),
            )
            .executor(CustomExecutorBuilder::default())
            .payload(NoopPayloadServiceBuilder::default())
            .network(OpNetworkBuilder::new(args.disable_txpool_gossip, !args.discovery_v4))
            .consensus(OpConsensusBuilder::default())
    }

    fn add_ons(&self) -> Self::AddOns {
        let args = &self.inner.args;
        RpcAddOns::new(
            OpEthApiBuilder::default()
                .with_sequencer(args.sequencer.clone())
                .with_sequencer_headers(args.sequencer_headers.clone())
                .with_min_suggested_priority_fee(args.min_suggested_priority_fee)
                .with_flashblocks(args.flashblocks_url.clone())
                .with_flashblock_consensus(args.flashblock_consensus),
            CustomEngineValidatorBuilder,
            CustomEngineApiBuilder::default(),
            BasicEngineValidatorBuilder::<CustomEngineValidatorBuilder>::default(),
            Default::default(),
        )
    }
}
