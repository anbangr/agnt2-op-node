package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"

	"github.com/ethereum-optimism/optimism/op-chain-ops/pkg/sdmreplay"
	"github.com/ethereum-optimism/optimism/op-service/bigs"
	"github.com/ethereum-optimism/optimism/op-service/superutil"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	gstate "github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

type dumpAccount struct {
	Balance hexutil.Big                 `json:"balance"`
	Nonce   uint64                      `json:"nonce"`
	Code    hexutil.Bytes               `json:"code,omitempty"`
	Storage map[common.Hash]common.Hash `json:"storage,omitempty"`
}

type rpcReceipt struct {
	BlockNumber      hexutil.Uint64  `json:"blockNumber"`
	TransactionIndex hexutil.Uint64  `json:"transactionIndex"`
	GasUsed          hexutil.Uint64  `json:"gasUsed"`
	Status           hexutil.Uint64  `json:"status"`
	OPGasRefund      *hexutil.Uint64 `json:"opGasRefund"`
}

type slotKey struct {
	Address common.Address
	Slot    common.Hash
}

type storageOp struct {
	Index       int
	PC          uint64
	Depth       int
	Gas         uint64
	GasCost     uint64
	Contract    common.Address
	Caller      common.Address
	Op          string
	Slot        common.Hash
	ReadValue   common.Hash
	PrevValue   common.Hash
	NewValue    common.Hash
	HasNewValue bool
}

type storageTracer struct {
	env *tracing.VMContext
	ops []storageOp
}

func (t *storageTracer) Hooks() *tracing.Hooks {
	return &tracing.Hooks{
		OnTxStart: t.OnTxStart,
		OnOpcode:  t.OnOpcode,
	}
}

func (t *storageTracer) OnTxStart(env *tracing.VMContext, _ *types.Transaction, _ common.Address) {
	t.env = env
}

func (t *storageTracer) OnOpcode(pc uint64, opcode byte, gas, cost uint64, scope tracing.OpContext, _ []byte, depth int, err error) {
	if err != nil || t.env == nil {
		return
	}
	stack := scope.StackData()
	if len(stack) == 0 {
		return
	}
	op := vm.OpCode(opcode)
	contractAddr := scope.Address()
	caller := scope.Caller()
	slot := common.Hash(stack[len(stack)-1].Bytes32())

	rec := storageOp{
		Index:    len(t.ops),
		PC:       pc,
		Depth:    depth,
		Gas:      gas,
		GasCost:  cost,
		Contract: contractAddr,
		Caller:   caller,
		Slot:     slot,
	}

	switch op {
	case vm.SLOAD:
		rec.Op = "SLOAD"
		rec.ReadValue = t.env.StateDB.GetState(contractAddr, slot)
		t.ops = append(t.ops, rec)
	case vm.SSTORE:
		if len(stack) < 2 {
			return
		}
		rec.Op = "SSTORE"
		rec.PrevValue = t.env.StateDB.GetState(contractAddr, slot)
		rec.NewValue = common.Hash(stack[len(stack)-2].Bytes32())
		rec.HasNewValue = true
		t.ops = append(t.ops, rec)
	}
}

type traceResult struct {
	Ops     []storageOp
	Receipt *types.Receipt
}

type slotOverlap struct {
	Touches int
	TxRefs  map[int]common.Hash
}

type txSummary struct {
	BlockNum          uint64
	BlockHash         common.Hash
	ParentHash        common.Hash
	TxIndex           int
	TxHash            common.Hash
	TxType            uint8
	From              common.Address
	To                *common.Address
	Nonce             uint64
	GasLimit          uint64
	GasUsed           uint64
	Status            uint64
	Value             *big.Int
	InputLen          int
	ReplayRefund      uint64
	ReceiptRefund     uint64
	PayloadRefund     uint64
	EffectiveGas      uint64
	RefundRatio       float64
	ReplayMismatch    bool
	RefundBreakdown   []sdmreplay.ReplaySdmRefundEvent
	BlockGasUsed      uint64
	BlockRefund       uint64
	BlockRefundRatio  float64
	BlockEffectiveGas uint64
	SDMTxPresent      bool
	ReplayMode        string
}

