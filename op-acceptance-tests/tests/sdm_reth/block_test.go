package sdm_reth

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ethereum-optimism/optimism/op-chain-ops/pkg/sdmreplay"
	"github.com/ethereum-optimism/optimism/op-devstack/devtest"
	"github.com/ethereum-optimism/optimism/op-devstack/dsl"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/txplan"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
)

const sdmTxPosition = 1 // position 1 in the block (after L1 info deposit tx at 0)

// rpcTransaction is a minimal representation of a transaction from eth_getBlockByNumber
// with full transactions. We use raw JSON to avoid depending on op-geth types for deposit
// fields that may differ between op-geth and op-reth RPC responses.
type rpcTransaction struct {
	Hash                common.Hash     `json:"hash"`
	Type                hexutil.Uint64  `json:"type"`
	From                common.Address  `json:"from"`
	To                  *common.Address `json:"to"`
	Input               hexutil.Bytes   `json:"input"`
	Gas                 hexutil.Uint64  `json:"gas"`
	IsSystemTx          *bool           `json:"isSystemTx,omitempty"`          // op-geth style
	IsSystemTransaction *bool           `json:"isSystemTransaction,omitempty"` // op-reth style
	SourceHash          *common.Hash    `json:"sourceHash,omitempty"`
}

func (tx *rpcTransaction) isSystemTransaction() bool {
	if tx.IsSystemTx != nil && *tx.IsSystemTx {
		return true
	}
	if tx.IsSystemTransaction != nil && *tx.IsSystemTransaction {
		return true
	}
	return false
}

// rpcBlock is a minimal representation of a block from eth_getBlockByNumber(n, true).
type rpcBlock struct {
	Number       hexutil.Uint64   `json:"number"`
	Hash         common.Hash      `json:"hash"`
	Transactions []rpcTransaction `json:"transactions"`
}

type replaySdmPayloadEntry struct {
	Index     uint64 `json:"index"`
	GasRefund uint64 `json:"gas_refund"`
}

type replaySdmPayload struct {
	Version uint64                  `json:"version"`
	Entries []replaySdmPayloadEntry `json:"entries"`
}

type replaySdmTx struct {
	TxIndex            uint64      `json:"tx_index"`
	ReplayTxIndex      uint64      `json:"replay_tx_index"`
	TxHash             common.Hash `json:"tx_hash"`
	TxType             uint64      `json:"tx_type"`
	IsDepositTx        bool        `json:"is_deposit_tx"`
	GasUsed            uint64      `json:"gas_used"`
	OPGasRefundReplay  uint64      `json:"op_gas_refund_replay"`
	OPGasRefundPayload *uint64     `json:"op_gas_refund_payload"`
	OPGasRefundReceipt *uint64     `json:"op_gas_refund_receipt"`
	EffectiveGas       uint64      `json:"effective_gas"`
	Mismatch           bool        `json:"mismatch"`
}

type replaySdmMismatch struct {
	Category string  `json:"category"`
	BlockNum uint64  `json:"block_num"`
	TxIndex  *uint64 `json:"tx_index"`
	Expected *uint64 `json:"expected"`
	Actual   *uint64 `json:"actual"`
	Message  string  `json:"message"`
}

type replaySdmSummary struct {
	BlockNum               uint64      `json:"block_num"`
	BlockHash              common.Hash `json:"block_hash"`
	TxCountTotal           int         `json:"tx_count_total"`
	TxCountUser            int         `json:"tx_count_user"`
	SDMTxPresent           bool        `json:"sdm_tx_present"`
	SDMPayloadEntryCount   int         `json:"sdm_payload_entry_count"`
	BlockGasUsed           uint64      `json:"block_gas_used"`
	ReplayRefundTotal      uint64      `json:"replay_refund_total"`
	PayloadRefundTotal     uint64      `json:"payload_refund_total"`
	NodeReceiptRefundTotal uint64      `json:"node_receipt_refund_total"`
	BlockEffectiveGas      uint64      `json:"block_effective_gas"`
	MismatchCount          int         `json:"mismatch_count"`
	ReplayMode             string      `json:"replay_mode"`
}

