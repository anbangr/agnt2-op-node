package agnt2utils

import (
	"bytes"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

// TestAccumulator_EmptyState_MatchesEmptyMMRConvention pins the genesis
// snapshot's root to keccak256(""), the same convention used by op-geth's
// agnt2MMR.getRoot() and core/types.FoldInteractionRoot.
func TestAccumulator_EmptyState_MatchesEmptyMMRConvention(t *testing.T) {
	s := EmptyAccumulatorState()
	require.Equal(t, uint64(0), s.Count)
	require.Empty(t, s.Peaks)
	want := crypto.Keccak256([]byte{})
	require.True(t, bytes.Equal(s.Root[:], want), "empty state root must equal keccak256(\"\")")
}

// TestAccumulator_IncrementalEqualsBatch is the core consensus invariant:
// folding leaves one-at-a-time across N steps must produce identical
// (root, count, peaks) to folding all leaves in one step. This is what
// guarantees op-node's incremental fold matches op-geth's per-block batch
// fold (FoldInteractionRoot) byte-for-byte.
func TestAccumulator_IncrementalEqualsBatch(t *testing.T) {
	cases := []int{1, 2, 3, 4, 5, 7, 8, 9, 15, 16, 17}
	for _, n := range cases {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			leaves := makeLeaves(n)

			// Batch
			batch := IncrementalAppend(EmptyAccumulatorState(), leaves)

			// Incremental — one leaf at a time
			incr := EmptyAccumulatorState()
			for _, l := range leaves {
				incr = IncrementalAppend(incr, [][32]byte{l})
			}

			require.Equal(t, batch.Count, incr.Count, "count mismatch N=%d", n)
			require.Equal(t, batch.Root, incr.Root, "root mismatch N=%d (incremental != batch)", n)
			require.Equal(t, len(batch.Peaks), len(incr.Peaks), "peak count mismatch N=%d", n)
			for i := range batch.Peaks {
				require.Equal(t, batch.Peaks[i], incr.Peaks[i], "peak[%d] mismatch N=%d", i, n)
			}
		})
	}
}

// TestAccumulator_ReorgIsolation locks the reorg-correctness property:
// two divergent branches off the same parent produce different snapshots,
// each remains queryable after the other is written, and folding from the
// divergence-point's snapshot reproduces each branch's tip independently.
//
// This is the load-bearing property the Phase 7 plan called out: when the
// canonical chain switches branches, op-node uses the persisted divergence
// snapshot to re-fold rather than re-fold from genesis.
func TestAccumulator_ReorgIsolation(t *testing.T) {
	acc := NewAccumulator()

	// Common ancestor: empty MMR
	parentHash := common.HexToHash("0x01")
	acc.Put(parentHash, EmptyAccumulatorState())

	// Branch A: parent → blockA1 (3 leaves) → blockA2 (2 more leaves)
	leavesA1 := makeLeavesWithSeed(3, "A1")
	stateA1, ok := acc.AppendLeavesToParent(parentHash, leavesA1)
	require.True(t, ok, "parent snapshot must be present")
	hashA1 := common.HexToHash("0x0a01")
	acc.Put(hashA1, stateA1)

	leavesA2 := makeLeavesWithSeed(2, "A2")
	stateA2, ok := acc.AppendLeavesToParent(hashA1, leavesA2)
	require.True(t, ok)
	hashA2 := common.HexToHash("0x0a02")
	acc.Put(hashA2, stateA2)

	// Branch B: parent → blockB1 (4 different leaves)
	leavesB1 := makeLeavesWithSeed(4, "B1")
	stateB1, ok := acc.AppendLeavesToParent(parentHash, leavesB1)
	require.True(t, ok, "parent snapshot must be present from branch B's view too")
	hashB1 := common.HexToHash("0x0b01")
	acc.Put(hashB1, stateB1)

	// Both branches must remain queryable
	gotA2, ok := acc.Get(hashA2)
	require.True(t, ok)
	gotB1, ok := acc.Get(hashB1)
	require.True(t, ok)

	// Their roots must differ — divergent leaves produce divergent MMR roots
	require.NotEqual(t, gotA2.Root, gotB1.Root, "branch A tip and branch B tip must have distinct roots")
	require.NotEqual(t, gotA2.Count, gotB1.Count, "test fixtures: counts also differ here")

	// Folding from the parent again must reproduce branch B exactly,
	// proving incremental replay from the divergence point is byte-stable
	// — the property that lets op-node recover from any reorg by replaying
	// from the divergence snapshot.
	replay, _ := acc.AppendLeavesToParent(parentHash, leavesB1)
	require.Equal(t, gotB1.Root, replay.Root)
	require.Equal(t, gotB1.Count, replay.Count)
}

