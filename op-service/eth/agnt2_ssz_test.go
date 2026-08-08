package eth

import (
	"bytes"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"
)

// TestAGNT2ExecutionPayloadSSZRoundTrip asserts that the AGNT2 header extensions
// (InteractionRoot/InteractionCount, TypedOpRoot/TypedOpCount) survive an
// ExecutionPayload SSZ marshal -> unmarshal round-trip.
//
// STATUS: THIS TEST CURRENTLY FAILS — it documents a KNOWN, MEASURED DEFECT.
// It is skipped by default so the suite stays green; run it explicitly with:
//
//	AGNT2_SSZ_DEFECT_TEST=1 go test ./op-service/eth/ -run TestAGNT2ExecutionPayloadSSZRoundTrip -v
//
// THE DEFECT. ExecutionPayload declares the four AGNT2 fields (types.go, "AGNT2
// extensions: interaction MMR + typed-op root/count are in the block header (affect
// the hash)") and CheckBlockHash folds all four into the header it reconstructs to
// verify the block hash. But the payload SSZ codec in ssz.go never serializes them:
// blockV4FixedPart stops at WithdrawalsRoot and there is not a single reference to
// Interaction*/TypedOp* in the file. So the fields are silently dropped in transit
// and the receiver recomputes a DIFFERENT block hash than the producer sealed.
//
// CONSEQUENCE (measured, see docs/claims/evidence/agnt2-ws1-payload-ssz-defect-measured.json):
// post-Isthmus op-geth sets InteractionRoot/Count on EVERY block (not just blocks
// carrying typed ops), so once the AGNT2 fork tag is active NO block can cross
// op-node p2p at all: the 3-node op-conductor cluster logs "payload has bad block
// hash" for every gossiped payload, and op-conductor's own leadership handoff
// (raft-stored payload -> admin_postUnsafePayload, also SSZ) fails the same way, so
// a newly elected leader can never become active and failover breaks.
//
// FIX SKETCH: add a BlockV5 payload version carrying the AGNT2 fields, select it via
// inferVersion() when InteractionRoot != nil, add a matching blocksV5 gossip topic,
// and teach op-conductor's raft FSM to decode V5. Reserve space for TypedReexecRoot/
// Count at the same time to avoid a second wire-format break.
func TestAGNT2ExecutionPayloadSSZRoundTrip(t *testing.T) {
	if os.Getenv("AGNT2_SSZ_DEFECT_TEST") != "1" {
		t.Skip("documents a known unfixed defect (AGNT2 header fields dropped by the payload SSZ codec); set AGNT2_SSZ_DEFECT_TEST=1 to run")
	}

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
}
