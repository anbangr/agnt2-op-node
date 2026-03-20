package sdm_reth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ethereum-optimism/optimism/op-devstack/devtest"
	"github.com/ethereum-optimism/optimism/op-devstack/dsl"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/txplan"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/lmittmann/w3"
)

// Contract bytecodes compiled from Solidity, embedded as hex strings.
//
// ComputeHeavy: run(uint256 n) loops keccak256 n times (pure computation).
const computeHeavyBin = "6080604052348015600e575f5ffd5b506101908061001c5f395ff3fe608060405234801561000f575f5ffd5b5060043610610029575f3560e01c8063a444f5e91461002d575b5f5ffd5b610047600480360381019061004291906100ec565b610049565b005b5f7f66a80b61b29ec044d14c4c8c613e762ba1fb8eeb0c454d1ee00ed6dedaa5b5c590505f5f90505b828110156100b0578160405160200161008b9190610140565b6040516020818303038152906040528051906020012091508080600101915050610072565b505050565b5f5ffd5b5f819050919050565b6100cb816100b9565b81146100d5575f5ffd5b50565b5f813590506100e6816100c2565b92915050565b5f60208284031215610101576101006100b5565b5b5f61010e848285016100d8565b91505092915050565b5f819050919050565b5f819050919050565b61013a61013582610117565b610120565b82525050565b5f61014b8284610129565b6020820191508190509291505056fea264697066735822122013cd314931f1991e7797e220c9553bb73dfef407d4d266dd8b2553907d5bc14364736f6c634300081c0033"

// StateBloat: run(uint256 n) writes n unique SSTORE slots (state growth).
const stateBloatBin = "6080604052348015600e575f5ffd5b5060f28061001b5f395ff3fe6080604052348015600e575f5ffd5b50600436106026575f3560e01c8063a444f5e914602a575b5f5ffd5b60406004803603810190603c91906096565b6042565b005b5f5f90505b8181101560605760018101815580806001019150506047565b5050565b5f5ffd5b5f819050919050565b6078816068565b81146081575f5ffd5b50565b5f813590506090816071565b92915050565b5f6020828403121560a85760a76064565b5b5f60b3848285016084565b9150509291505056fea2646970667358221220fb9ef6750b6ac6ded2dd901595e50b6daefe24726b41a0346f3a36ac6fcf5f8264736f6c634300081c0033"

var (
	funcRun     = w3.MustNewFunc("run(uint256)", "")
	funcEmitLog = w3.MustNewFunc("emitLog(bytes32[],bytes)", "")
)

type benchRecord struct {
	Type         string   `json:"type"`
	Category     string   `json:"category,omitempty"`
	Block        uint64   `json:"block,omitempty"`
	TxIndex      uint     `json:"tx_index,omitempty"`
	CanonicalGas uint64   `json:"canonical_gas,omitempty"`
	OPGasRefund  uint64   `json:"op_gas_refund,omitempty"`
	EffectiveGas int64    `json:"effective_op_gas,omitempty"`
	RefundRatio  float64  `json:"refund_ratio,omitempty"`
	TxCount      int      `json:"tx_count,omitempty"`
	Categories   []string `json:"categories,omitempty"`
	ELClient     string   `json:"el_client,omitempty"`

	Count         int     `json:"count,omitempty"`
	MeanCanonical float64 `json:"mean_canonical,omitempty"`
	MeanEffective float64 `json:"mean_effective,omitempty"`
	MeanRatio     float64 `json:"mean_ratio,omitempty"`
	P50Ratio      float64 `json:"p50_ratio,omitempty"`
	P95Ratio      float64 `json:"p95_ratio,omitempty"`
	P99Ratio      float64 `json:"p99_ratio,omitempty"`
}

type txResult struct {
	category    string
	blockNum    uint64
	txIndex     uint
	gasUsed     uint64
	opGasRefund uint64
}

