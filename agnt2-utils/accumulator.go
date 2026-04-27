package agnt2utils

import (
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// AccumulatorState is the per-block snapshot of the AGNT2 interaction MMR
// after that block has been folded in. Persisting (peaks, count) keyed by
// block hash — instead of just the cumulative root — is the load-bearing
// reorg-correctness property: when the canonical chain switches to a
// different fork branch, op-node can replay incremental MMR appends from
// the divergence-point's snapshot rather than re-folding from genesis.
//
// The shape is intentionally minimal so the same struct can be serialized
// to disk later (Week 12+) without schema churn — a Phase 7 in-memory
// implementation is sufficient for the current single-node devnet/test
// path; durability is layered on later.
type AccumulatorState struct {
	// Peaks holds the right-to-left peak hashes of the cumulative MMR
	// after this block. len(Peaks) == popcount(Count) — one peak per set
	// bit in the leaf count, consistent with the bit-decomposition fold
	// used by core/vm/agnt2_mmr.go and core/types/agnt2_header.go.
	Peaks [][32]byte
	// Count is the cumulative leaf count after this block. Required for
	// the next-block fold to pick the correct peak-decomposition.
	Count uint64
	// Root is the cached MMR root for this block (the right-to-left peak
	// combine of Peaks). Stored so callers don't have to re-bag peaks on
	// every read; trivially recomputed via foldPeaks(Peaks).
	Root common.Hash
}

// Accumulator stores AccumulatorState snapshots keyed by L2 block hash.
// Reorg-safe: the snapshot for an abandoned fork branch remains queryable
// as long as the caller still holds the block hash, so op-node can roll
// back to the divergence-point and re-fold from there. Pruning of stale
// branches is the caller's responsibility (the accumulator does not own
// canonicality).
//
// Thread-safety: sync.RWMutex around the map. Reads (look up state for a
// parent block hash before folding the next block) outnumber writes
// (snapshot after each new block) by at least the order of intra-block
// log validations.
type Accumulator struct {
	mu     sync.RWMutex
	states map[common.Hash]AccumulatorState
}

// NewAccumulator returns an empty in-memory accumulator. The genesis L2
// block snapshot (empty MMR, count 0, root keccak256("")) is NOT inserted
// automatically — callers Put() it explicitly so the accumulator stays
// agnostic to the chain's genesis layout.
func NewAccumulator() *Accumulator {
	return &Accumulator{states: make(map[common.Hash]AccumulatorState)}
}

// Put records the cumulative MMR snapshot after the block with the given
// hash. Overwrites any prior entry for the same hash (idempotent on
// re-application of identical state, useful for crash-recovery replays).
func (a *Accumulator) Put(blockHash common.Hash, state AccumulatorState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cpyPeaks := make([][32]byte, len(state.Peaks))
	copy(cpyPeaks, state.Peaks)
	a.states[blockHash] = AccumulatorState{
		Peaks: cpyPeaks,
		Count: state.Count,
		Root:  state.Root,
	}
}

// Get returns the cumulative MMR snapshot for the block with the given
// hash and a presence flag. The returned slice is a copy; mutating it
// does not affect the accumulator.
func (a *Accumulator) Get(blockHash common.Hash) (AccumulatorState, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	s, ok := a.states[blockHash]
	if !ok {
		return AccumulatorState{}, false
	}
	cpyPeaks := make([][32]byte, len(s.Peaks))
	copy(cpyPeaks, s.Peaks)
	return AccumulatorState{Peaks: cpyPeaks, Count: s.Count, Root: s.Root}, true
}

// Delete drops the snapshot for a block hash — useful for pruning
// abandoned fork branches once they're known unreachable. Idempotent on
// missing keys.
func (a *Accumulator) Delete(blockHash common.Hash) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.states, blockHash)
}

// Len reports the number of snapshots currently held — primarily for
// tests and observability.
func (a *Accumulator) Len() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.states)
}

