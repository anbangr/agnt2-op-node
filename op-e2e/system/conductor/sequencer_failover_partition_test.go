package conductor

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/hashicorp/raft"
	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/wait"
	"github.com/ethereum-optimism/optimism/op-e2e/system/e2esys"
)

// TestSequencerFailover_Partition_MinorityHaltsNoForkOnHeal injects a TRUE, heal-able
// partition that isolates one node at BOTH layers that matter — the op-conductor Raft
// consensus (InmemTransport) AND the op-node block-gossip p2p (libp2p Mocknet) — so the
// isolated node genuinely cannot participate in consensus and cannot receive the majority's
// blocks over gossip. It then heals both and asserts the partition-safety invariants a
// crash-stop cannot exercise:
//
//	(1) NO SPLIT-BRAIN PRODUCTION — the isolated (former) leader loses quorum, steps down,
//	    and its conductor drives the sequencer INACTIVE: it produces no blocks while isolated.
//	(2) NO FORK — while isolated the minority never holds a block that DIVERGES from the
//	    majority: at the minority's own head height, its block hash equals the majority's
//	    block at that height (the minority only ever tracks a prefix of the single canonical
//	    chain; it never builds a competing one).
//	(3) MAJORITY CONTINUES — the two-node majority elects exactly one new leader and advances
//	    its unsafe head well past the minority.
//	(4) SINGLE CHAIN ON HEAL — after healing raft + p2p there is exactly one cluster-wide
//	    leader, membership is intact (3 servers), and all three op-geth nodes converge to the
//	    same block hash at a height the majority produced while the minority was isolated: the
//	    rejoined node adopts the single majority chain.
//
// FIDELITY (honest): the cut is injected in-process — raft via hashicorp/raft's InmemTransport
// Connect/Disconnect and block-gossip via libp2p Mocknet Unlink/Disconnect — NOT at the OS/TCP
// layer, and only as a symmetric hard cut with an instant heal. It drives the REAL AGNT2 stack
// (real op-node sequencing, real op-geth fork-choice, real unsafeHeadTracker FSM, real
// hashicorp/raft) but bypasses the production NetworkTransport/libp2p sockets and their
// reconnect paths, and does not exercise asymmetric/gray failures. Note the shared in-process
// L1 is NOT partitioned, so the isolated node may still derive the majority's SAFE chain from
// L1 (which is why invariant (2) checks non-divergence rather than a strictly frozen head).
// FUNDS-CONSERVED is an L2/escrow property out of scope here (slice 3b).
func TestSequencerFailover_Partition_MinorityHaltsNoForkOnHeal(t *testing.T) {
	seqNames := []string{Sequencer1Name, Sequencer2Name, Sequencer3Name}

	inmem := make(map[string]*raft.InmemTransport, len(seqNames))
	transports := make(map[string]raft.Transport, len(seqNames))
	for _, n := range seqNames {
		_, tr := raft.NewInmemTransportWithTimeout(raft.ServerAddress(n), 500*time.Millisecond)
		inmem[n] = tr
		transports[n] = tr
	}
	for _, a := range seqNames {
		for _, b := range seqNames {
			if a != b {
				inmem[a].Connect(raft.ServerAddress(b), inmem[b])
			}
		}
	}

	sys, conductors, cleanup := setupSequencerFailoverTestWithTransports(t, transports)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID, "expected an initial leader")
	majorityIDs := make([]string, 0, 2)
	for _, n := range seqNames {
		if n != leaderID {
			majorityIDs = append(majorityIDs, n)
		}
	}
	require.Len(t, majorityIDs, 2)
	// Every node the minority must be cut off from over p2p (the 2 majority sequencers AND the
	// verifier — gossip relays transitively, so leaving any path open would let the minority
	// keep following the majority chain).
	p2pPeers := []string{majorityIDs[0], majorityIDs[1], VerifierName}

	preBlk, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	t.Logf("PARTITION-SETUP: pre-partition unsafe head #%d %s; minority=%s majority=%v",
		preBlk.NumberU64(), preBlk.Hash().Hex(), leaderID, majorityIDs)

	// --- PARTITION at BOTH layers: raft + p2p. ---
	partitionRaft(inmem, []string{leaderID}, majorityIDs)
	partitionP2P(t, sys, leaderID, p2pPeers)
	t.Logf("PARTITION: isolated minority=%s from majority=%v + verifier (raft + p2p)", leaderID, majorityIDs)

	// (1) NO SPLIT-BRAIN PRODUCTION: the isolated leader loses quorum, steps down, sequencer inactive.
	require.Eventually(t, func() bool {
		isLeader, err := conductors[leaderID].client.Leader(ctx)
		return err == nil && !isLeader
	}, 60*time.Second, 1*time.Second, "isolated leader should step down after losing quorum")
	require.NoError(t, waitForSequencerStatusChange(t, sys.RollupClient(leaderID), false),
		"isolated leader's sequencer should become inactive (no split-brain production)")
	minAtStep, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	t.Logf("NO-SPLIT-BRAIN: %s stepped down, sequencer inactive, head #%d %s",
		leaderID, minAtStep.NumberU64(), minAtStep.Hash().Hex())

	// (3) MAJORITY CONTINUES: exactly one majority node becomes leader and advances well past
	// the minority's isolated head.
	var newLeaderID string
	require.Eventually(t, func() bool {
		for _, n := range majorityIDs {
			isLeader, err := conductors[n].client.Leader(ctx)
			if err == nil && isLeader {
				newLeaderID = n
				return true
			}
		}
		return false
	}, 60*time.Second, 1*time.Second, "majority should elect a new leader")
	require.NotEqual(t, leaderID, newLeaderID, "new leader must differ from the isolated old leader")

	require.NoError(t, wait.For(ctx, 1*time.Second, func() (bool, error) {
		h, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, nil)
		if err != nil {
			return false, nil
		}
		return h.NumberU64() >= minAtStep.NumberU64()+4, nil
	}), "majority should advance well past the minority's isolated head")
	majHead, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)

	// (2) NO FORK while isolated: the minority never holds a block that diverges from the
	// majority. Whatever the minority's head is (frozen, or crept forward via L1 derivation of
	// the majority's own safe chain), its block at that height must equal the majority's block
	// at that height, and it must not have run ahead of the majority.
	minIsolated, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	require.LessOrEqual(t, minIsolated.NumberU64(), majHead.NumberU64(),
		"isolated minority must not run ahead of the majority (it cannot outproduce a quorum)")
	majAtMinHead, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, new(big.Int).SetUint64(minIsolated.NumberU64()))
	require.NoError(t, err)
	require.Equal(t, majAtMinHead.Hash(), minIsolated.Hash(),
		"isolated minority must not hold a block that diverges from the majority (no competing fork)")
	t.Logf("NO-FORK: minority isolated head #%d %s matches majority at that height; majority advanced to #%d",
		minIsolated.NumberU64(), minIsolated.Hash().Hex(), majHead.NumberU64())

	// Convergence height: a recent block the majority produced while the minority was isolated.
	// When the isolation is clean (the minority is frozen well behind — the common case, since
	// p2p gossip is cut) the minority does NOT hold this block and must adopt it on heal; when
	// the minority crept forward by deriving the majority's SAFE chain from the shared,
	// un-partitioned L1, it already holds it. Either way it is the single majority chain, so we
	// derive the height from the majority head (robust) rather than assuming the minority froze.
	forkHeight := majHead.NumberU64() - 1
	majBlkAtFork, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, new(big.Int).SetUint64(forkHeight))
	require.NoError(t, err)
	wantHash := majBlkAtFork.Hash()
	t.Logf("MAJORITY-CONTINUES: new leader %s at #%d; minority isolated at #%d (%d blocks behind); convergence block #%d = %s",
		newLeaderID, majHead.NumberU64(), minIsolated.NumberU64(), majHead.NumberU64()-minIsolated.NumberU64(), forkHeight, wantHash.Hex())

	// --- HEAL both layers. ---
	healRaft(inmem, []string{leaderID}, majorityIDs)
	healP2P(t, sys, leaderID, p2pPeers)
	t.Logf("HEAL: reconnected minority=%s (raft + p2p)", leaderID)

	// (4a) SINGLE CHAIN: exactly one cluster-wide leader after heal.
	ensureOnlyOneLeader(t, sys, conductors)

	// (4b) membership intact.
	require.Eventually(t, func() bool {
		m, err := conductors[newLeaderID].client.ClusterMembership(ctx)
		return err == nil && len(m.Servers) == 3
	}, 30*time.Second, 1*time.Second, "cluster membership should be 3 after heal")

	// (4c) all three op-geth nodes converge to the majority's block at a height the majority
	// produced while the minority was isolated — the rejoined node adopts the single chain.
	require.Eventually(t, func() bool {
		for _, n := range seqNames {
			b, err := sys.NodeClient(n).BlockByNumber(ctx, new(big.Int).SetUint64(forkHeight))
			if err != nil || b == nil || b.Hash() != wantHash {
				return false
			}
		}
		return true
	}, 90*time.Second, 2*time.Second,
		"all 3 op-geth nodes should converge to the single majority chain at the partition height (minority cannot impose a fork)")
	t.Logf("SINGLE-CHAIN-ON-HEAL: all 3 nodes agree at #%d %s", forkHeight, wantHash.Hex())
}