// TestAccumulator_GetReturnsCopy proves the accumulator does not return
// aliased internal slices that callers could mutate.
func TestAccumulator_GetReturnsCopy(t *testing.T) {
	acc := NewAccumulator()
	leaves := makeLeaves(3)
	state := IncrementalAppend(EmptyAccumulatorState(), leaves)
	hash := common.HexToHash("0x01")
	acc.Put(hash, state)

	got, ok := acc.Get(hash)
	require.True(t, ok)
	require.NotEmpty(t, got.Peaks)

	// Mutate the returned slice
	for i := range got.Peaks {
		got.Peaks[i] = [32]byte{0xff}
	}

	got2, _ := acc.Get(hash)
	require.NotEqual(t, got.Peaks, got2.Peaks, "internal state must be insulated from caller mutation")
}

// TestAccumulator_AssertRoot verifies the assertion helper returns nil on a
// matching root and a descriptive error on mismatch or unknown block.
func TestAccumulator_AssertRoot(t *testing.T) {
	acc := NewAccumulator()
	blockHash := common.HexToHash("0xdeadbeef")

	leaves := makeLeaves(3)
	state := IncrementalAppend(EmptyAccumulatorState(), leaves)
	acc.Put(blockHash, state)

	// nil on match
	require.NoError(t, acc.AssertRoot(blockHash, state.Root))

	// error on mismatch
	wrongRoot := common.HexToHash("0x1234")
	err := acc.AssertRoot(blockHash, wrongRoot)
	require.Error(t, err)
	require.Contains(t, err.Error(), "root mismatch")
	require.Contains(t, err.Error(), blockHash.Hex())

	// error on unknown block
	unknownHash := common.HexToHash("0xffffffff")
	err = acc.AssertRoot(unknownHash, state.Root)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no accumulated state")
}

// TestAccumulator_DeleteIsIdempotent
func TestAccumulator_DeleteIsIdempotent(t *testing.T) {
	acc := NewAccumulator()
	hash := common.HexToHash("0xab")
	acc.Put(hash, EmptyAccumulatorState())
	require.Equal(t, 1, acc.Len())

	acc.Delete(hash)
	require.Equal(t, 0, acc.Len())
	acc.Delete(hash) // second call — must not panic
	require.Equal(t, 0, acc.Len())

	_, ok := acc.Get(hash)
	require.False(t, ok)
}

// TestAccumulator_RootMatchesEncodingVectors is the cross-implementation
// lock: the accumulator must produce the same root as the locked TS
// reference vectors used by the rest of the AGNT2 stack. The 3-leaf vector
// is the canonical one tested in op-geth's agnt2MMR_test.go and in the
// existing encoding_vectors_test.go in this package.
func TestAccumulator_RootMatchesEncodingVectors(t *testing.T) {
	leaf0 := encodeLeaf("test-wf-001", "step-1", "worker-a", big.NewInt(1000), [32]byte{})
	leaf1 := encodeLeaf("test-wf-001", "step-2", "worker-b", big.NewInt(2000), leaf0)
	leaf2 := encodeLeaf("test-wf-001", "step-3", "worker-c", big.NewInt(3000), leaf1)

	state := IncrementalAppend(EmptyAccumulatorState(), [][32]byte{leaf0, leaf1, leaf2})
	got := fmt.Sprintf("0x%x", state.Root[:])
	require.Equal(t, "0xd54c19717603e20fbf82ff44e90eafd7d1ad14ef4d7811f8802cc3c078cc86c3", got,
		"accumulator MMR root must match locked v1_3step reference vector")
	require.Equal(t, uint64(3), state.Count)
}

// makeLeaves produces n distinct deterministic leaves for fold tests.
func makeLeaves(n int) [][32]byte {
	out := make([][32]byte, 0, n)
	for i := 0; i < n; i++ {
		var l [32]byte
		copy(l[:], crypto.Keccak256([]byte(fmt.Sprintf("leaf-%d", i))))
		out = append(out, l)
	}
	return out
}

func makeLeavesWithSeed(n int, seed string) [][32]byte {
	out := make([][32]byte, 0, n)
	for i := 0; i < n; i++ {
		var l [32]byte
		copy(l[:], crypto.Keccak256([]byte(fmt.Sprintf("%s-%d", seed, i))))
		out = append(out, l)
	}
	return out
}
