package conductor

import (
	"context"
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/wait"
)

// agnt2SettlePayload builds ADR-002 calldata for the 0x0BC2 AGNT2 interaction
// precompile, which is what a typed tx's payload field is fed into (InvokeTx.to()
// is hardwired to 0x0BC2 and InvokeTx.data() == Payload).
//
// Layout (pinned op-geth core/vm/agnt2_interaction.go):
//
//	[0]        version, must be 0x00                     -> else revert 0x01
//	[1:5]      uint32 BE step_count
//	[5:37]     ABI offset word, must be exactly 0x20
//	[37:69]    ABI length word, len(workflowID) in [69-4:69]
//	[69:69+p]  workflowID bytes, zero padded to 32
//	then step_count * 160-byte leaves:
//	  [0:32]    keccak256(workflowID)  -> else revert 0x07
//	  [32:64]   stepIdHash    (free)
//	  [64:96]   agentRoleHash (free)
//	  [96:128]  payout        (free)
//	  [128:160] keccak256(previous leaf), zeros for i==0 -> else revert 0x08
//
// The precompile requires len(input) == 5 + 64 + padded + step_count*160 exactly,
// and len(workflowID) > 0.
//
// steps == 0 is a legal, revert-free call that builds the empty MMR and emits NO
// logs: receipt status 1, interactionCount unchanged. That is deliberate for the
// primary test - it isolates "typed tx reached a block" from "the interaction-log
// fold path works", which no op-e2e test has exercised before.
func agnt2SettlePayload(workflowID string, steps int) []byte {
	wf := []byte(workflowID)
	padded := (len(wf) + 31) / 32 * 32

	out := make([]byte, 0, 5+64+padded+steps*160)
	out = append(out, 0x00)
	var sc [4]byte
	binary.BigEndian.PutUint32(sc[:], uint32(steps))
	out = append(out, sc[:]...)

	off := make([]byte, 32)
	off[31] = 0x20
	out = append(out, off...)

	lw := make([]byte, 32)
	binary.BigEndian.PutUint32(lw[28:], uint32(len(wf)))
	out = append(out, lw...)

	body := make([]byte, padded)
	copy(body, wf)
	out = append(out, body...)

	wfHash := crypto.Keccak256(wf)
	var prev [32]byte
	for i := 0; i < steps; i++ {
		leaf := make([]byte, 160)
		copy(leaf[0:32], wfHash)
		leaf[63] = byte(i + 1) // stepIdHash - opaque to the precompile
		leaf[95] = 0x01        // agentRoleHash - opaque
		leaf[127] = 0x2a       // payout - opaque
		copy(leaf[128:160], prev[:])
		out = append(out, leaf...)
		copy(prev[:], crypto.Keccak256(leaf))
	}
	return out
}

