package conductor

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/wait"
)

// TestSequencerFailover_RealP2P_Smoke brings up the 3-node op-conductor cluster with REAL
// libp2p TCP p2p (SystemConfig.RealP2P) instead of the in-process Mocknet, and real TCP raft
// (no transport override — the production path). It proves the cluster forms over real
// networking: setup itself requires a single elected leader and all three sequencers healthy
// (which requires their op-geth to be syncing the leader's blocks), and this test additionally
// requires all three op-geth nodes to reach a fresh leader block via REAL libp2p gossip.
//
// This is the milestone-1 enabler for the OS-level iptables partition test (WS1 slice 3b):
// once p2p is real TCP, a Linux `iptables` cut of the real raft + p2p ports produces a genuine
// OS-level partition (which the in-process Mocknet/InmemTransport cut of slice 3a could not).
func TestSequencerFailover_RealP2P_Smoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("RealP2P binds each node to a distinct 127.0.0.x loopback IP (for iptables-by-IP partitioning), which requires Linux; skipping on " + runtime.GOOS)
	}
	sys, conductors, cleanup := setupSequencerFailoverTestWithTransports(t, nil, true /* realP2P */)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// A single leader was elected over real TCP raft.
	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID, "expected a leader over real p2p + real raft")

	// Blocks propagate to the followers over REAL libp2p gossip: wait for the leader's current
	// head, then require every op-geth node to reach it.
	leaderBlk, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	target := leaderBlk.NumberU64()
	require.Greater(t, target, uint64(0), "leader should have produced blocks")
	for _, name := range []string{Sequencer1Name, Sequencer2Name, Sequencer3Name} {
		nm := name
		require.NoError(t, wait.For(ctx, 1*time.Second, func() (bool, error) {
			h, err := sys.NodeClient(nm).BlockByNumber(ctx, nil)
			if err != nil {
				return false, nil
			}
			return h.NumberU64() >= target, nil
		}), "node %s should reach block #%d via real p2p gossip", nm, target)
	}
	t.Logf("REAL-P2P-SMOKE: 3-node cluster formed over real libp2p TCP; leader=%s; all 3 nodes reached #%d", leaderID, target)
}