type replaySdmBlock struct {
	BlockNum                uint64              `json:"block_num"`
	BlockHash               common.Hash         `json:"block_hash"`
	ParentHash              common.Hash         `json:"parent_hash"`
	SDMTxPresent            bool                `json:"sdm_tx_present"`
	SDMTxIndex              *uint64             `json:"sdm_tx_index"`
	SynthesizedPayload      replaySdmPayload    `json:"synthesized_payload"`
	SynthesizedPayloadBytes hexutil.Bytes       `json:"synthesized_payload_bytes"`
	Txs                     []replaySdmTx       `json:"txs"`
	Mismatches              []replaySdmMismatch `json:"mismatches"`
	Summary                 replaySdmSummary    `json:"summary"`
}

// getBlockWithTxs fetches a block by number with full transaction objects via raw JSON RPC.
func getBlockWithTxs(t devtest.T, l2EL *dsl.L2ELNode, blockNum uint64) *rpcBlock {
	rpcClient := l2EL.Escape().L2EthClient().RPC()
	var raw json.RawMessage
	err := rpcClient.CallContext(context.Background(), &raw, "eth_getBlockByNumber",
		fmt.Sprintf("0x%x", blockNum), true)
	t.Require().NoError(err, "eth_getBlockByNumber RPC failed for block %d", blockNum)
	t.Require().NotNil(raw, "block %d not found", blockNum)

	var block rpcBlock
	err = json.Unmarshal(raw, &block)
	t.Require().NoError(err, "failed to unmarshal block %d", blockNum)
	return &block
}

func replayBlockWithSDM(t devtest.T, l2EL *dsl.L2ELNode, blockNum uint64) *replaySdmBlock {
	rpcClient := l2EL.Escape().L2EthClient().RPC()
	var raw json.RawMessage
	err := rpcClient.CallContext(context.Background(), &raw, "debug_replaySdmBlock",
		fmt.Sprintf("0x%x", blockNum),
		map[string]bool{
			"compare_payload":  true,
			"compare_receipts": true,
		},
	)
	t.Require().NoError(err, "debug_replaySdmBlock RPC failed for block %d", blockNum)
	t.Require().NotNil(raw, "replay result for block %d must not be nil", blockNum)

	var replay replaySdmBlock
	err = json.Unmarshal(raw, &replay)
	t.Require().NoError(err, "failed to unmarshal replay result for block %d", blockNum)
	return &replay
}

// findSDMTransaction searches for the SDM tx anywhere in the block.
// Returns the transaction and its position if found, nil/-1 otherwise.
// The SDM tx is identified purely by type 0x7D.
func findSDMTransaction(block *rpcBlock) (*rpcTransaction, int) {
	for i := range block.Transactions {
		tx := &block.Transactions[i]
		if uint64(tx.Type) != 0x7d {
			continue
		}
		return tx, i
	}
	return nil, -1
}

// submitTxWithoutWait sends a transaction to the mempool without waiting for inclusion.
// Returns the PlannedTx whose Included field can be evaluated later.
// The caller must provide a nonce to avoid the default PendingNonce lookup racing between txs.
func submitTxWithoutWait(
	t devtest.T,
	alice *dsl.EOA,
	nonce uint64,
	opts ...txplan.Option,
) *txplan.PlannedTx {
	combined := append([]txplan.Option{
		alice.Plan(),
		txplan.WithNonce(nonce),
	}, opts...)
	ptx := txplan.NewPlannedTx(combined...)
	_, err := ptx.Submitted.Eval(t.Ctx())
	t.Require().NoError(err, "failed to submit tx with nonce %d", nonce)
	return ptx
}