// TestSequencerFailover_AGNT2TypedOpsSealedAndPropagate submits real AGNT2 typed
// transactions (0x7A INVOKE, 0x7B RESPOND) to the op-conductor cluster leader and
// proves that the sealed block carries a NON-NIL typedOpRoot, and that the root
// survives gossip to the other sequencers.
//
// WHY THIS EXISTS. TestSequencerFailover_AGNT2FieldsLiveAndPropagate proves the
// cluster is AGNT2-active via interactionRoot, but typedOpRoot is nil on every
// block it inspects - not because the path is broken, but because no typed tx is
// ever submitted. consensus/beacon FinalizeAndAssemble only stamps
// header.TypedOpRoot when FoldTypedOpRoot(body.Transactions) returns count > 0.
// Until a typed tx lands in a cluster block there is nothing for a Byzantine
// leader to forge, so the equivocation test has no substrate.
func TestSequencerFailover_AGNT2TypedOpsSealedAndPropagate(t *testing.T) {
	sys, conductors, cleanup := setupSequencerFailoverTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID)

	// NodeClient dials the node's op-geth RPC directly, bypassing the conductor
	// proxy - which is what we want, we are deliberately targeting the leader.
	l2 := sys.NodeClient(leaderID)

	chainID := sys.Cfg.L2ChainIDBig()
	// newModernSigner sets the InvokeTx/RespondTx/ComposeTypedTx bits
	// unconditionally, so LatestSignerForChainID is enough - no Isthmus-specific
	// signer needed, and the sighash depends only on chainID + type byte, so it
	// is compatible with whatever signer the node recovers with.
	signer := types.LatestSignerForChainID(chainID)

	alice := sys.Cfg.Secrets.Alice // premined 1000 ETH on L2 (e2esys setup Premine)
	bob := sys.Cfg.Secrets.Bob
	aliceAddr := sys.Cfg.Secrets.Addresses().Alice
	bobAddr := sys.Cfg.Secrets.Addresses().Bob

	head, err := l2.HeaderByNumber(ctx, nil)
	require.NoError(t, err)
	tip := big.NewInt(params.GWei)
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)

	aliceNonce, err := l2.PendingNonceAt(ctx, aliceAddr)
	require.NoError(t, err)
	bobNonce, err := l2.PendingNonceAt(ctx, bobAddr)
	require.NoError(t, err)

	wf := common.HexToHash("0x0000000000000000000000000000000000000000000000000000000000001000")

	// (1) INVOKE, type 0x7A.
	//   DepInvokeIds MUST be non-nil (txpool: ErrAgnt2DepInvokeIdsNil at
	//   MarshalBinary time) AND empty (a dep hash not present in the miner's
	//   batch makes the miner DEFER the invoke out of the block entirely).
	//   AgentRole must be non-empty, <=64 bytes, valid UTF-8, NFC-normalized.
	//   Gas: intrinsic 21000 + calldata + 2000 typed surcharge, then the
	//   precompile itself charges AGNT2BaseGas 21000 (+4405/leaf). 500k is ample.
	invoke, err := types.SignNewTx(alice, signer, &types.InvokeTx{
		ChainID:      chainID,
		Nonce:        aliceNonce,
		GasTipCap:    tip,
		GasFeeCap:    feeCap,
		Gas:          500_000,
		WorkflowId:   wf,
		StepId:       1,
		AgentRole:    "agnt2-e2e-worker",
		DepInvokeIds: []common.Hash{},
		Payload:      agnt2SettlePayload("agnt2-e2e-wf-1", 0),
	})
	require.NoError(t, err)
	require.Equal(t, uint8(types.InvokeTxType), invoke.Type())

	// (2) RESPOND, type 0x7B, from a DIFFERENT sender so there is no same-sender
	//     nonce coupling, and with InvokeRef pointing at the invoke - a real
	//     in-block dependency edge, which is the substrate the ordering validator
	//     (and later the equivocation test) needs. Status must be 0, 1 or 2.
	respond, err := types.SignNewTx(bob, signer, &types.RespondTx{
		ChainID:         chainID,
		Nonce:           bobNonce,
		GasTipCap:       tip,
		GasFeeCap:       feeCap,
		Gas:             500_000,
		WorkflowId:      wf,
		StepId:          2, // distinct (type, workflowId, stepId) or the miner dedups it
		InvokeRef:       invoke.Hash(),
		ResponsePayload: agnt2SettlePayload("agnt2-e2e-wf-1", 0),
		Status:          0,
	})
	require.NoError(t, err)
	require.Equal(t, uint8(types.RespondTxType), respond.Type())

	// SendTransaction == MarshalBinary + eth_sendRawTransaction. A failure here
	// means either envelope validation rejected it locally or the pool refused
	// the type - both are load-bearing, so assert loudly.
	require.NoError(t, l2.SendTransaction(ctx, invoke), "leader must accept the 0x7A INVOKE")
	require.NoError(t, l2.SendTransaction(ctx, respond), "leader must accept the 0x7B RESPOND")

	// step_count == 0 is a valid precompile call, so expect status 1. If you
	// change the payload to garbage the tx still LANDS (and still bumps
	// typedOpCount) but reverts - use wait.ForReceiptMaybe(..., 0, true) then.
	rcpt, err := wait.ForReceiptOK(ctx, l2, invoke.Hash())
	require.NoError(t, err)
	blockNum := rcpt.BlockNumber.Uint64()
	t.Logf("INVOKE %s mined in block #%d (gasUsed=%d)", invoke.Hash(), blockNum, rcpt.GasUsed)

	// (3) The tx really is in the block, still typed after a full
	//     encode/gossip/decode round trip.
	blk, err := l2.BlockByHash(ctx, rcpt.BlockHash)
	require.NoError(t, err)
	var typedInBlock int
	for _, btx := range blk.Transactions() {
		switch btx.Type() {
		case types.InvokeTxType, types.RespondTxType, types.ComposeTypedTxType:
			typedInBlock++
		}
	}
	require.GreaterOrEqual(t, typedInBlock, 1, "block #%d must contain the typed tx", blockNum)

	// (4) THE POINT: the sealed header commits a non-nil typedOpRoot. Read the
	//     raw JSON the node actually serves rather than trusting Go decoding.
	var raw map[string]any
	require.NoError(t, l2.Client().CallContext(ctx, &raw,
		"eth_getBlockByNumber", hexUint64(blockNum), false))
	require.NotNil(t, raw["typedOpRoot"],
		"sealed block #%d must carry a non-nil typedOpRoot (raw header: %v)", blockNum, raw)
	require.NotNil(t, raw["typedOpCount"])
	require.NotEqual(t, "0x0", raw["typedOpCount"], "typedOpCount must be > 0")
	// NON-VACUITY FOR THE B2' CL FIX: this block must also carry typedReexecRoot/Count.
	// FoldTypedReexecRoot appends a leaf for every INVOKE with no skip path, so a block
	// holding a typed op necessarily has them -- and those two fields are hash-relevant but
	// were, until the payload carried them, invisible to op-node. Without this assertion the
	// propagation checks below would still pass on an op-geth predating B2' Stage 2, and
	// would say nothing about whether op-node can hash a real typed-op block.
	require.NotNil(t, raw["typedReexecRoot"],
		"sealed block #%d must carry typedReexecRoot, else the propagation checks below say nothing about whether op-node can hash a real typed-op block (raw header: %v)",
		blockNum, raw)
	require.NotEqual(t, "0x0", raw["typedReexecCount"], "typedReexecCount must be > 0")
	t.Logf("REEXEC-COVERAGE: block #%d carries typedReexecRoot=%v count=%v — the B2' CL fix is exercised",
		blockNum, raw["typedReexecRoot"], raw["typedReexecCount"])
	t.Logf("TYPED-OP SEALED: block #%d typedOpRoot=%v typedOpCount=%v interactionRoot=%v interactionCount=%v typedReexecRoot=%v typedReexecCount=%v",
		blockNum, raw["typedOpRoot"], raw["typedOpCount"],
		raw["interactionRoot"], raw["interactionCount"],
		raw["typedReexecRoot"], raw["typedReexecCount"])

	// (5) Cross-check the fold locally: the header commitment is the MMR over the
	//     block's typed tx hashes, not an opaque value the sequencer made up.
	hdr, err := l2.HeaderByHash(ctx, rcpt.BlockHash)
	require.NoError(t, err)
	require.NotNil(t, hdr.TypedOpRoot)
	require.NotNil(t, hdr.TypedOpCount)
	wantRoot, wantCount := types.FoldTypedOpRoot(blk.Transactions())
	require.Equal(t, wantRoot, *hdr.TypedOpRoot, "header typedOpRoot must equal the local fold")
	require.Equal(t, wantCount, *hdr.TypedOpCount)
	require.Equal(t, uint64(typedInBlock), wantCount)

	// (6) The typed-op commitment survives propagation. This is the direct
	//     precondition for slice 4: honest followers must independently hold the
	//     same typedOpRoot for the forged one to be rejectable.
	for _, n := range []string{Sequencer1Name, Sequencer2Name, Sequencer3Name} {
		if n == leaderID {
			continue
		}
		follower := sys.NodeClient(n)
		require.NoError(t, wait.For(ctx, 1*time.Second, func() (bool, error) {
			b, err := follower.BlockByNumber(ctx, nil)
			if err != nil {
				return false, nil
			}
			return b.NumberU64() >= blockNum, nil
		}), "follower %s should receive block #%d", n, blockNum)

		var fraw map[string]any
		require.NoError(t, follower.Client().CallContext(ctx, &fraw,
			"eth_getBlockByNumber", hexUint64(blockNum), false))
		require.Equal(t, raw["hash"], fraw["hash"], "follower %s must be on the same block", n)
		require.NotNil(t, fraw["typedOpRoot"], "follower %s lost typedOpRoot in transit", n)
		require.Equal(t, raw["typedOpRoot"], fraw["typedOpRoot"],
			"follower %s must agree with the leader on typedOpRoot at #%d", n, blockNum)
		require.Equal(t, raw["typedOpCount"], fraw["typedOpCount"])
		t.Logf("PROPAGATED: follower %s block #%d typedOpRoot matches the leader", n, blockNum)
	}
}