type inspectResult struct {
	Tx              txSummary
	Ops             []storageOp
	PriorOverlaps   map[slotKey]*slotOverlap
	PriorTraceCount int
	PriorTraceSkips []string
	SourceBlockTxs  int
	BlockTxHashes   map[int]common.Hash
}

func main() {
	rpcURL := flag.String("rpc", "http://localhost:8545", "RPC endpoint")
	txHashStr := flag.String("tx-hash", "", "Transaction hash to inspect")
	outPath := flag.String("out", "-", "Output markdown path ('-' for stdout)")
	comparePayload := flag.Bool("compare-payload", false, "Compare replay refunds against embedded SDM payload entries")
	compareRPCReceipts := flag.Bool("compare-rpc-receipts", false, "Compare replay refunds against receipt opGasRefund values")
	tracePriorBlock := flag.Bool("trace-prior-block", true, "Trace earlier transactions in the block to mark overlapping storage slots")
	flag.Parse()

	if *txHashStr == "" {
		fmt.Fprintln(os.Stderr, "Usage: sdm-inspect-tx --rpc URL --tx-hash 0x... [flags]")
		flag.PrintDefaults()
		os.Exit(1)
	}
	if !common.IsHexHash(*txHashStr) {
		fmt.Fprintf(os.Stderr, "Invalid --tx-hash: %s\n", *txHashStr)
		os.Exit(1)
	}

	ctx := context.Background()
	result, err := inspectTransaction(ctx, *rpcURL, common.HexToHash(*txHashStr), *comparePayload, *compareRPCReceipts, *tracePriorBlock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Inspect failed: %v\n", err)
		os.Exit(1)
	}

	report := renderMarkdown(result)
	if *outPath == "-" {
		fmt.Print(report)
		return
	}
	if err := os.WriteFile(*outPath, []byte(report), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "Write report: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Saved report to %s\n", *outPath)
}