// TestSDMBlockInspection submits multiple transactions rapidly so they land in the same block,
// then inspects the block to check for the SDM transaction and non-zero refunds on later txs
// that touch the same storage slots.
//
// This test is forward-compatible:
// - When SDM is NOT yet active: verifies batching works, logs that no SDM tx is present.
// - When SDM IS active: verifies the SDM tx is present and contains refund entries.
func TestSDMBlockInspection(gt *testing.T) {
	t := devtest.SerialT(gt)
	sys := newSDMRethSystem(t)
	l := t.Logger()

	// Verify we're running op-reth
	clientVersion := verifyOpReth(t, sys.L2EL)
	l.Info("Verified op-reth", "version", clientVersion)

	// Fund alice with enough ETH for several storage-writing txs.
	alice := sys.FunderL2.NewFundedEOA(eth.OneEther)
	l.Info("Funded account", "alice", alice.Address())

	// Deploy a contract whose run(n) call writes to slots [0, n).
	// Repeating the same calldata across txs reuses the same storage slots.
	stateBloatAddr := deployContract(t, alice, stateBloatBin)
	l.Info("Deployed StateBloat", "address", stateBloatAddr)

	// ========================================================================
	// PHASE 1: Submit multiple transactions without waiting for inclusion
	// ========================================================================
	const batchSize = 50
	const slotCount = 20
	startNonce := alice.PendingNonce()
	l.Info("Starting batch submission",
		"startNonce", startNonce,
		"batchSize", batchSize,
		"slotCount", slotCount)

	var plannedTxs []*txplan.PlannedTx

	for i := 0; i < batchSize; i++ {
		nonce := startNonce + uint64(i)
		calldata := encodeRun(slotCount)
		ptx := submitTxWithoutWait(t, alice, nonce,
			txplan.WithTo(addrPtr(stateBloatAddr)),
			txplan.WithData(calldata),
			txplan.WithGasLimit(1_000_000),
		)
		plannedTxs = append(plannedTxs, ptx)
		l.Info("Submitted storage tx", "index", i, "nonce", nonce)
	}

	// ========================================================================
	// PHASE 2: Wait for all transactions to be included and collect receipts
	// ========================================================================
	type includedTx struct {
		receipt  *types.Receipt
		txIndex  int
		blockNum uint64
	}
	included := make([]includedTx, 0, batchSize)

	for i, ptx := range plannedTxs {
		receipt, err := ptx.Included.Eval(t.Ctx())
		t.Require().NoError(err, "tx %d: failed to get receipt", i)
		t.Require().Equal(types.ReceiptStatusSuccessful, receipt.Status, "tx %d: must succeed", i)

		included = append(included, includedTx{
			receipt:  receipt,
			txIndex:  i,
			blockNum: receipt.BlockNumber.Uint64(),
		})
		l.Info("Tx included",
			"index", i,
			"block", receipt.BlockNumber.Uint64(),
			"txIndexInBlock", receipt.TransactionIndex,
			"gasUsed", receipt.GasUsed)
	}

	// ========================================================================
	// PHASE 3: Group transactions by block and find a block with multiple user txs
	// ========================================================================
	blockTxs := make(map[uint64][]includedTx)
	for _, itx := range included {
		blockTxs[itx.blockNum] = append(blockTxs[itx.blockNum], itx)
	}

	l.Info("Transaction distribution across blocks", "numBlocks", len(blockTxs))
	for blockNum, txs := range blockTxs {
		l.Info("Block", "number", blockNum, "userTxCount", len(txs))
	}

	// Find a block with at least 2 user txs (needed for block-level warming to have an effect)
	var targetBlockNum uint64
	var targetUserTxCount int
	for blockNum, txs := range blockTxs {
		if len(txs) > targetUserTxCount {
			targetBlockNum = blockNum
			targetUserTxCount = len(txs)
		}
	}

	if targetUserTxCount < 2 {
		l.Warn("No block contained multiple user txs — block-level warming cannot be measured. " +
			"This can happen if block time is very short relative to tx submission speed.")
		// Don't fail — this is informational. The txs still landed successfully.
		return
	}

	l.Info("Selected target block for SDM inspection",
		"block", targetBlockNum,
		"userTxCount", targetUserTxCount)

	// ========================================================================
	// PHASE 4: Fetch the target block and inspect for SDM transaction
	// ========================================================================
	block := getBlockWithTxs(t, sys.L2EL, targetBlockNum)
	l.Info("Fetched block",
		"number", uint64(block.Number),
		"totalTxCount", len(block.Transactions))

	// Log all transactions in the block for debugging
	for i, tx := range block.Transactions {
		l.Info("Block tx",
			"position", i,
			"type", fmt.Sprintf("0x%x", uint64(tx.Type)),
			"from", tx.From.Hex(),
			"isSystemTx", tx.isSystemTransaction(),
			"inputLen", len(tx.Input))
	}

	// Check position 0 is the L1 info deposit tx (sanity check)
	t.Require().Greater(len(block.Transactions), 0, "block must have at least one transaction")
	l1InfoTx := &block.Transactions[0]
	t.Require().Equal(uint64(types.DepositTxType), uint64(l1InfoTx.Type),
		"position 0 must be a deposit tx (L1 info)")

	// Look for SDM transaction anywhere in the block
	sdmTx, sdmPos := findSDMTransaction(block)
	if sdmTx == nil {
		l.Info("No SDM transaction found in block — SDM fork likely not yet active. "+
			"This is expected when BlockWarming fork is not configured.",
			"block", targetBlockNum)

		// Forward-compatible: when SDM is not active, verify our batching worked
		// and that multiple user txs did land in the same block.
		t.Require().GreaterOrEqual(targetUserTxCount, 2,
			"expected at least 2 user txs in the target block")
		l.Info("Batch submission verified: multiple user txs in same block",
			"block", targetBlockNum,
			"userTxCount", targetUserTxCount)
		return
	}

	// ========================================================================
	// PHASE 5: SDM transaction found — validate its contents
	// ========================================================================
	l.Info("SDM transaction found!",
		"block", targetBlockNum,
		"position", sdmPos,
		"inputLen", len(sdmTx.Input))

	// The SDM tx input contains RLP-encoded SDMPayload.
	t.Require().Greater(len(sdmTx.Input), 0,
		"SDM tx input must not be empty (contains RLP-encoded SDMPayload)")
	t.Require().Equal(uint64(0x7d), uint64(sdmTx.Type),
		"SDM tx type must be 0x7D")

	payload, err := sdmreplay.DecodePayload(sdmTx.Input)
	t.Require().NoError(err, "SDM payload must decode")
	t.Require().Equal(uint64(1), payload.Version, "SDM payload version must be 1")
	l.Info("Decoded SDM payload",
		"block", targetBlockNum,
		"entryCount", len(payload.Entries))
	for _, itx := range blockTxs[targetBlockNum] {
		refund := getOPGasRefund(t, sys.L2EL, itx.receipt.TxHash)
		l.Info("Receipt opGasRefund for tx in target block",
			"userTxIndex", itx.txIndex,
			"positionInBlock", itx.receipt.TransactionIndex,
			"txHash", itx.receipt.TxHash,
			"gasUsed", itx.receipt.GasUsed,
			"opGasRefund", refund)
	}
	t.Require().NotEmpty(payload.Entries, "SDM payload must include refund entries for repeated-slot block")

	for _, entry := range payload.Entries {
		t.Require().Less(int(entry.Index), len(block.Transactions), "payload index must be in block range")
		targetTx := block.Transactions[entry.Index]
		t.Require().NotEqual(uint64(types.DepositTxType), uint64(targetTx.Type), "payload must not target deposits")
		t.Require().NotEqual(uint64(0x7d), uint64(targetTx.Type), "payload must not target the SDM tx itself")

		refund := getOPGasRefund(t, sys.L2EL, targetTx.Hash)
		t.Require().Equal(entry.GasRefund, refund,
			"payload refund must match receipt opGasRefund for tx index %d", entry.Index)
	}

	replay := replayBlockWithSDM(t, sys.L2EL, targetBlockNum)
	t.Require().Equal(targetBlockNum, replay.BlockNum, "replay must target the selected block")
	t.Require().Equal(block.Hash, replay.BlockHash, "replay block hash must match source block")
	t.Require().True(replay.SDMTxPresent, "replay response must report the SDM tx in the source block")
	t.Require().NotNil(replay.SDMTxIndex, "replay response must report the SDM tx index")
	t.Require().Equal(uint64(sdmPos), *replay.SDMTxIndex, "replay SDM tx index must match source block")
	t.Require().Equal(len(block.Transactions)-1, len(replay.Txs),
		"replay must strip the SDM tx and preserve all original txs")

	expectedOriginalIndexes := make([]uint64, 0, len(block.Transactions)-1)
	for i := range block.Transactions {
		if i == sdmPos {
			continue
		}
		expectedOriginalIndexes = append(expectedOriginalIndexes, uint64(i))
	}

	replayRefundByIndex := make(map[uint64]uint64, len(replay.Txs))
	hasReplayRefund := false
	for i, tx := range replay.Txs {
		t.Require().Equal(uint64(i), tx.ReplayTxIndex, "replay tx indexes must be sequential")
		t.Require().Equal(expectedOriginalIndexes[i], tx.TxIndex,
			"replay tx %d must preserve original block index", i)

		sourceTx := block.Transactions[tx.TxIndex]
		t.Require().Equal(sourceTx.Hash, tx.TxHash, "replay tx hash must match source tx at index %d", tx.TxIndex)
		t.Require().Equal(uint64(sourceTx.Type), tx.TxType, "replay tx type must match source tx at index %d", tx.TxIndex)
		t.Require().Equal(uint64(types.DepositTxType) == uint64(sourceTx.Type), tx.IsDepositTx,
			"deposit classification must match source tx at index %d", tx.TxIndex)
		t.Require().Equal(tx.GasUsed-tx.OPGasRefundReplay, tx.EffectiveGas,
			"effective gas must match replay accounting at tx index %d", tx.TxIndex)

		if tx.OPGasRefundReplay > 0 {
			hasReplayRefund = true
		}
		replayRefundByIndex[tx.TxIndex] = tx.OPGasRefundReplay

		if tx.OPGasRefundPayload != nil {
			t.Require().Equal(*tx.OPGasRefundPayload, tx.OPGasRefundReplay,
				"payload refund must match replay refund at tx index %d", tx.TxIndex)
		}
		if tx.OPGasRefundReceipt != nil {
			t.Require().Equal(*tx.OPGasRefundReceipt, tx.OPGasRefundReplay,
				"receipt refund must match replay refund at tx index %d", tx.TxIndex)
		}
	}

	t.Require().True(hasReplayRefund, "replay must produce non-zero refunds for the repeated-slot block")
	t.Require().Empty(replay.Mismatches, "SDM-active devnet block should replay without mismatches")
	t.Require().Equal(len(replay.SynthesizedPayload.Entries), replay.Summary.SDMPayloadEntryCount,
		"summary payload entry count must match synthesized payload")

	var totalReplayRefund uint64
	for _, entry := range replay.SynthesizedPayload.Entries {
		sourceTx := block.Transactions[entry.Index]
		refund := getOPGasRefund(t, sys.L2EL, sourceTx.Hash)
		t.Require().Equal(refund, entry.GasRefund,
			"synthesized payload refund must match receipt opGasRefund for tx index %d", entry.Index)
		t.Require().Equal(entry.GasRefund, replayRefundByIndex[entry.Index],
			"synthesized payload refund must match replay tx refund for tx index %d", entry.Index)
		totalReplayRefund += entry.GasRefund
	}

	t.Require().Equal(totalReplayRefund, replay.Summary.ReplayRefundTotal,
		"summary replay refund total must match synthesized payload")
	t.Require().Equal(totalReplayRefund, replay.Summary.PayloadRefundTotal,
		"summary payload refund total must match synthesized payload")
	t.Require().Equal(totalReplayRefund, replay.Summary.NodeReceiptRefundTotal,
		"summary receipt refund total must match synthesized payload")

	// Check opGasRefund on receipts for txs in this block
	hasNonZeroRefund := false
	for _, itx := range blockTxs[targetBlockNum] {
		refund := getOPGasRefund(t, sys.L2EL, itx.receipt.TxHash)
		l.Info("SDM refund for tx in block",
			"userTxIndex", itx.txIndex,
			"positionInBlock", itx.receipt.TransactionIndex,
			"gasUsed", itx.receipt.GasUsed,
			"opGasRefund", refund)
		if refund > 0 {
			hasNonZeroRefund = true
		}
	}

	if hasNonZeroRefund {
		l.Info("Block-level warming is producing non-zero refunds on repeated same-slot txs!",
			"block", targetBlockNum)
	} else {
		l.Crit("SDM tx present in block but all repeated same-slot tx refunds were 0", "block", targetBlockNum)
	}
}

