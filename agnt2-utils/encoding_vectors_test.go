package agnt2utils

import (
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"
)

// encodeLeaf computes a single AGNT2 MMR leaf hash using tight 160-byte packing:
// keccak256(wfIdHash[32] ++ stepIdHash[32] ++ agentRoleHash[32] ++ payout[32] ++ prevLeafHash[32])
// This matches the TypeScript solidityPacked implementation in benchmarks/src/trace.ts.
func encodeLeaf(workflowId, stepId, agentRole string, payout *big.Int, prevLeafHash [32]byte) [32]byte {
	var buf []byte
	buf = append(buf, crypto.Keccak256Hash([]byte(workflowId)).Bytes()...)
	buf = append(buf, crypto.Keccak256Hash([]byte(stepId)).Bytes()...)
	buf = append(buf, crypto.Keccak256Hash([]byte(agentRole)).Bytes()...)

	padded := make([]byte, 32)
	payout.FillBytes(padded)
	buf = append(buf, padded...)
	buf = append(buf, prevLeafHash[:]...)

	return crypto.Keccak256Hash(buf)
}

// buildTree recursively hashes a power-of-2 slice of leaves into a single root.
func buildTree(nodes [][32]byte) [32]byte {
	if len(nodes) == 1 {
		return nodes[0]
	}
	var next [][32]byte
	for i := 0; i < len(nodes); i += 2 {
		if i+1 < len(nodes) {
			next = append(next, crypto.Keccak256Hash(append(nodes[i][:], nodes[i+1][:]...)))
		} else {
			next = append(next, nodes[i])
		}
	}
	return buildTree(next)
}

// mmrGetRoot computes the MMR root matching the TypeScript MMR.getRoot() in trace.ts.
func mmrGetRoot(leaves [][32]byte) [32]byte {
	if len(leaves) == 0 {
		return crypto.Keccak256Hash([]byte{})
	}

	var peaks [][32]byte
	n := len(leaves)
	offset := 0
	for bit := 31; bit >= 0; bit-- {
		size := 1 << bit
		if n&size != 0 {
			peaks = append(peaks, buildTree(leaves[offset:offset+size]))
			offset += size
		}
	}

	// Combine peaks right-to-left: matches TS `peaks[i] ++ root` order.
	root := peaks[len(peaks)-1]
	for i := len(peaks) - 2; i >= 0; i-- {
		root = crypto.Keccak256Hash(append(peaks[i][:], root[:]...))
	}
	return root
}

// TestEncodingVectors verifies byte-exact parity with the TypeScript reference
// hashes locked on 2026-04-26 (Week 9 C3 fix). Zero mismatches required.
func TestEncodingVectors(t *testing.T) {
	t.Run("Vector1_SingleLeaf", func(t *testing.T) {
		leaf := encodeLeaf("test-wf-001", "step-1", "worker-a", big.NewInt(1000), [32]byte{})
		got := fmt.Sprintf("0x%x", leaf[:])
		require.Equal(t, "0x709a0076c0f5b8310f6625008f99159e787b75e2bf268af2edebdc5f25b1de2e", got,
			"MISMATCH: Vector1_SingleLeaf")
	})

	t.Run("Vector1_ThreeStepRoot", func(t *testing.T) {
		leaf0 := encodeLeaf("test-wf-001", "step-1", "worker-a", big.NewInt(1000), [32]byte{})
		leaf1 := encodeLeaf("test-wf-001", "step-2", "worker-b", big.NewInt(2000), leaf0)
		leaf2 := encodeLeaf("test-wf-001", "step-3", "worker-c", big.NewInt(3000), leaf1)
		root := mmrGetRoot([][32]byte{leaf0, leaf1, leaf2})
		got := fmt.Sprintf("0x%x", root[:])
		require.Equal(t, "0xd54c19717603e20fbf82ff44e90eafd7d1ad14ef4d7811f8802cc3c078cc86c3", got,
			"MISMATCH: Vector1_ThreeStepRoot")
	})

	t.Run("Vector2_EmptyRoot", func(t *testing.T) {
		root := mmrGetRoot([][32]byte{})
		got := fmt.Sprintf("0x%x", root[:])
		require.Equal(t, "0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470", got,
			"MISMATCH: Vector2_EmptyRoot")
	})

	t.Run("Vector3_MaxPayout", func(t *testing.T) {
		maxUint256 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
		leaf := encodeLeaf("test-wf-overflow", "step-1", "worker-a", maxUint256, [32]byte{})
		got := fmt.Sprintf("0x%x", leaf[:])
		require.Equal(t, "0x6ec4babc86d7fe8ac9dba9595cdc2273eb87d9588de683266a84f883373abb66", got,
			"MISMATCH: Vector3_MaxPayout")
	})

	// Vector 4: 5-leaf MMR root. Decomposes to peaks 4+1, exercising multi-peak
	// bagging that the 3-leaf vector (peaks 2+1, only one fold step) does not
	// cover. Added in /review specialist pass alongside TS + Solidity equivalents.
	t.Run("Vector4_5StepMultiPeak", func(t *testing.T) {
		leaf0 := encodeLeaf("test-wf-005", "step-1", "worker-a", big.NewInt(1000), [32]byte{})
		leaf1 := encodeLeaf("test-wf-005", "step-2", "worker-b", big.NewInt(2000), leaf0)
		leaf2 := encodeLeaf("test-wf-005", "step-3", "worker-c", big.NewInt(3000), leaf1)
		leaf3 := encodeLeaf("test-wf-005", "step-4", "worker-d", big.NewInt(4000), leaf2)
		leaf4 := encodeLeaf("test-wf-005", "step-5", "worker-e", big.NewInt(5000), leaf3)
		root := mmrGetRoot([][32]byte{leaf0, leaf1, leaf2, leaf3, leaf4})
		got := fmt.Sprintf("0x%x", root[:])
		require.Equal(t, "0xe34cda67eaf574138a02ab6ea87fd1ec55f8e3c09545f2c7c44edb8365316c91", got,
			"MISMATCH: Vector4_5StepMultiPeak")
	})
}