func inspectTransaction(ctx context.Context, rpcURL string, txHash common.Hash, comparePayload bool, compareRPCReceipts bool, tracePriorBlock bool) (*inspectResult, error) {
	rpcClient, err := rpc.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial rpc: %w", err)
	}
	defer rpcClient.Close()

	ethCl := ethclient.NewClient(rpcClient)
	defer ethCl.Close()

	replayClient := sdmreplay.NewClient(rpcURL)

	tx, _, err := ethCl.TransactionByHash(ctx, txHash)
	if err != nil {
		return nil, fmt.Errorf("fetch transaction %s: %w", txHash.Hex(), err)
	}
	rpcRec, err := fetchRPCReceipt(ctx, rpcClient, txHash)
	if err != nil {
		return nil, fmt.Errorf("fetch receipt %s: %w", txHash.Hex(), err)
	}
	blockNum := uint64(rpcRec.BlockNumber)
	block, err := ethCl.BlockByNumber(ctx, new(big.Int).SetUint64(blockNum))
	if err != nil {
		return nil, fmt.Errorf("fetch block %d: %w", blockNum, err)
	}
	header := block.Header()
	chainCfg, err := fetchChainConfig(ctx, rpcClient)
	if err != nil {
		return nil, fmt.Errorf("fetch chain config: %w", err)
	}
	signer := types.MakeSigner(chainCfg, header.Number, header.Time)
	from, err := types.Sender(signer, tx)
	if err != nil {
		return nil, fmt.Errorf("derive sender for %s: %w", txHash.Hex(), err)
	}

	replay, err := replayClient.ReplaySdmBlock(ctx, blockNum, comparePayload, compareRPCReceipts)
	if err != nil {
		return nil, fmt.Errorf("replay block %d: %w", blockNum, err)
	}

	txIndex := int(rpcRec.TransactionIndex)
	txReplay := findReplayTx(replay, txHash)
	if txReplay == nil {
		return nil, fmt.Errorf("transaction %s not found in replay tx rows for block %d (it may be an SDM tx)", txHash.Hex(), blockNum)
	}

	trace, err := traceOneTx(ctx, rpcClient, chainCfg, header, tx, from)
	if err != nil {
		return nil, fmt.Errorf("trace target tx %s: %w", txHash.Hex(), err)
	}

	priorOverlaps := map[slotKey]*slotOverlap{}
	priorTraceCount := 0
	var priorTraceSkips []string
	if tracePriorBlock {
		for idx, priorTx := range block.Transactions() {
			if idx >= txIndex {
				break
			}
			if priorTx.Type() == 0x7d {
				continue
			}
			priorFrom, err := types.Sender(signer, priorTx)
			if err != nil {
				priorTraceSkips = append(priorTraceSkips, fmt.Sprintf("tx %d %s sender: %v", idx, priorTx.Hash().Hex(), err))
				continue
			}
			priorTrace, err := traceOneTx(ctx, rpcClient, chainCfg, header, priorTx, priorFrom)
			if err != nil {
				priorTraceSkips = append(priorTraceSkips, fmt.Sprintf("tx %d %s trace: %v", idx, priorTx.Hash().Hex(), err))
				continue
			}
			priorTraceCount++
			seenInTx := make(map[slotKey]bool)
			for _, op := range priorTrace.Ops {
				key := slotKey{Address: op.Contract, Slot: op.Slot}
				info := priorOverlaps[key]
				if info == nil {
					info = &slotOverlap{TxRefs: make(map[int]common.Hash)}
					priorOverlaps[key] = info
				}
				info.Touches++
				if !seenInTx[key] {
					info.TxRefs[idx] = priorTx.Hash()
					seenInTx[key] = true
				}
			}
		}
	}

	receiptRefund := uint64(0)
	if rpcRec.OPGasRefund != nil {
		receiptRefund = uint64(*rpcRec.OPGasRefund)
	}

	refundRatio := 0.0
	if txReplay.GasUsed > 0 {
		refundRatio = float64(txReplay.OPGasRefundReplay) / float64(txReplay.GasUsed)
	}

	blockTxHashes := make(map[int]common.Hash, len(block.Transactions()))
	for idx, blockTx := range block.Transactions() {
		blockTxHashes[idx] = blockTx.Hash()
	}

	result := &inspectResult{
		Tx: txSummary{
			BlockNum:          blockNum,
			BlockHash:         block.Hash(),
			ParentHash:        block.ParentHash(),
			TxIndex:           txIndex,
			TxHash:            txHash,
			TxType:            tx.Type(),
			From:              from,
			To:                tx.To(),
			Nonce:             tx.Nonce(),
			GasLimit:          tx.Gas(),
			GasUsed:           txReplay.GasUsed,
			Status:            uint64(rpcRec.Status),
			Value:             tx.Value(),
			InputLen:          len(tx.Data()),
			ReplayRefund:      txReplay.OPGasRefundReplay,
			ReceiptRefund:     receiptRefund,
			PayloadRefund:     valueOrZero(txReplay.OPGasRefundPayload),
			EffectiveGas:      txReplay.EffectiveGas,
			RefundRatio:       refundRatio,
			ReplayMismatch:    txReplay.Mismatch,
			RefundBreakdown:   txReplay.RefundBreakdown,
			BlockGasUsed:      replay.Summary.BlockGasUsed,
			BlockRefund:       replay.Summary.ReplayRefundTotal,
			BlockRefundRatio:  ratio(replay.Summary.ReplayRefundTotal, replay.Summary.BlockGasUsed),
			BlockEffectiveGas: replay.Summary.BlockEffectiveGas,
			SDMTxPresent:      replay.Summary.SDMTxPresent,
			ReplayMode:        replay.Summary.ReplayMode,
		},
		Ops:             trace.Ops,
		PriorOverlaps:   priorOverlaps,
		PriorTraceCount: priorTraceCount,
		PriorTraceSkips: priorTraceSkips,
		SourceBlockTxs:  len(block.Transactions()),
		BlockTxHashes:   blockTxHashes,
	}
	return result, nil
}

func traceOneTx(ctx context.Context, rpcClient *rpc.Client, chainCfg *params.ChainConfig, header *types.Header, tx *types.Transaction, from common.Address) (*traceResult, error) {
	prestate, err := fetchPrestate(ctx, rpcClient, tx.Hash())
	if err != nil {
		return nil, err
	}
	state, err := loadPrestate(prestate, header, chainCfg)
	if err != nil {
		return nil, err
	}

	rules := chainCfg.Rules(header.Number, true, header.Time)
	precompiles := vm.ActivePrecompiles(rules)
	state.Prepare(rules, from, header.Coinbase, tx.To(), precompiles, tx.AccessList())
	state.SetTxContext(tx.Hash(), 0)

	tracer := &storageTracer{}
	cCtx := &simChainContext{eng: beacon.New(ethash.NewFaker()), head: header, cfg: chainCfg}
	gp := core.GasPool(tx.Gas())
	usedGas := uint64(0)
	blockCtx := core.NewEVMBlockContext(header, cCtx, nil, chainCfg, state)
	evm := vm.NewEVM(blockCtx, state, chainCfg, vm.Config{Tracer: tracer.Hooks()})
	receipt, err := core.ApplyTransaction(evm, &gp, state, header, tx, &usedGas)
	if err != nil {
		return nil, err
	}
	return &traceResult{Ops: tracer.ops, Receipt: receipt}, nil
}