// partitionRaft cuts the in-memory raft transports between every minority server and every
// majority server, in BOTH directions, while every node stays alive.
func partitionRaft(inmem map[string]*raft.InmemTransport, minority, majority []string) {
	for _, m := range minority {
		for _, j := range majority {
			inmem[m].Disconnect(raft.ServerAddress(j))
			inmem[j].Disconnect(raft.ServerAddress(m))
		}
	}
}

// healRaft restores the connections partitionRaft removed, in both directions.
func healRaft(inmem map[string]*raft.InmemTransport, minority, majority []string) {
	for _, m := range minority {
		for _, j := range majority {
			inmem[m].Connect(raft.ServerAddress(j), inmem[j])
			inmem[j].Connect(raft.ServerAddress(m), inmem[m])
		}
	}
}

// partitionP2P severs the libp2p block-gossip links between the minority node and every peer
// in `peers`, in both directions, so the minority cannot receive the majority's blocks over
// gossip. UnlinkPeers removes the ability to reconnect; DisconnectPeers drops any live conn.
func partitionP2P(t *testing.T, sys *e2esys.System, minority string, peers []string) {
	minID := sys.RollupNodes[minority].P2P().Host().ID()
	for _, p := range peers {
		pID := sys.RollupNodes[p].P2P().Host().ID()
		_ = sys.Mocknet.DisconnectPeers(minID, pID)
		_ = sys.Mocknet.DisconnectPeers(pID, minID)
		require.NoError(t, sys.Mocknet.UnlinkPeers(minID, pID))
	}
}

// healP2P restores the libp2p links partitionP2P removed and reconnects the peers.
func healP2P(t *testing.T, sys *e2esys.System, minority string, peers []string) {
	minID := sys.RollupNodes[minority].P2P().Host().ID()
	for _, p := range peers {
		pID := sys.RollupNodes[p].P2P().Host().ID()
		_, err := sys.Mocknet.LinkPeers(minID, pID)
		require.NoError(t, err)
		_, _ = sys.Mocknet.ConnectPeers(minID, pID)
	}
}
