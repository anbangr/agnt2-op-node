package conductor

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/wait"
	"github.com/ethereum-optimism/optimism/op-e2e/system/e2esys"
)

// TestSequencerFailover_Partition_IPTables_NoForkOnHeal is the OS-LEVEL analogue of the
// in-process slice-3a partition test (TestSequencerFailover_Partition_MinorityHaltsNoForkOnHeal).
// Instead of an in-process InmemTransport/Mocknet cut, it runs the 3-node op-conductor cluster
// with RealP2P (real libp2p TCP + real TCP raft, each node bound to a distinct 127.0.0.x IP)
// and injects a genuine kernel-netfilter partition with `iptables`, dropping all traffic
// between the isolated node's IP and every other node's IP, both directions. It then removes
// the rules to heal.
//
// It asserts the same invariants slice 3a did, now over real sockets cut by the OS:
//  1. NO SPLIT-BRAIN PRODUCTION — the isolated (former) leader loses quorum, steps down, and
//     its conductor drives the sequencer inactive.
//  2. NO FORK — while isolated the minority never holds a block that diverges from the majority.
//  3. MAJORITY CONTINUES — the 2-node majority elects one leader and advances.
//  4. SINGLE CHAIN ON HEAL — after the iptables rules are removed there is one leader,
//     membership is intact, and all 3 op-geth nodes converge to the majority chain.
//
// Requires Linux (127.0.0.0/8 all-loopback + iptables) and root (to edit netfilter). The op-node
// RPC / engine / geth / batcher all stay on 127.0.0.1 and are NOT cut, so only raft + p2p are
// partitioned and the node stays observable. The shared L1 is not partitioned (so the minority
// may still derive the majority's SAFE chain from L1 — "behind", not strictly frozen), the same
// honest caveat as slice 3a; the upgrade here is that the cut is a real OS-level iptables drop of
// real TCP sockets between real op-conductor/op-node/op-geth processes.
func TestSequencerFailover_Partition_IPTables_NoForkOnHeal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("iptables partition requires Linux (all-loopback 127.0.0.0/8 + netfilter); skipping on " + runtime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("iptables partition requires root; run with `sudo -E $(command -v go) test ...`")
	}

	sys, conductors, cleanup := setupSequencerFailoverTestWithTransports(t, nil, true /* realP2P */)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	topology := sys.Cfg.P2PTopology
	require.NotNil(t, topology, "expected a p2p topology")

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID, "expected an initial leader")
	majorityIDs := make([]string, 0, 2)
	for _, n := range []string{Sequencer1Name, Sequencer2Name, Sequencer3Name} {
		if n != leaderID {
			majorityIDs = append(majorityIDs, n)
		}
	}
	require.Len(t, majorityIDs, 2)
	// Isolate the minority from every other node's IP (the 2 majority sequencers AND the
	// verifier — otherwise gossip relays transitively through the verifier).
	otherNodes := append(append([]string{}, majorityIDs...), VerifierName)

	minIP := e2esys.RealP2PNodeIP(topology, leaderID).String()
	otherIPs := make([]string, 0, len(otherNodes))
	for _, n := range otherNodes {
		otherIPs = append(otherIPs, e2esys.RealP2PNodeIP(topology, n).String())
	}
	t.Logf("IPTABLES-SETUP: minority=%s(%s) others=%v(%v)", leaderID, minIP, otherNodes, otherIPs)

	preBlk, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	t.Logf("IPTABLES-SETUP: pre-partition unsafe head #%d %s", preBlk.NumberU64(), preBlk.Hash().Hex())

	// --- PARTITION via iptables: drop all traffic between minIP and every otherIP. ---
	healIPTables(minIP, otherIPs) // clear any stale rules from a prior aborted run
	defer healIPTables(minIP, otherIPs)
	partitionIPTables(t, minIP, otherIPs)
	t.Logf("IPTABLES: cut %s <-> %v (raft + p2p, both directions)", minIP, otherIPs)

	// (1) NO SPLIT-BRAIN PRODUCTION.
	require.Eventually(t, func() bool {
		isLeader, err := conductors[leaderID].client.Leader(ctx)
		return err == nil && !isLeader
	}, 90*time.Second, 1*time.Second, "isolated leader should step down after losing quorum")
	require.NoError(t, waitForSequencerStatusChange(t, sys.RollupClient(leaderID), false),
		"isolated leader's sequencer should become inactive")
	t.Logf("NO-SPLIT-BRAIN: %s stepped down + sequencer inactive under the iptables cut", leaderID)

	// (3) MAJORITY CONTINUES.
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
	}, 90*time.Second, 1*time.Second, "majority should elect a new leader")
	require.NotEqual(t, leaderID, newLeaderID)

	minAtStep, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, wait.For(ctx, 1*time.Second, func() (bool, error) {
		h, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, nil)
		if err != nil {
			return false, nil
		}
		return h.NumberU64() >= minAtStep.NumberU64()+4, nil
	}), "majority should advance well past the minority")
	majHead, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)

	// (2) NO FORK: minority never diverges from the majority.
	minIsolated, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	require.LessOrEqual(t, minIsolated.NumberU64(), majHead.NumberU64())
	majAtMinHead, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, new(big.Int).SetUint64(minIsolated.NumberU64()))
	require.NoError(t, err)
	require.Equal(t, majAtMinHead.Hash(), minIsolated.Hash(),
		"isolated minority must not hold a block that diverges from the majority")

	forkHeight := majHead.NumberU64() - 1
	majBlkAtFork, err := sys.NodeClient(newLeaderID).BlockByNumber(ctx, new(big.Int).SetUint64(forkHeight))
	require.NoError(t, err)
	wantHash := majBlkAtFork.Hash()
	t.Logf("MAJORITY-CONTINUES: new leader %s at #%d; minority isolated at #%d (%d behind); convergence block #%d",
		newLeaderID, majHead.NumberU64(), minIsolated.NumberU64(), majHead.NumberU64()-minIsolated.NumberU64(), forkHeight)

	// --- HEAL: remove the iptables rules. ---
	healIPTables(minIP, otherIPs)
	t.Logf("HEAL: removed iptables rules for %s", minIP)

	// (4) SINGLE CHAIN ON HEAL.
	ensureOnlyOneLeader(t, sys, conductors)
	require.Eventually(t, func() bool {
		m, err := conductors[newLeaderID].client.ClusterMembership(ctx)
		return err == nil && len(m.Servers) == 3
	}, 30*time.Second, 1*time.Second, "cluster membership should be 3 after heal")
	require.Eventually(t, func() bool {
		for _, n := range []string{Sequencer1Name, Sequencer2Name, Sequencer3Name} {
			b, err := sys.NodeClient(n).BlockByNumber(ctx, new(big.Int).SetUint64(forkHeight))
			if err != nil || b == nil || b.Hash() != wantHash {
				return false
			}
		}
		return true
	}, 90*time.Second, 2*time.Second,
		"all 3 op-geth nodes should converge to the single majority chain at the partition height")
	t.Logf("SINGLE-CHAIN-ON-HEAL: all 3 nodes agree at #%d %s", forkHeight, wantHash.Hex())
}