func fetchPrestate(ctx context.Context, rpcClient *rpc.Client, txHash common.Hash) (map[common.Address]dumpAccount, error) {
	var raw json.RawMessage
	traceCfg := map[string]any{"tracer": "prestateTracer"}
	if err := rpcClient.CallContext(ctx, &raw, "debug_traceTransaction", txHash, traceCfg); err != nil {
		return nil, fmt.Errorf("prestate trace %s: %w", txHash.Hex(), err)
	}
	var out map[common.Address]dumpAccount
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode prestate %s: %w", txHash.Hex(), err)
	}
	return out, nil
}

func fetchRPCReceipt(ctx context.Context, rpcClient *rpc.Client, txHash common.Hash) (*rpcReceipt, error) {
	var raw json.RawMessage
	if err := rpcClient.CallContext(ctx, &raw, "eth_getTransactionReceipt", txHash); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("receipt not found")
	}
	var rec rpcReceipt
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func fetchChainConfig(ctx context.Context, rpcClient *rpc.Client) (*params.ChainConfig, error) {
	var idResult hexutil.Big
	if err := rpcClient.CallContext(ctx, &idResult, "eth_chainId"); err != nil {
		return nil, fmt.Errorf("retrieve chain ID: %w", err)
	}
	id := (*big.Int)(&idResult)
	if id.IsUint64() {
		cfg, err := superutil.LoadOPStackChainConfigFromChainID(id.Uint64())
		if err == nil {
			return cfg, nil
		}
	}
	var config params.ChainConfig
	if err := rpcClient.CallContext(ctx, &config, "debug_chainConfig"); err != nil {
		return nil, fmt.Errorf("retrieve chain config: %w", err)
	}
	return &config, nil
}

func loadPrestate(dump map[common.Address]dumpAccount, header *types.Header, chainCfg *params.ChainConfig) (*gstate.StateDB, error) {
	memDB := rawdb.NewMemoryDatabase()
	stateDB := gstate.NewDatabase(triedb.NewDatabase(memDB, nil), nil)
	state, err := gstate.New(types.EmptyRootHash, stateDB)
	if err != nil {
		return nil, fmt.Errorf("create in-memory state: %w", err)
	}
	for addr, acc := range dump {
		state.CreateAccount(addr)
		state.SetBalance(addr, uint256.MustFromBig((*big.Int)(&acc.Balance)), tracing.BalanceChangeUnspecified)
		state.SetNonce(addr, acc.Nonce, tracing.NonceChangeUnspecified)
		state.SetCode(addr, acc.Code, tracing.CodeChangeUnspecified)
		state.SetStorage(addr, acc.Storage)
	}
	_, err = state.Commit(bigs.Uint64Strict(header.Number)-1, true, chainCfg.IsCancun(header.Number, header.Time))
	if err != nil {
		return nil, fmt.Errorf("commit prestate: %w", err)
	}
	return state, nil
}

type simChainContext struct {
	eng  consensus.Engine
	head *types.Header
	cfg  *params.ChainConfig
}

var _ core.ChainContext = (*simChainContext)(nil)

func (d *simChainContext) Engine() consensus.Engine { return d.eng }

func (d *simChainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
	if d.head.Hash() == hash && bigs.Uint64Strict(d.head.Number) == number {
		return d.head
	}
	panic(fmt.Errorf("header retrieval not supported, cannot fetch %s %d", hash, number))
}

func (d *simChainContext) CurrentHeader() *types.Header { return d.head }

func (d *simChainContext) GetHeaderByHash(hash common.Hash) *types.Header {
	if d.head.Hash() == hash {
		return d.head
	}
	panic(fmt.Errorf("header retrieval not supported, cannot fetch %s", hash))
}