// verifyOpReth checks the L2 execution layer client is op-reth by calling
// web3_clientVersion via the L2EthClient's RPC and asserting it contains "reth".
// This is the critical check that ensures we are NOT running on op-geth.
func verifyOpReth(t devtest.T, l2EL *dsl.L2ELNode) string {
	rpcClient := l2EL.Escape().L2EthClient().RPC()
	var clientVersion string
	err := rpcClient.CallContext(context.Background(), &clientVersion, "web3_clientVersion")
	t.Require().NoError(err, "web3_clientVersion RPC failed — cannot verify EL client")

	lower := strings.ToLower(clientVersion)
	t.Require().True(
		strings.Contains(lower, "reth"),
		"FATAL: Expected op-reth execution client, but got: %q. "+
			"This test MUST run on op-reth. "+
			"Set DEVSTACK_L2EL_KIND=op-reth or ensure op-reth binary is available.",
		clientVersion,
	)

	// Extra check: must NOT be op-geth
	t.Require().False(
		strings.Contains(lower, "geth"),
		"FATAL: Detected op-geth (%q) but this test requires op-reth.", clientVersion,
	)

	return clientVersion
}

func TestSDMBenchmarkOpReth(gt *testing.T) {
	t := devtest.SerialT(gt)
	sys := newSDMRethSystem(t)
	l := t.Logger()

	// ========================================================================
	// VERIFY OP-RETH IS THE EXECUTION CLIENT
	// ========================================================================
	clientVersion := verifyOpReth(t, sys.L2EL)
	l.Info("Verified L2 execution client is op-reth", "version", clientVersion)

	// Fund accounts
	alice := sys.FunderL2.NewFundedEOA(eth.OneEther)
	bob := sys.FunderL2.NewFundedEOA(eth.ZeroWei)
	l.Info("Funded accounts", "alice", alice.Address(), "bob", bob.Address())

	// Deploy contracts
	computeHeavyAddr := deployContract(t, alice, computeHeavyBin)
	l.Info("Deployed ComputeHeavy", "address", computeHeavyAddr)

	stateBloatAddr := deployContract(t, alice, stateBloatBin)
	l.Info("Deployed StateBloat", "address", stateBloatAddr)

	eventLoggerAddr := alice.DeployEventLogger()
	l.Info("Deployed EventLogger", "address", eventLoggerAddr)

	// Open output file
	outputPath := os.Getenv("SDM_BENCH_OUTPUT")
	if outputPath == "" {
		outputPath = "/tmp/sdm_reth_bench.jsonl"
	}
	outFile, err := os.Create(outputPath)
	t.Require().NoError(err, "failed to create output file")
	defer outFile.Close()
	enc := json.NewEncoder(outFile)

	categories := []string{
		"eoa_transfer", "compute_heavy", "event_emitter", "state_bloat",
	}
	txCount := 20

	// Write run config — includes el_client to confirm op-reth is being used
	err = enc.Encode(benchRecord{
		Type:       "run_config",
		TxCount:    txCount,
		Categories: categories,
		ELClient:   clientVersion,
	})
	t.Require().NoError(err)

	results := make(map[string][]txResult)

	for _, cat := range categories {
		for i := 0; i < txCount; i++ {
			var ptx *txplan.PlannedTx

			switch cat {
			case "eoa_transfer":
				ptx = alice.Transfer(bob.Address(), eth.OneHundredthEther)
			case "compute_heavy":
				calldata := encodeRun(500)
				ptx = alice.Transact(alice.Plan(), txplan.WithTo(&computeHeavyAddr), txplan.WithData(calldata))
			case "event_emitter":
				calldata := encodeEmitLog(4, 100)
				ptx = alice.Transact(alice.Plan(), txplan.WithTo(&eventLoggerAddr), txplan.WithData(calldata))
			case "state_bloat":
				calldata := encodeRun(50)
				ptx = alice.Transact(alice.Plan(), txplan.WithTo(&stateBloatAddr), txplan.WithData(calldata))
			}

			receipt, err := ptx.Included.Eval(t.Ctx())
			t.Require().NoError(err, "tx %s #%d: receipt not found", cat, i)

			blockNum := receipt.BlockNumber.Uint64()
			txIndex := receipt.TransactionIndex

			// Read OPGasRefund from receipt via raw JSON RPC.
			// The opGasRefund field is only present when SDM is active.
			// When SDM is not yet active, refund will be 0.
			refund := getOPGasRefund(t, sys.L2EL, receipt.TxHash)

			res := txResult{
				category:    cat,
				blockNum:    blockNum,
				txIndex:     txIndex,
				gasUsed:     receipt.GasUsed,
				opGasRefund: refund,
			}
			results[cat] = append(results[cat], res)

			canonicalGas := receipt.GasUsed
			effectiveGas := canonicalGas - refund
			var ratio float64
			if canonicalGas > 0 {
				ratio = float64(refund) / float64(canonicalGas)
			}

			err = enc.Encode(benchRecord{
				Type:         "tx",
				Category:     cat,
				Block:        blockNum,
				TxIndex:      txIndex,
				CanonicalGas: canonicalGas,
				OPGasRefund:  refund,
				EffectiveGas: int64(effectiveGas),
				RefundRatio:  ratio,
			})
			t.Require().NoError(err)

			l.Info("Recorded tx",
				"category", cat, "i", i,
				"canonicalGas", canonicalGas,
				"effectiveGas", effectiveGas,
				"opGasRefund", refund,
				"refundRatio", fmt.Sprintf("%.4f", ratio))
		}
	}

	// Compute and write summaries per category
	for _, cat := range categories {
		txs := results[cat]
		if len(txs) == 0 {
			continue
		}

		var totalGas, totalEffective float64
		ratios := make([]float64, 0, len(txs))

		for _, tx := range txs {
			canonical := float64(tx.gasUsed)
			totalGas += canonical
			totalEffective += float64(tx.gasUsed - tx.opGasRefund)
			var ratio float64
			if canonical > 0 {
				ratio = float64(tx.opGasRefund) / canonical
			}
			ratios = append(ratios, ratio)
		}

		sort.Float64s(ratios)
		n := float64(len(txs))

		summary := benchRecord{
			Type:          "summary",
			Category:      cat,
			Count:         len(txs),
			MeanCanonical: totalGas / n,
			MeanEffective: totalEffective / n,
			MeanRatio:     mean(ratios),
			P50Ratio:      percentile(ratios, 50),
			P95Ratio:      percentile(ratios, 95),
			P99Ratio:      percentile(ratios, 99),
			ELClient:      clientVersion,
		}

		err = enc.Encode(summary)
		t.Require().NoError(err)

		l.Info("Summary",
			"category", cat,
			"count", summary.Count,
			"meanCanonicalGas", summary.MeanCanonical,
			"meanEffectiveGas", summary.MeanEffective,
			"meanRefundRatio", fmt.Sprintf("%.4f", summary.MeanRatio),
			"p50Ratio", fmt.Sprintf("%.4f", summary.P50Ratio),
			"p95Ratio", fmt.Sprintf("%.4f", summary.P95Ratio))
	}

	l.Info("Benchmark complete",
		"output", outputPath,
		"totalTxs", txCount*len(categories),
		"elClient", clientVersion)
}