// TestSequencerFailover_AGNT2TypedOpMovesInteractionCount is the OPTIONAL second
// step: a one-leaf payload makes the precompile emit an AGNT2LeafEvent, so the
// interaction fold has real leaves and interactionCount goes above 0. Kept
// separate from the typedOpRoot test on purpose - it exercises a path
// (FoldInteractionRoot over non-empty logs, on both the sealing and the import
// side) that NO op-e2e test has ever run, so a failure here must not be able to
// mask the simpler typedOpRoot result.
func TestSequencerFailover_AGNT2TypedOpMovesInteractionCount(t *testing.T) {
	sys, conductors, cleanup := setupSequencerFailoverTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID)
	l2 := sys.NodeClient(leaderID)

	chainID := sys.Cfg.L2ChainIDBig()
	signer := types.LatestSignerForChainID(chainID)
	alice := sys.Cfg.Secrets.Alice
	aliceAddr := sys.Cfg.Secrets.Addresses().Alice

	head, err := l2.HeaderByNumber(ctx, nil)
	require.NoError(t, err)
	tip := big.NewInt(params.GWei)
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
	nonce, err := l2.PendingNonceAt(ctx, aliceAddr)
	require.NoError(t, err)

	invoke, err := types.SignNewTx(alice, signer, &types.InvokeTx{
		ChainID:      chainID,
		Nonce:        nonce,
		GasTipCap:    tip,
		GasFeeCap:    feeCap,
		Gas:          500_000,
		WorkflowId:   common.HexToHash("0x2000"),
		StepId:       1,
		AgentRole:    "agnt2-e2e-worker",
		DepInvokeIds: []common.Hash{},
		Payload:      agnt2SettlePayload("agnt2-e2e-wf-2", 2),
	})
	require.NoError(t, err)
	require.NoError(t, l2.SendTransaction(ctx, invoke))

	rcpt, err := wait.ForReceiptOK(ctx, l2, invoke.Hash())
	require.NoError(t, err)
	require.NotEmpty(t, rcpt.Logs, "the 0x0BC2 precompile must emit one AGNT2LeafEvent per step")

	var raw map[string]any
	require.NoError(t, l2.Client().CallContext(ctx, &raw,
		"eth_getBlockByNumber", hexUint64(rcpt.BlockNumber.Uint64()), false))
	require.NotNil(t, raw["typedOpRoot"])
	require.NotEqual(t, "0x0", raw["interactionCount"],
		"a leaf-emitting INVOKE must move interactionCount above 0 (raw header: %v)", raw)
	t.Logf("INTERACTION LEAVES: block #%d interactionRoot=%v interactionCount=%v typedOpRoot=%v",
		rcpt.BlockNumber, raw["interactionRoot"], raw["interactionCount"], raw["typedOpRoot"])
}