func (d *simChainContext) GetHeaderByNumber(number uint64) *types.Header {
	if bigs.Uint64Strict(d.head.Number) == number {
		return d.head
	}
	panic(fmt.Errorf("header retrieval not supported, cannot fetch %d", number))
}

func (d *simChainContext) Config() *params.ChainConfig { return d.cfg }

func findReplayTx(replay *sdmreplay.ReplaySdmBlock, txHash common.Hash) *sdmreplay.ReplaySdmTx {
	for i := range replay.Txs {
		if replay.Txs[i].TxHash == txHash {
			return &replay.Txs[i]
		}
	}
	return nil
}

func valueOrZero(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}

func ratio(numerator uint64, denominator uint64) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func refundKindLabel(kind string) string {
	switch kind {
	case "warm_account":
		return "Warm account"
	case "warm_sload":
		return "Warm SLOAD"
	case "warm_sstore":
		return "Warm SSTORE"
	default:
		return kind
	}
}

func refundBreakdownSummaryRows(events []sdmreplay.ReplaySdmRefundEvent, replayRefund uint64) [][]string {
	byKind := map[string]uint64{}
	total := uint64(0)
	for _, event := range events {
		byKind[event.Kind] += event.Amount
		total += event.Amount
	}
	rows := make([][]string, 0, len(byKind)+2)
	for _, kind := range []string{"warm_account", "warm_sload", "warm_sstore"} {
		if amount := byKind[kind]; amount > 0 {
			rows = append(rows, []string{refundKindLabel(kind), formatUint(amount), fmt.Sprintf("%d event(s)", countRefundKind(events, kind))})
		}
	}
	rows = append(rows,
		[]string{"Breakdown total", formatUint(total), "Must match exact replay refund"},
		[]string{"Exact replay refund", formatUint(replayRefund), "Authoritative value from debug_replaySdmBlock"},
	)
	return rows
}

func countRefundKind(events []sdmreplay.ReplaySdmRefundEvent, kind string) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

func refundBreakdownRows(result *inspectResult) [][]string {
	rows := make([][]string, 0, len(result.Tx.RefundBreakdown))
	for idx, event := range result.Tx.RefundBreakdown {
		firstWarmHash := "-"
		if hash, ok := result.BlockTxHashes[int(event.FirstWarmedByTxIndex)]; ok {
			firstWarmHash = hash.Hex()
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", idx),
			refundKindLabel(event.Kind),
			formatUint(event.Amount),
			event.Address.Hex(),
			optionalHash(event.Slot),
			fmt.Sprintf("%d", event.FirstWarmedByTxIndex),
			firstWarmHash,
		})
	}
	return rows
}