// getOPGasRefund reads the opGasRefund field from a transaction receipt via
// raw JSON RPC. Returns 0 if the field is not present (SDM inactive).
func getOPGasRefund(t devtest.T, l2EL *dsl.L2ELNode, txHash common.Hash) uint64 {
	rpcClient := l2EL.Escape().L2EthClient().RPC()
	var raw json.RawMessage
	err := rpcClient.CallContext(context.Background(), &raw, "eth_getTransactionReceipt", txHash)
	if err != nil || raw == nil {
		return 0
	}

	var result struct {
		OPGasRefund *hexutil.Uint64 `json:"opGasRefund"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0
	}
	if result.OPGasRefund != nil {
		return uint64(*result.OPGasRefund)
	}
	return 0
}

func deployContract(t devtest.T, eoa *dsl.EOA, hexBytecode string) common.Address {
	tx := txplan.NewPlannedTx(eoa.Plan(), txplan.WithData(common.FromHex(hexBytecode)))
	res, err := tx.Included.Eval(t.Ctx())
	t.Require().NoError(err, "failed to deploy contract")
	return res.ContractAddress
}

func encodeRun(n uint64) []byte {
	data, err := funcRun.EncodeArgs(new(big.Int).SetUint64(n))
	if err != nil {
		panic(fmt.Sprintf("failed to encode run(%d): %v", n, err))
	}
	return data
}

func encodeEmitLog(topicCount int, dataLen int) []byte {
	topics := make([][32]byte, topicCount)
	for i := range topics {
		topics[i] = [32]byte{byte(i + 1)}
	}
	opaqueData := make([]byte, dataLen)
	for i := range opaqueData {
		opaqueData[i] = byte(i % 256)
	}
	data, err := funcEmitLog.EncodeArgs(topics, opaqueData)
	if err != nil {
		panic(fmt.Sprintf("failed to encode emitLog: %v", err))
	}
	return data
}

func mean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := p / 100.0 * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sorted[lower]
	}
	weight := idx - float64(lower)
	return sorted[lower]*(1-weight) + sorted[upper]*weight
}
