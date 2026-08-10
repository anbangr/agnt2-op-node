package conductor

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/wait"
	"github.com/ethereum-optimism/optimism/op-e2e/system/e2esys"
)

// forgedRoot is the value the Byzantine leader is made to commit instead of the real
// typed-op fold. It is deliberately recognisable so it can be searched for on every node.
var forgedRoot = common.HexToHash("0xdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")

// TestSequencerFailover_EquivocationForgedTypedOpRoot makes the op-conductor cluster's
// LEADER Byzantine — it commits a forged typedOpRoot instead of the real fold over the
// block's typed operations — and asserts the cluster does not accept it.
//
// This is WS1 slice 4. It builds directly on the two preceding results: the cluster runs
// AGNT2-active (so header typed-op fields exist at all), and real typed ops are sealed into
// cluster blocks (so there is a non-trivial root to forge — the injection in op-geth's
// FinalizeAndAssemble is gated behind typedCount > 0 and is a no-op without typed traffic).
//
// MECHANISM. debug_setBadRoot arms op-geth's agnt2debug injection for a specific block
// NUMBER. It is consume-once (GetBadRoot deletes on read) and the state is process-global,
// but only the conductor LEADER builds blocks (the other sequencers run SequencerStopped),
// so in practice the arming lands on the leader's build. A range of consecutive heights is
// armed because payload building can iterate, and a non-final build can consume an arming.
//
// The assertions are written to be honest about what is observed rather than assuming a
// particular failure mode: the invariant that must hold is that THE FORGED ROOT NEVER
// BECOMES CANONICAL ON ANY NODE, and that wherever a block does exist, its committed
// typed-op root equals an independent local recomputation of the fold.
func TestSequencerFailover_EquivocationForgedTypedOpRoot(t *testing.T) {
	sys, conductors, cleanup := setupSequencerFailoverTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID)
	l2 := sys.NodeClient(leaderID)
	allNodes := []string{Sequencer1Name, Sequencer2Name, Sequencer3Name}

	chainID := sys.Cfg.L2ChainIDBig()
	signer := types.LatestSignerForChainID(chainID)
	alice := sys.Cfg.Secrets.Alice
	aliceAddr := sys.Cfg.Secrets.Addresses().Alice

	// --- Baseline: an honest typed-op block, so we know the substrate works here. ---
	sendInvoke := func(nonceOffset uint64, wf common.Hash, step uint8) *types.Transaction {
		head, err := l2.HeaderByNumber(ctx, nil)
		require.NoError(t, err)
		tip := big.NewInt(1_000_000_000)
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		nonce, err := l2.PendingNonceAt(ctx, aliceAddr)
		require.NoError(t, err)
		tx, err := types.SignNewTx(alice, signer, &types.InvokeTx{
			ChainID:      chainID,
			Nonce:        nonce + nonceOffset,
			GasTipCap:    tip,
			GasFeeCap:    feeCap,
			Gas:          500_000,
			WorkflowId:   wf,
			StepId:       step,
			AgentRole:    "agnt2-equivocation",
			DepInvokeIds: []common.Hash{},
			Payload:      agnt2SettlePayload("agnt2-equivocation-wf", 0),
		})
		require.NoError(t, err)
		require.NoError(t, l2.SendTransaction(ctx, tx))
		return tx
	}

	baseTx := sendInvoke(0, common.HexToHash("0x3001"), 1)
	baseRcpt, err := wait.ForReceiptOK(ctx, l2, baseTx.Hash())
	require.NoError(t, err)
	baseNum := baseRcpt.BlockNumber.Uint64()
	var baseRaw map[string]any
	require.NoError(t, l2.Client().CallContext(ctx, &baseRaw, "eth_getBlockByNumber", hexUint64(baseNum), false))
	require.NotNil(t, baseRaw["typedOpRoot"], "baseline typed-op block must exist before forging")
	t.Logf("BASELINE: honest typed-op block #%d typedOpRoot=%v", baseNum, baseRaw["typedOpRoot"])

	// Snapshot the AGNT2 typed-op rejection counter BEFORE arming. Without this the test
	// could pass vacuously: "no forged root is canonical" is trivially true if the
	// injection never fired at all. The counter is core.Agnt2InvalidSignatureCount, which
	// only increments inside the AGNT2 typed-op validator.
	invalidCountBefore := agnt2InvalidBlockCount(t, ctx, sys, leaderID)
	t.Logf("BASELINE: engine_invalid_block_count=%d", invalidCountBefore)

	// --- Arm the forgery on the leader for a range of upcoming heights. ---
	head, err := l2.HeaderByNumber(ctx, nil)
	require.NoError(t, err)
	armFrom := head.Number.Uint64() + 1
	armTo := armFrom + 5
	for n := armFrom; n <= armTo; n++ {
		// blockNumber is a plain uint64 on the Go side, so it must go over the wire as a
		// JSON number, not a hex quantity string.
		require.NoError(t, l2.Client().CallContext(ctx, nil, "debug_setBadRoot", n, forgedRoot),
			"debug_setBadRoot must be permitted on this chain (guard allowlist)")
	}
	t.Logf("ARMED: leader %s forged typedOpRoot=%s for heights #%d..#%d", leaderID, forgedRoot.Hex(), armFrom, armTo)

	// Typed traffic at the armed heights — the injection only fires when typedCount > 0.
	for i := uint64(0); i < 4; i++ {
		sendInvoke(0, common.BytesToHash([]byte{0x40, byte(i)}), uint8(i+1))
		time.Sleep(700 * time.Millisecond)
	}

	// Let the cluster react.
	time.Sleep(6 * time.Second)

	// --- Observe. Log everything; assert only the invariant that must hold. ---
	scanTo := armTo + 4
	forgedSeen := 0
	for _, n := range allNodes {
		c := sys.NodeClient(n)
		h, err := c.BlockByNumber(ctx, nil)
		if err != nil {
			t.Logf("OBSERVE %s: head unavailable: %v", n, err)
			continue
		}
		t.Logf("OBSERVE %s: head #%d", n, h.NumberU64())
		for bn := armFrom; bn <= scanTo; bn++ {
			var raw map[string]any
			if err := c.Client().CallContext(ctx, &raw, "eth_getBlockByNumber", hexUint64(bn), false); err != nil || raw == nil {
				continue
			}
			tor, _ := raw["typedOpRoot"].(string)
			if tor == "" {
				continue
			}
			if common.HexToHash(tor) == forgedRoot {
				forgedSeen++
				t.Errorf("FORGED ROOT CANONICAL on %s at #%d (%s) — the cluster accepted an equivocating block", n, bn, tor)
				continue
			}
			// Where a typed-op block does exist, its commitment must equal the real fold.
			blk, err := c.BlockByNumber(ctx, new(big.Int).SetUint64(bn))
			if err != nil {
				continue
			}
			wantRoot, wantCount := types.FoldTypedOpRoot(blk.Transactions())
			require.Equal(t, wantRoot.Hex(), common.HexToHash(tor).Hex(),
				"%s block #%d committed a typed-op root that is not the fold of its own txs", n, bn)
			t.Logf("  %s #%d typedOpRoot=%s (count=%d) == local fold ✓", n, bn, tor, wantCount)
		}
	}

	// (A) NON-VACUITY: the forgery must actually have been attempted AND caught. Without
	//     this, "no forged root is canonical" would be trivially true if the injection
	//     never fired. Agnt2InvalidSignatureCount increments ONLY inside the AGNT2
	//     typed-op validator, so a strict increase is proof the forged block was built,
	//     validated, and REJECTED.
	invalidCountAfter := agnt2InvalidBlockCount(t, ctx, sys, leaderID)
	require.Greater(t, invalidCountAfter, invalidCountBefore,
		"engine_invalid_block_count must increase — otherwise the forgery never fired and this test is vacuous (before=%d after=%d)",
		invalidCountBefore, invalidCountAfter)
	t.Logf("DETECTED: engine_invalid_block_count %d -> %d (the forged block was built and rejected by the AGNT2 typed-op validator)",
		invalidCountBefore, invalidCountAfter)

	// (B) THE SAFETY INVARIANT: a forged typed-op root must never be canonical anywhere.
	require.Zero(t, forgedSeen, "the forged typedOpRoot must not be canonical on any node")
	t.Logf("NO-FORGED-ROOT-CANONICAL: forged root absent from all %d nodes across #%d..#%d", len(allNodes), armFrom, scanTo)

	// (C) LIVENESS — OBSERVED, NOT ASSERTED. Safety (A + B) is the claim of this test and
	//     holds robustly. Liveness under a leader that forges REPEATEDLY does not: the
	//     rejected block leaves the leader unable to advance, op-conductor marks it
	//     unhealthy and transfers leadership, and the cluster can churn for a long time.
	//     A run with a wide armed range was observed thrashing (raft reaching term 51 with
	//     "healthy: false") and NOT advancing past the armed range within ~4 minutes.
	//     Asserting recovery here would make the test flaky AND would overclaim a liveness
	//     property the system does not reliably provide under sustained forging. So this is
	//     recorded as an observation; the honest finding is written up in the evidence.
	livenessCtx, livenessCancel := context.WithTimeout(ctx, 45*time.Second)
	defer livenessCancel()
	recovered := wait.For(livenessCtx, 1*time.Second, func() (bool, error) {
		h, err := sys.NodeClient(leaderID).BlockByNumber(livenessCtx, nil)
		if err != nil {
			return false, nil
		}
		return h.NumberU64() > armTo, nil
	}) == nil
	finalHead := "unavailable"
	if h, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil); err == nil {
		finalHead = h.Number().String()
	}
	t.Logf("LIVENESS-OBSERVED: recovered_past_armed_range=%v (armedTo=#%d, head=%s) — observation only, not asserted",
		recovered, armTo, finalHead)
}

// agnt2InvalidBlockCount reads core.Agnt2InvalidSignatureCount via admin_metrics. NOTE:
// that counter is a process-global atomic and op-e2e runs every node in ONE process, so it
// CANNOT be attributed to a specific node — it is used here only as a coarse "a rejection
// happened at all" signal, which is exactly what the non-vacuity check needs.
func agnt2InvalidBlockCount(t *testing.T, ctx context.Context, sys *e2esys.System, node string) uint64 {
	var m map[string]any
	require.NoError(t, sys.NodeClient(node).Client().CallContext(ctx, &m, "admin_metrics"))
	v, ok := m["engine_invalid_block_count"]
	require.True(t, ok, "admin_metrics must expose engine_invalid_block_count (got %v)", m)
	switch n := v.(type) {
	case float64:
		return uint64(n)
	case string:
		return uint64(common.HexToHash(n).Big().Uint64())
	default:
		t.Fatalf("unexpected engine_invalid_block_count type %T (%v)", v, v)
		return 0
	}
}
