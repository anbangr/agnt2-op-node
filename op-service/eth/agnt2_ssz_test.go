package eth

import (
	"bytes"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

// TestAGNT2ExecutionPayloadSSZRoundTrip asserts that the AGNT2 header extensions
// (InteractionRoot/InteractionCount, TypedOpRoot/TypedOpCount) survive an
// ExecutionPayload SSZ marshal -> unmarshal round-trip.
//
// REGRESSION GUARD. This test was originally added RED, to document a measured defect:
// ExecutionPayload declares the four AGNT2 fields as hash-affecting and CheckBlockHash
// folds all four into the header it reconstructs, but the payload SSZ codec never
// serialized them — so they were dropped in transit and the receiver recomputed a
// DIFFERENT block hash than the producer sealed. Because op-geth stamps
// InteractionRoot/Count on EVERY block once the AGNT2 (Isthmus) fork tag is active, no
// block could cross op-node p2p at all: the 3-node op-conductor cluster logged "payload
// has bad block hash" for every gossiped payload, and op-conductor's leadership handoff
// failed the same way, so failover broke.
// (Measured: docs/claims/evidence/agnt2-ws1-payload-ssz-defect-measured.json.)
//
// It is now GREEN, fixed by the BlockV5 payload version in ssz.go. Keep it running by
// default: it is the cheapest guard against a silent re-break of the block-hash
// commitment, including an off-by-N in the fixed-part offset arithmetic.
func TestAGNT2ExecutionPayloadSSZRoundTrip(t *testing.T) {
	interactionRoot := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	typedOpRoot := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	withdrawalsRoot := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	interactionCount := Uint64Quantity(7)
	typedOpCount := Uint64Quantity(3)
	blobGasUsed := Uint64Quantity(0)
	excessBlobGas := Uint64Quantity(0)

	payload := &ExecutionPayload{
		BlobGasUsed:      &blobGasUsed,
		ExcessBlobGas:    &excessBlobGas,
		WithdrawalsRoot:  &withdrawalsRoot,
		Withdrawals:      &types.Withdrawals{},
		InteractionRoot:  &interactionRoot,
		InteractionCount: &interactionCount,
		TypedOpRoot:      &typedOpRoot,
		TypedOpCount:     &typedOpCount,
	}

	var buf bytes.Buffer
	_, err := payload.MarshalSSZ(&buf)
	require.NoError(t, err)

	var got ExecutionPayload
	require.NoError(t, got.UnmarshalSSZ(payload.inferVersion(), uint32(buf.Len()), bytes.NewReader(buf.Bytes())))

	// Each of these fails today: every field comes back nil.
	require.NotNil(t, got.InteractionRoot, "InteractionRoot must survive the SSZ round-trip (it is folded into the block hash)")
	require.NotNil(t, got.InteractionCount, "InteractionCount must survive the SSZ round-trip")
	require.NotNil(t, got.TypedOpRoot, "TypedOpRoot must survive the SSZ round-trip (it is folded into the block hash)")
	require.NotNil(t, got.TypedOpCount, "TypedOpCount must survive the SSZ round-trip")
	require.Equal(t, interactionRoot, *got.InteractionRoot)
	require.Equal(t, typedOpRoot, *got.TypedOpRoot)
	require.Equal(t, interactionCount, *got.InteractionCount)
	require.Equal(t, typedOpCount, *got.TypedOpCount)
	require.Equal(t, BlockV5, payload.inferVersion(), "a payload carrying AGNT2 fields must encode as V5")
}

// TestAGNT2ExecutionPayloadSSZRoundTripNoTypedOps covers the other half of the AGNT2
// presence rule: op-geth leaves TypedOpRoot/Count nil on blocks that carry no typed ops
// (it only sets them when count > 0), while InteractionRoot/Count are still stamped on
// every post-Isthmus block. Nil-ness is hash-relevant — CheckBlockHash folds these
// pointers into the reconstructed header — so a block with no typed ops must decode back
// with TypedOpRoot/Count STILL NIL, not as a zero-valued pointer.
func TestAGNT2ExecutionPayloadSSZRoundTripNoTypedOps(t *testing.T) {
	interactionRoot := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	withdrawalsRoot := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	interactionCount := Uint64Quantity(0)
	blobGasUsed := Uint64Quantity(0)
	excessBlobGas := Uint64Quantity(0)

	payload := &ExecutionPayload{
		BlobGasUsed:      &blobGasUsed,
		ExcessBlobGas:    &excessBlobGas,
		WithdrawalsRoot:  &withdrawalsRoot,
		Withdrawals:      &types.Withdrawals{},
		InteractionRoot:  &interactionRoot,
		InteractionCount: &interactionCount,
		// TypedOpRoot / TypedOpCount intentionally nil
	}
	require.Equal(t, BlockV5, payload.inferVersion())

	var buf bytes.Buffer
	_, err := payload.MarshalSSZ(&buf)
	require.NoError(t, err)

	var got ExecutionPayload
	require.NoError(t, got.UnmarshalSSZ(payload.inferVersion(), uint32(buf.Len()), bytes.NewReader(buf.Bytes())))

	require.NotNil(t, got.InteractionRoot)
	require.Equal(t, interactionRoot, *got.InteractionRoot)
	require.NotNil(t, got.InteractionCount)
	require.Equal(t, interactionCount, *got.InteractionCount)
	require.Nil(t, got.TypedOpRoot, "TypedOpRoot must stay nil when the block carries no typed ops (nil-ness is hash-relevant)")
	require.Nil(t, got.TypedOpCount, "TypedOpCount must stay nil when the block carries no typed ops")
}
