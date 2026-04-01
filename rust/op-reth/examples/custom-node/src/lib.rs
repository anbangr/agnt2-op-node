//! This crate previously hosted a fully custom OP node example.
//!
//! The upstream Optimism/reth APIs it depended on changed significantly with
//! `alloy-evm 0.27.3` and the introduction of post-exec support. Until the
//! example is refreshed against the new APIs, keep the crate compiling by
//! re-exporting the standard OP node type.

#![cfg_attr(not(test), allow(unused_crate_dependencies))]

pub use reth_op::node::OpNode as CustomNode;