func renderMarkdown(result *inspectResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# SDM Transaction Inspect: %s\n\n", result.Tx.TxHash.Hex())
	b.WriteString("This report combines the block replay refund data with a local per-op storage trace. ")
	b.WriteString("The overlap markers show which storage slots in this tx were already touched earlier in the block. ")
	b.WriteString("SDM refund is not the legacy EVM refund counter: it is a separate block-warming rebate computed by the execution engine.\n\n")

	b.WriteString("## Transaction Summary\n")
	b.WriteString(markdownTable([]string{"Field", "Value"}, [][]string{
		{"Block", fmt.Sprintf("%d", result.Tx.BlockNum)},
		{"Block hash", result.Tx.BlockHash.Hex()},
		{"Parent hash", result.Tx.ParentHash.Hex()},
		{"Tx index", fmt.Sprintf("%d", result.Tx.TxIndex)},
		{"Tx type", fmt.Sprintf("0x%x", result.Tx.TxType)},
		{"From", result.Tx.From.Hex()},
		{"To", optionalAddress(result.Tx.To)},
		{"Nonce", fmt.Sprintf("%d", result.Tx.Nonce)},
		{"Value", result.Tx.Value.String()},
		{"Calldata bytes", fmt.Sprintf("%d", result.Tx.InputLen)},
		{"Gas limit", formatUint(result.Tx.GasLimit)},
		{"Gas used", formatUint(result.Tx.GasUsed)},
		{"Effective gas", formatUint(result.Tx.EffectiveGas)},
		{"Replay refund", formatUint(result.Tx.ReplayRefund)},
		{"Receipt refund", formatUint(result.Tx.ReceiptRefund)},
		{"Payload refund", formatUint(result.Tx.PayloadRefund)},
		{"Refund ratio", formatPct(result.Tx.RefundRatio)},
		{"Status", fmt.Sprintf("%d", result.Tx.Status)},
		{"Replay mismatch", yesNo(result.Tx.ReplayMismatch)},
		{"Replay mode", result.Tx.ReplayMode},
	}))
	b.WriteString("\n\n")

	b.WriteString("## Block Summary\n")
	b.WriteString(markdownTable([]string{"Field", "Value"}, [][]string{
		{"Source block tx count", fmt.Sprintf("%d", result.SourceBlockTxs)},
		{"SDM tx present", yesNo(result.Tx.SDMTxPresent)},
		{"Block gas used", formatUint(result.Tx.BlockGasUsed)},
		{"Block replay refund", formatUint(result.Tx.BlockRefund)},
		{"Block refund ratio", formatPct(result.Tx.BlockRefundRatio)},
		{"Block effective gas", formatUint(result.Tx.BlockEffectiveGas)},
	}))
	b.WriteString("\n\n")

	targetUnique := uniqueSlots(result.Ops)
	overlappingUnique := 0
	for key := range targetUnique {
		if info := result.PriorOverlaps[key]; info != nil {
			overlappingUnique++
		}
	}

	b.WriteString("## How the Replay Refund Is Built\n")
	b.WriteString(markdownTable([]string{"Component", "Value", "Notes"}, refundBreakdownSummaryRows(result.Tx.RefundBreakdown, result.Tx.ReplayRefund)))
	b.WriteString("\n\n")
	b.WriteString("Engine rules used by SDM: warm account = 2500 gas, warm SLOAD = 2000 gas, warm SSTORE = 2100 gas. These rows are emitted directly by the replay engine at the exact increment sites.\n\n")
	b.WriteString("## Exact Refund Attribution Events\n")
	if len(result.Tx.RefundBreakdown) == 0 {
		b.WriteString("No refund attribution events were emitted for this transaction.\n\n")
	} else {
		b.WriteString(markdownTable([]string{"#", "Kind", "Amount", "Address", "Slot", "First Warmed By TxIdx", "First Warmed By TxHash"}, refundBreakdownRows(result)))
		b.WriteString("\n\n")
	}

	b.WriteString("## Prior Block Overlap Summary\n")
	b.WriteString(markdownTable([]string{"Field", "Value"}, [][]string{
		{"Traced earlier block txs", fmt.Sprintf("%d", result.PriorTraceCount)},
		{"Storage ops in target tx", fmt.Sprintf("%d", len(result.Ops))},
		{"Unique storage slots in target tx", fmt.Sprintf("%d", len(targetUnique))},
		{"Unique target slots touched earlier in block", fmt.Sprintf("%d", overlappingUnique)},
	}))
	b.WriteString("\n\n")

	if len(result.PriorTraceSkips) > 0 {
		b.WriteString("### Prior Trace Warnings\n")
		for _, warning := range result.PriorTraceSkips {
			fmt.Fprintf(&b, "- %s\n", warning)
		}
		b.WriteString("\n")
	}

	overlapRows := overlapRows(result)
	b.WriteString("## Overlapping Slots Touched Earlier in the Block\n")
	if len(overlapRows) == 0 {
		b.WriteString("No overlapping storage slots from earlier traced txs were found.\n\n")
	} else {
		b.WriteString(markdownTable([]string{"Contract", "Slot", "Target Ops", "Earlier Touches", "Earlier Txs"}, overlapRows))
		b.WriteString("\n\n")
	}

	b.WriteString("## Full Storage Operations for Target Tx\n")
	if len(result.Ops) == 0 {
		b.WriteString("No SLOAD/SSTORE operations captured.\n")
		return b.String()
	}
	b.WriteString(markdownTable([]string{
		"#", "Depth", "PC", "Op", "Contract", "Slot", "Read/Prev", "New", "GasCost", "Overlap Heuristic Rebate", "Earlier Block Touches", "Earlier Txs",
	}, storageRows(result)))
	b.WriteString("\n")
	return b.String()
}