// partitionIPTables drops all traffic between minIP and every otherIP, in both directions, on
// both the INPUT and OUTPUT chains (loopback packets traverse both). Distinct per-node IPs make
// this a clean per-node cut.
func partitionIPTables(t *testing.T, minIP string, otherIPs []string) {
	for _, o := range otherIPs {
		for _, chain := range []string{"INPUT", "OUTPUT"} {
			require.NoError(t, runIPTables("-I", chain, minIP, o))
			require.NoError(t, runIPTables("-I", chain, o, minIP))
		}
	}
}

// healIPTables removes the rules partitionIPTables added (best-effort: a rule that is not present
// simply returns an error we ignore, so healing is idempotent and safe to call defensively).
func healIPTables(minIP string, otherIPs []string) {
	for _, o := range otherIPs {
		for _, chain := range []string{"INPUT", "OUTPUT"} {
			_ = runIPTables("-D", chain, minIP, o)
			_ = runIPTables("-D", chain, o, minIP)
		}
	}
}

func runIPTables(action, chain, src, dst string) error {
	cmd := exec.Command("iptables", action, chain, "-s", src, "-d", dst, "-j", "DROP")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("iptables %s %s -s %s -d %s: %v: %s", action, chain, src, dst, err, string(out))
	}
	return nil
}