// TestSDMMultiCategoryBatch submits transactions from multiple categories in a single burst,
// without calling .Eval() between submissions. This tests that different tx types
// (transfer, compute, events, state writes) can be batched into the same block.
func TestSDMMultiCategoryBatch(gt *testing.T) {
	t := devtest.SerialT(gt)
	sys := newSDMRethSystem(t)
	l := t.Logger()

	clientVersion := verifyOpReth(t, sys.L2EL)
	l.Info("Verified op-reth", "version", clientVersion)

	// Fund alice
	alice := sys.FunderL2.NewFundedEOA(eth.OneEther)
	bob := sys.FunderL2.NewFundedEOA(eth.ZeroWei)

	// Deploy contracts
	computeHeavyAddr := deployContract(t, alice, computeHeavyBin)
	stateBloatAddr := deployContract(t, alice, stateBloatBin)
	eventLoggerAddr := alice.DeployEventLogger()
	l.Info("Deployed contracts",
		"computeHeavy", computeHeavyAddr,
		"stateBloat", stateBloatAddr,
		"eventLogger", eventLoggerAddr)

	// Submit a diverse batch of transactions without waiting between them
	startNonce := alice.PendingNonce()
	type batchEntry struct {
		category string
		ptx      *txplan.PlannedTx
	}
	var batch []batchEntry

	categories := []struct {
		name string
		opts func(nonce uint64) []txplan.Option
	}{
		{
			name: "eoa_transfer",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(bob.Address())),
					txplan.WithValue(eth.OneHundredthEther),
				}
			},
		},
		{
			name: "compute_heavy",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(computeHeavyAddr)),
					txplan.WithData(encodeRun(200)),
					txplan.WithGasLimit(200_000),
				}
			},
		},
		{
			name: "event_emitter",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(eventLoggerAddr)),
					txplan.WithData(encodeEmitLog(3, 64)),
					txplan.WithGasLimit(200_000),
				}
			},
		},
		{
			name: "state_bloat",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(stateBloatAddr)),
					txplan.WithData(encodeRun(20)),
					txplan.WithGasLimit(500_000),
				}
			},
		},
		// Second round of same categories to trigger cross-tx warming
		{
			name: "compute_heavy_2",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(computeHeavyAddr)),
					txplan.WithData(encodeRun(200)),
					txplan.WithGasLimit(200_000),
				}
			},
		},
		{
			name: "event_emitter_2",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(eventLoggerAddr)),
					txplan.WithData(encodeEmitLog(3, 64)),
					txplan.WithGasLimit(200_000),
				}
			},
		},
		{
			name: "state_bloat_2",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(stateBloatAddr)),
					txplan.WithData(encodeRun(20)),
					txplan.WithGasLimit(500_000),
				}
			},
		},
		{
			name: "eoa_transfer_2",
			opts: func(nonce uint64) []txplan.Option {
				return []txplan.Option{
					txplan.WithTo(addrPtr(bob.Address())),
					txplan.WithValue(eth.OneHundredthEther),
				}
			},
		},
	}

	l.Info("Submitting batch", "txCount", len(categories), "startNonce", startNonce)

	for i, cat := range categories {
		nonce := startNonce + uint64(i)
		ptx := submitTxWithoutWait(t, alice, nonce, cat.opts(nonce)...)
		batch = append(batch, batchEntry{category: cat.name, ptx: ptx})
		l.Info("Submitted", "category", cat.name, "nonce", nonce)
	}

	// Wait for all to be included
	blockCounts := make(map[uint64]int)
	for i, entry := range batch {
		receipt, err := entry.ptx.Included.Eval(t.Ctx())
		t.Require().NoError(err, "tx %d (%s): failed to get receipt", i, entry.category)
		t.Require().Equal(types.ReceiptStatusSuccessful, receipt.Status,
			"tx %d (%s): must succeed", i, entry.category)

		blockNum := receipt.BlockNumber.Uint64()
		blockCounts[blockNum]++

		refund := getOPGasRefund(t, sys.L2EL, receipt.TxHash)
		l.Info("Included",
			"category", entry.category,
			"block", blockNum,
			"txIdx", receipt.TransactionIndex,
			"gasUsed", receipt.GasUsed,
			"opGasRefund", refund)
	}

	// Report distribution
	l.Info("Batch distribution", "numBlocks", len(blockCounts))
	maxInBlock := 0
	var maxBlockNum uint64
	for blockNum, count := range blockCounts {
		l.Info("Block", "number", blockNum, "txCount", count)
		if count > maxInBlock {
			maxInBlock = count
			maxBlockNum = blockNum
		}
	}

	if maxInBlock >= 2 {
		l.Info("Multi-tx block found — inspecting for SDM tx",
			"block", maxBlockNum, "txCount", maxInBlock)

		block := getBlockWithTxs(t, sys.L2EL, maxBlockNum)
		sdmTx, sdmPos := findSDMTransaction(block)
		if sdmTx != nil {
			l.Info("SDM transaction present in multi-category block!",
				"block", maxBlockNum,
				"position", sdmPos,
				"inputLen", len(sdmTx.Input))
		} else {
			l.Info("No SDM tx in block (fork not active yet)",
				"block", maxBlockNum)
		}
	} else {
		l.Warn("All txs landed in separate blocks — no cross-tx warming possible")
	}
}

// addrPtr returns a pointer to the given address (helper for txplan.WithTo).
func addrPtr(addr common.Address) *common.Address {
	return &addr
}