func overlapRows(result *inspectResult) [][]string {
	type row struct {
		contract string
		slot     string
		target   int
		touches  int
		txs      string
	}
	counts := make(map[slotKey]int)
	for _, op := range result.Ops {
		counts[slotKey{Address: op.Contract, Slot: op.Slot}]++
	}
	rows := make([]row, 0)
	for key, count := range counts {
		info := result.PriorOverlaps[key]
		if info == nil {
			continue
		}
		rows = append(rows, row{
			contract: key.Address.Hex(),
			slot:     key.Slot.Hex(),
			target:   count,
			touches:  info.Touches,
			txs:      formatTxRefs(info.TxRefs),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].touches != rows[j].touches {
			return rows[i].touches > rows[j].touches
		}
		if rows[i].target != rows[j].target {
			return rows[i].target > rows[j].target
		}
		if rows[i].contract != rows[j].contract {
			return rows[i].contract < rows[j].contract
		}
		return rows[i].slot < rows[j].slot
	})
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{r.contract, r.slot, fmt.Sprintf("%d", r.target), fmt.Sprintf("%d", r.touches), r.txs})
	}
	return out
}

func storageRows(result *inspectResult) [][]string {
	rows := make([][]string, 0, len(result.Ops))
	seen := make(map[slotKey]bool)
	for _, op := range result.Ops {
		key := slotKey{Address: op.Contract, Slot: op.Slot}
		info := result.PriorOverlaps[key]
		earlierTouches := "0"
		earlierTxs := "-"
		estimatedRebate := "0"
		if info != nil {
			earlierTouches = fmt.Sprintf("%d", info.Touches)
			earlierTxs = formatTxRefs(info.TxRefs)
			if !seen[key] {
				if op.Op == "SSTORE" {
					estimatedRebate = "2100"
				} else {
					estimatedRebate = "2000"
				}
			}
		}
		seen[key] = true
		readPrev := op.ReadValue.Hex()
		newValue := "-"
		if op.Op == "SSTORE" {
			readPrev = op.PrevValue.Hex()
			newValue = op.NewValue.Hex()
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", op.Index),
			fmt.Sprintf("%d", op.Depth),
			fmt.Sprintf("%d", op.PC),
			op.Op,
			op.Contract.Hex(),
			op.Slot.Hex(),
			readPrev,
			newValue,
			fmt.Sprintf("%d", op.GasCost),
			estimatedRebate,
			earlierTouches,
			earlierTxs,
		})
	}
	return rows
}

func uniqueSlots(ops []storageOp) map[slotKey]struct{} {
	out := make(map[slotKey]struct{})
	for _, op := range ops {
		out[slotKey{Address: op.Contract, Slot: op.Slot}] = struct{}{}
	}
	return out
}

func markdownTable(headers []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| ")
	b.WriteString(strings.Join(headers, " | "))
	b.WriteString(" |\n| ")
	b.WriteString(strings.Join(repeat("---", len(headers)), " | "))
	b.WriteString(" |\n")
	for _, row := range rows {
		b.WriteString("| ")
		escaped := make([]string, len(row))
		for i, cell := range row {
			escaped[i] = strings.ReplaceAll(cell, "|", "\\|")
		}
		b.WriteString(strings.Join(escaped, " | "))
		b.WriteString(" |\n")
	}
	return b.String()
}

func repeat(value string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = value
	}
	return out
}

func formatTxRefs(refs map[int]common.Hash) string {
	indices := make([]int, 0, len(refs))
	for idx := range refs {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	parts := make([]string, 0, len(indices))
	for _, idx := range indices {
		h := refs[idx]
		parts = append(parts, fmt.Sprintf("%d (%s)", idx, shortHash(h)))
	}
	return strings.Join(parts, ", ")
}

func optionalHash(hash *common.Hash) string {
	if hash == nil {
		return "-"
	}
	return hash.Hex()
}

func optionalAddress(addr *common.Address) string {
	if addr == nil {
		return "(contract creation)"
	}
	return addr.Hex()
}

func formatUint(v uint64) string { return fmt.Sprintf("%d", v) }

func formatPct(v float64) string { return fmt.Sprintf("%.2f%%", v*100) }

func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func shortHash(hash common.Hash) string {
	hex := hash.Hex()
	if len(hex) <= 12 {
		return hex
	}
	return hex[:10]
}
