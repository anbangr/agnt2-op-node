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

// TestSequencerFailover_EquivocationBadDependencyOrder is the second Byzantine class on the
// cluster, complementing the forged-root test: instead of lying about the typed-op ROOT, the
// leader emits typed operations in an order that VIOLATES A DECLARED DEPENDENCY.
//
// WHY IT IS A DISTINCT CLASS. The forged-root fault is caught by re-deriving the MMR fold and
// comparing hashes. Dependency order is a different validator (a topological check over the
// declared edges: a RESPOND's InvokeRef, an INVOKE's DepInvokeIds) and a different rejection
// path, with its own error string and its own increment site on the rejection counter. AGNT2's
// central claim is that dependency order is part of block VALIDITY rather than a scheduling
// hint, so this is the fault that most directly exercises that claim.
//
// MECHANISM. op-geth's miner topologically sorts the block's typed transactions and then, if
// debug_setBadOrder is armed for that height, swaps the two transactions at the given indices
// before committing them. Swapping indices 0 and 1 puts the RESPOND ahead of the INVOKE it
// names, which is exactly the violation validateAGNT2TypedOpOrder is meant to catch. The
// injection self-retries: if the indices are out of range for that block (fewer typed txs than
// expected) it re-arms itself for the next height, so a range of heights need not be armed.
func TestSequencerFailover_EquivocationBadDependencyOrder(t *testing.T) {
	sys, conductors, cleanup := setupSequencerFailoverTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID)
	l2 := sys.NodeClient(leaderID)

	chainID := sys.Cfg.L2ChainIDBig()
	signer := types.LatestSignerForChainID(chainID)
	alice, bob := sys.Cfg.Secrets.Alice, sys.Cfg.Secrets.Bob
	aliceAddr, bobAddr := sys.Cfg.Secrets.Addresses().Alice, sys.Cfg.Secrets.Addresses().Bob

	// Submit a dependent PAIR: RESPOND names the INVOKE via InvokeRef, which is the declared
	// edge the order validator enforces. Both must be in the same block for the swap to
	// produce an in-block violation (the validator skips dependencies not present in the
	// block, so a dangling reference is legal and would make this test vacuous).
	sendPair := func(wf common.Hash, step uint8) (common.Hash, common.Hash) {
		head, err := l2.HeaderByNumber(ctx, nil)
		require.NoError(t, err)
		tip := big.NewInt(1_000_000_000)
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		an, err := l2.PendingNonceAt(ctx, aliceAddr)
		require.NoError(t, err)
		bn, err := l2.PendingNonceAt(ctx, bobAddr)
		require.NoError(t, err)

		invoke, err := types.SignNewTx(alice, signer, &types.InvokeTx{
			ChainID: chainID, Nonce: an, GasTipCap: tip, GasFeeCap: feeCap, Gas: 500_000,
			WorkflowId: wf, StepId: step, AgentRole: "agnt2-badorder",
			DepInvokeIds: []common.Hash{}, Payload: agnt2SettlePayload("agnt2-badorder-wf", 0),
		})
		require.NoError(t, err)
		respond, err := types.SignNewTx(bob, signer, &types.RespondTx{
			ChainID: chainID, Nonce: bn, GasTipCap: tip, GasFeeCap: feeCap, Gas: 500_000,
			WorkflowId: wf, StepId: step + 1, InvokeRef: invoke.Hash(),
			ResponsePayload: agnt2SettlePayload("agnt2-badorder-wf", 0), Status: 0,
		})
		require.NoError(t, err)
		require.NoError(t, l2.SendTransaction(ctx, invoke))
		require.NoError(t, l2.SendTransaction(ctx, respond))
		return invoke.Hash(), respond.Hash()
	}

	// Baseline: an honest dependent pair lands, establishing that the substrate works here and
	// that any later rejection is caused by the injection rather than by malformed traffic.
	inv0, _ := sendPair(common.HexToHash("0x5001"), 1)
	baseRcpt, err := wait.ForReceiptOK(ctx, l2, inv0)
	require.NoError(t, err)
	t.Logf("BASELINE: honest dependent pair mined in block #%d", baseRcpt.BlockNumber.Uint64())

	before := agnt2InvalidBlockCount(t, ctx, sys, leaderID)
	t.Logf("BASELINE: engine_invalid_block_count=%d", before)

	// Arm the dependency-order violation: swap the first two typed transactions, which inverts
	// the INVOKE -> RESPOND order the fold was built from.
	head, err := l2.HeaderByNumber(ctx, nil)
	require.NoError(t, err)
	armAt := head.Number.Uint64() + 1
	armTo := armAt + 7
	// Arm a RANGE, not a single height. The swap is consume-once per height and only
	// produces a violation if that particular block happens to hold a DEPENDENT pair: a
	// block carrying two unrelated typed ops is swapped harmlessly and silently consumes
	// the arming (this is exactly what a single-height arming hit on the first attempt).
	for n := armAt; n <= armTo; n++ {
		require.NoError(t, l2.Client().CallContext(ctx, nil, "debug_setBadOrder", n, []int{0, 1}),
			"debug_setBadOrder must be permitted on this chain (guard allowlist)")
	}
	t.Logf("ARMED: leader %s dependency-order swap [0,1] for heights #%d..#%d", leaderID, armAt, armTo)

	// Feed EXACTLY ONE dependent pair per block. This pacing is load-bearing, not incidental:
	// the miner topologically sorts typed ops, so all INVOKEs (in-degree 0) come first and the
	// RESPONDs follow. With two pairs in one block the sorted order is
	// [INVOKE_a, INVOKE_b, RESPOND_a, RESPOND_b] and swapping indices 0 and 1 exchanges two
	// INDEPENDENT invokes --- a harmless permutation that silently consumes the arming and
	// produces no violation. (Measured: an earlier version submitted pairs every 500 ms, put
	// typedOpCount=4 in every armed block, and detected nothing.) With one pair per block the
	// sorted order is [INVOKE, RESPOND] and the same swap inverts a real declared edge.
	// The L2 block time is 1 s in this harness, so 2.5 s spacing reliably isolates pairs.
	for i := 0; i < 8; i++ {
		sendPair(common.BytesToHash([]byte{0x60, byte(i)}), uint8(2*i+1))
		time.Sleep(2500 * time.Millisecond)
	}
	time.Sleep(6 * time.Second)

	// Diagnostic: how many typed ops actually landed at each armed height? The swap only
	// creates a violation on a block holding a DEPENDENT PAIR, so if these counts are mostly
	// 0 or 1 the injection had nothing to invert and any "pass" would be meaningless.
	for n := armAt; n <= armTo+3; n++ {
		var raw map[string]any
		if err := l2.Client().CallContext(ctx, &raw, "eth_getBlockByNumber", hexUint64(n), false); err != nil || raw == nil {
			continue
		}
		t.Logf("  DIAG height #%d typedOpCount=%v typedOpRoot=%v", n, raw["typedOpCount"], raw["typedOpRoot"])
	}

	// (A) NON-VACUITY. The rejection counter must increase. Because ONLY debug_setBadOrder was
	//     armed here --- no forged root --- an increase is attributable to the dependency-order
	//     validator, which is the other of the counter's two increment sites.
	after := agnt2InvalidBlockCount(t, ctx, sys, leaderID)
	require.Greater(t, after, before,
		"engine_invalid_block_count must increase; otherwise the dependency-order injection never fired and this test is vacuous (before=%d after=%d)",
		before, after)
	t.Logf("DETECTED: engine_invalid_block_count %d -> %d (dependency-order violation built and rejected)", before, after)

	// (B) SAFETY. Every typed-op block that IS canonical must still commit a root equal to the
	//     fold over its own transactions --- i.e. no out-of-order block was accepted.
	checked := 0
	for _, n := range []string{Sequencer1Name, Sequencer2Name, Sequencer3Name} {
		c := sys.NodeClient(n)
		h, err := c.BlockByNumber(ctx, nil)
		if err != nil {
			continue
		}
		for bn := armAt; bn <= h.NumberU64(); bn++ {
			blk, err := c.BlockByNumber(ctx, new(big.Int).SetUint64(bn))
			if err != nil || blk == nil {
				continue
			}
			hdr, err := c.HeaderByNumber(ctx, new(big.Int).SetUint64(bn))
			if err != nil || hdr.TypedOpRoot == nil {
				continue
			}
			wantRoot, _ := types.FoldTypedOpRoot(blk.Transactions())
			require.Equal(t, wantRoot, *hdr.TypedOpRoot,
				"%s block #%d committed a typed-op root that is not the fold of its own txs", n, bn)
			checked++
		}
	}
	// Report the sweep breadth honestly. checked==0 is the COMMON outcome here and must not be
	// dressed up as a pass: once the out-of-order block is rejected the leader cannot advance,
	// so typically NO canonical typed-op block exists above the armed height at all. The claim
	// this test supports is DETECTION (assertion A, which cannot pass vacuously), not a broad
	// safety sweep. The same wedging shows up in the forged-root test's liveness observation.
	if checked == 0 {
		t.Logf("SAFETY-SWEEP: 0 canonical typed-op blocks above #%d to check — the chain does not advance past the rejected block, so this sweep contributes NO evidence; the result rests on the detection assertion above", armAt)
	} else {
		t.Logf("SAFETY-SWEEP: %d canonical typed-op blocks across 3 nodes each commit the fold of their own txs", checked)
	}
}
