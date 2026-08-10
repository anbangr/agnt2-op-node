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
)

// TestSequencerFailover_EquivocationLivenessBound bounds the liveness NEGATIVE reported by
// TestSequencerFailover_EquivocationForgedTypedOpRoot.
//
// WHY. That test measured, honestly, that a leader forging repeatedly wedges the cluster: it
// recovered past the armed range in only 1 of 3 runs, and a wider armed range thrashed for
// 251 s with raft churn to term 628. Its evidence file records an explicit open question --
// "Whether the cluster recovers over a much longer horizon, or with operator intervention
// (OverrideLeader), is UNMEASURED." Leaving that unmeasured is the weakest part of the claim,
// because a reviewer's first question about a liveness failure is whether an operator can get
// out of it. This test answers exactly that, and answers it either way.
//
// WHAT IS MEASURED. Arm a BOUNDED range of forged heights so the injection is guaranteed to be
// exhausted (it is consume-once per height), let the cluster wedge, then apply the documented
// operator escape hatch -- op-conductor OverrideLeader plus op-node OverrideLeader and
// StartSequencer on a chosen node, the same sequence TestSequencerFailover_DisasterRecovery_
// OverrideLeader uses for quorum loss -- and observe whether the chain advances again.
//
// A SUBTLETY THAT SHAPES THE RESULT: agnt2debug's arming is process-global and keyed by BLOCK
// NUMBER, not by node. So whichever node builds an armed height forges at it, including a node
// the operator just promoted. Recovery therefore cannot mean "the override dodges the fault";
// it can only mean "once the armed heights are consumed, the operator can restart production".
// The bounded range makes that distinction measurable instead of confounding it.
func TestSequencerFailover_EquivocationLivenessBound(t *testing.T) {
	sys, conductors, cleanup := setupSequencerFailoverTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID)
	l2 := sys.NodeClient(leaderID)

	chainID := sys.Cfg.L2ChainIDBig()
	signer := types.LatestSignerForChainID(chainID)
	alice := sys.Cfg.Secrets.Alice
	aliceAddr := sys.Cfg.Secrets.Addresses().Alice

	sendInvoke := func(wf common.Hash, step uint8) {
		head, err := l2.HeaderByNumber(ctx, nil)
		require.NoError(t, err)
		tip := big.NewInt(1_000_000_000)
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		nonce, err := l2.PendingNonceAt(ctx, aliceAddr)
		require.NoError(t, err)
		tx, err := types.SignNewTx(alice, signer, &types.InvokeTx{
			ChainID: chainID, Nonce: nonce, GasTipCap: tip, GasFeeCap: feeCap, Gas: 500_000,
			WorkflowId: wf, StepId: step, AgentRole: "agnt2-liveness",
			DepInvokeIds: []common.Hash{}, Payload: agnt2SettlePayload("agnt2-liveness-wf", 0),
		})
		require.NoError(t, err)
		_ = l2.SendTransaction(ctx, tx) // best-effort: the cluster may already be wedged
	}

	// Establish typed traffic so the forgery hook (gated on typedCount > 0) can fire at all.
	sendInvoke(common.HexToHash("0x7001"), 1)
	time.Sleep(2 * time.Second)

	before := agnt2InvalidBlockCount(t, ctx, sys, leaderID)
	head, err := l2.HeaderByNumber(ctx, nil)
	require.NoError(t, err)
	armFrom := head.Number.Uint64() + 1
	armTo := armFrom + 2 // BOUNDED on purpose: the injection must be exhaustible
	for n := armFrom; n <= armTo; n++ {
		require.NoError(t, l2.Client().CallContext(ctx, nil, "debug_setBadRoot", n, forgedRoot))
	}
	t.Logf("ARMED (bounded): heights #%d..#%d on leader %s", armFrom, armTo, leaderID)

	for i := 0; i < 4; i++ {
		sendInvoke(common.BytesToHash([]byte{0x70, byte(i)}), uint8(i+1))
		time.Sleep(1500 * time.Millisecond)
	}
	time.Sleep(5 * time.Second)

	after := agnt2InvalidBlockCount(t, ctx, sys, leaderID)
	require.Greater(t, after, before,
		"the forgery must actually have fired, else this test measures nothing (before=%d after=%d)", before, after)
	t.Logf("FORGERY FIRED: engine_invalid_block_count %d -> %d", before, after)

	headAtWedge, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	t.Logf("STATE AFTER FORGING: leader head #%d (armed range ended at #%d)", headAtWedge.NumberU64(), armTo)

	// Did it self-recover before any intervention? Record it; do not require it. The budget is
	// a BOUNDED sub-context: polling on the parent ctx would consume the whole test deadline
	// waiting for a recovery that does not come, and leave nothing for the operator phase --
	// which is the part this test exists to measure.
	selfCtx, selfCancel := context.WithTimeout(ctx, 30*time.Second)
	defer selfCancel()
	selfRecovered := wait.For(selfCtx, 1*time.Second, func() (bool, error) {
		h, err := sys.NodeClient(leaderID).BlockByNumber(selfCtx, nil)
		if err != nil {
			return false, nil
		}
		return h.NumberU64() > armTo+1, nil
	}) == nil
	t.Logf("SELF-RECOVERY (no intervention, 30s budget): %v", selfRecovered)

	// --- OPERATOR ESCAPE HATCH ---
	// Promote an honest node with the documented override sequence, then measure whether the
	// chain advances from wherever it is now.
	var target string
	for _, n := range []string{Sequencer1Name, Sequencer2Name, Sequencer3Name} {
		if n != leaderID {
			target = n
			break
		}
	}
	preOverride, err := sys.NodeClient(target).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	t.Logf("OPERATOR: overriding leadership onto %s (its head #%d)", target, preOverride.NumberU64())

	if err := conductors[target].client.OverrideLeader(ctx, true); err != nil {
		t.Logf("  conductor OverrideLeader error (recorded, not fatal): %v", err)
	}
	if err := sys.RollupClient(target).OverrideLeader(ctx); err != nil {
		t.Logf("  op-node OverrideLeader error (recorded, not fatal): %v", err)
	}
	if blk, err := sys.NodeClient(target).BlockByNumber(ctx, nil); err == nil {
		if err := sys.RollupClient(target).StartSequencer(ctx, blk.Hash()); err != nil {
			t.Logf("  StartSequencer error (recorded, not fatal): %v", err)
		}
	}

	recoverCtx, recoverCancel := context.WithTimeout(ctx, 60*time.Second)
	defer recoverCancel()
	recovered := wait.For(recoverCtx, 1*time.Second, func() (bool, error) {
		h, err := sys.NodeClient(target).BlockByNumber(recoverCtx, nil)
		if err != nil {
			return false, nil
		}
		return h.NumberU64() > preOverride.NumberU64(), nil
	}) == nil

	finalHead := "unavailable"
	if h, err := sys.NodeClient(target).BlockByNumber(ctx, nil); err == nil {
		finalHead = h.Number().String()
	}
	t.Logf("OPERATOR-RECOVERY: advanced_after_override=%v (pre=#%d, post=%s, 60s budget)",
		recovered, preOverride.NumberU64(), finalHead)

	// The measurable, non-vacuous result of this test is the FORGERY-FIRED assertion above plus
	// these two recorded outcomes. Recovery is deliberately not asserted: the point is to
	// establish what actually happens, and an assertion either way would prejudge it.
	t.Logf("LIVENESS-BOUND SUMMARY: self_recovered=%v operator_recovered=%v", selfRecovered, recovered)
}
