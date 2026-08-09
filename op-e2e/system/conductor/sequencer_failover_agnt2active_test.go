package conductor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/wait"
)

// TestSequencerFailover_AGNT2FieldsLiveAndPropagate proves that the op-conductor cluster is
// genuinely running AGNT2-ACTIVE, and that the AGNT2 header fields survive propagation.
//
// WHY THIS EXISTS. The cluster suite previously ran on EcotoneSystemConfig, where op-geth
// leaves all four AGNT2 header fields nil — so every cluster result (leader failover,
// partition safety) was measured with AGNT2's typed-op header path inert. Simply switching
// the config to Isthmus is not enough to claim "AGNT2-active": that is a label, not a
// measurement. This test measures it, by reading the field off real blocks over RPC.
//
// It also pins the fix for the payload-SSZ defect at the system level: the sequencer seals a
// block carrying InteractionRoot, that block is gossiped as a BlockV5 payload, and the
// FOLLOWERS' copies must carry the identical InteractionRoot. Before the BlockV5 codec the
// followers could not accept such a block at all ("payload has bad block hash"), so
// agreement here is only possible if the field really is on the wire.
func TestSequencerFailover_AGNT2FieldsLiveAndPropagate(t *testing.T) {
	sys, conductors, cleanup := setupSequencerFailoverTest(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	leaderID, _ := findLeader(t, conductors)
	require.NotEmpty(t, leaderID)

	// Let the leader seal a few blocks past genesis.
	head, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
	require.NoError(t, err)
	target := head.NumberU64() + 2
	require.NoError(t, wait.For(ctx, 1*time.Second, func() (bool, error) {
		h, err := sys.NodeClient(leaderID).BlockByNumber(ctx, nil)
		if err != nil {
			return false, nil
		}
		return h.NumberU64() >= target, nil
	}), "leader should keep producing blocks")

	// Read the raw header JSON so we assert on what the node actually serves, not on Go
	// struct decoding.
	getInteractionRoot := func(node string, blockNum uint64) (any, map[string]any) {
		var raw map[string]any
		err := sys.NodeClient(node).Client().CallContext(
			ctx, &raw, "eth_getBlockByNumber", hexUint64(blockNum), false)
		require.NoError(t, err, "eth_getBlockByNumber on %s", node)
		require.NotNil(t, raw, "no block %d on %s", blockNum, node)
		return raw["interactionRoot"], raw
	}

	// (1) The AGNT2 fork tag is genuinely ACTIVE: the leader's sealed block carries a
	// non-null interactionRoot. On Ecotone this field is absent/null.
	leaderRoot, rawLeader := getInteractionRoot(leaderID, target)
	require.NotNil(t, leaderRoot,
		"interactionRoot must be present on the leader's block — the cluster is NOT AGNT2-active (raw header: %v)", rawLeader)
	require.NotEqual(t, "", leaderRoot)
	t.Logf("AGNT2-ACTIVE: leader %s block #%d interactionRoot=%v (interactionCount=%v)",
		leaderID, target, leaderRoot, rawLeader["interactionCount"])

	// (2) The field SURVIVES PROPAGATION: every other sequencer, having received the block
	// over real gossip as a BlockV5 payload, reports the identical interactionRoot. This is
	// the system-level proof that the AGNT2 header fields are on the wire.
	for _, n := range []string{Sequencer1Name, Sequencer2Name, Sequencer3Name} {
		if n == leaderID {
			continue
		}
		require.NoError(t, wait.For(ctx, 1*time.Second, func() (bool, error) {
			b, err := sys.NodeClient(n).BlockByNumber(ctx, nil)
			if err != nil {
				return false, nil
			}
			return b.NumberU64() >= target, nil
		}), "follower %s should receive the leader's blocks", n)

		followerRoot, _ := getInteractionRoot(n, target)
		require.NotNil(t, followerRoot, "follower %s lost interactionRoot in transit", n)
		require.Equal(t, leaderRoot, followerRoot,
			"follower %s must agree with the leader on interactionRoot at #%d", n, target)
		t.Logf("PROPAGATED: follower %s block #%d interactionRoot matches the leader", n, target)
	}
}

func hexUint64(v uint64) string {
	const digits = "0123456789abcdef"
	if v == 0 {
		return "0x0"
	}
	var buf [16]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return "0x" + string(buf[i:])
}