// AppendLeavesToParent computes the next AccumulatorState by folding the
// given leaves on top of the snapshot keyed by parentHash. If the parent
// snapshot is absent, the fold starts from empty (caller-detectable via
// the returned bool == false).
//
// The algorithm: append each leaf as a new size-1 peak, then collapse
// adjacent equal-height peaks via keccak256(left ++ right) until no two
// adjacent peaks share a height. The peak heights are inferred from the
// post-append count via the bit-decomposition rule: a leaf-count of N has
// one peak per set bit in N, with heights = bit positions. After all
// leaves are appended, the cumulative root is the right-to-left bag of
// the peaks (matching foldMMR in core/types/agnt2_header.go).
func (a *Accumulator) AppendLeavesToParent(parentHash common.Hash, leaves [][32]byte) (AccumulatorState, bool) {
	parent, ok := a.Get(parentHash)
	state := IncrementalAppend(parent, leaves)
	return state, ok
}

// IncrementalAppend folds `leaves` onto the prior cumulative state. Pure
// function — no Accumulator dependency, useful for tests and for callers
// who want to compute next-state without persisting.
func IncrementalAppend(prior AccumulatorState, leaves [][32]byte) AccumulatorState {
	peaks := make([][32]byte, len(prior.Peaks))
	copy(peaks, prior.Peaks)
	count := prior.Count
	for _, leaf := range leaves {
		peaks = appendPeak(peaks, count, leaf)
		count++
	}
	root := bagPeaks(peaks)
	return AccumulatorState{Peaks: peaks, Count: count, Root: root}
}

// appendPeak inserts `leaf` into the peak slice and collapses adjacent
// equal-height peaks. The post-append count tells us how many trailing
// 1-bits to collapse: if the new count has k trailing zeros (after the
// new leaf flipped the lowest 0-bit), we collapse k+1 peaks into one
// (the +1 accounts for the new leaf itself joining its height-0 sibling).
//
// We compute this implicitly: after appending the new leaf, examine the
// pre-append count's trailing 1-bits. A trailing 1-bit at position i in
// the pre-append count means there is currently a height-i peak that
// will combine with the new leaf-derived peak at height i.
func appendPeak(peaks [][32]byte, preCount uint64, leaf [32]byte) [][32]byte {
	peaks = append(peaks, leaf)
	// Collapse from right while the lowest bit of preCount is 1
	for preCount&1 == 1 {
		n := len(peaks)
		var combined [64]byte
		copy(combined[0:32], peaks[n-2][:])
		copy(combined[32:64], peaks[n-1][:])
		var h [32]byte
		copy(h[:], crypto.Keccak256(combined[:]))
		peaks = peaks[:n-2]
		peaks = append(peaks, h)
		preCount >>= 1
	}
	return peaks
}

// bagPeaks combines peaks right-to-left via keccak256(left ++ right).
// Empty input returns keccak256(""). Single-peak input returns that peak
// unchanged. Mirrors the right-to-left fold in core/types/agnt2_header.go.
func bagPeaks(peaks [][32]byte) common.Hash {
	if len(peaks) == 0 {
		var h common.Hash
		copy(h[:], crypto.Keccak256([]byte{}))
		return h
	}
	root := peaks[len(peaks)-1]
	for i := len(peaks) - 2; i >= 0; i-- {
		var combined [64]byte
		copy(combined[0:32], peaks[i][:])
		copy(combined[32:64], root[:])
		copy(root[:], crypto.Keccak256(combined[:]))
	}
	var out common.Hash
	copy(out[:], root[:])
	return out
}

// EmptyAccumulatorState returns the genesis-equivalent snapshot used as
// the seed for the first AGNT2 block. Convenience for callers wiring up
// the genesis entry.
func EmptyAccumulatorState() AccumulatorState {
	return AccumulatorState{
		Peaks: nil,
		Count: 0,
		Root:  bagPeaks(nil),
	}
}
