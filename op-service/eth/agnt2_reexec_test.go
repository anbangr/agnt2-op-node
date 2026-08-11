package eth

import (
	"bytes"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// reexecTestHeader builds the header op-geth actually seals for an AGNT2 typed-op block. All six
// AGNT2 header fields are set, because B2' Stage 2 stamps the re-exec pair on any Isthmus block
// whose typed re-execution fold yields a leaf -- and FoldTypedReexecRoot appends one for EVERY
// INVOKE with no skip path, so a single 0x7A is enough.
func reexecTestHeader(withReexec bool) *types.Header {
	interactionRoot := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	interactionCount := uint64(2)
	typedOpRoot := common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222")
	typedOpCount := uint64(2)
	withdrawalsRoot := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	beaconRoot := common.HexToHash("0x4444444444444444444444444444444444444444444444444444444444444444")

	h := &types.Header{
		ParentHash:       common.HexToHash("0xaaaa"),
		UncleHash:        types.EmptyUncleHash,
		Coinbase:         common.Address{0x01},
		Root:             common.HexToHash("0xbbbb"),
		TxHash:           types.EmptyTxsHash,
		ReceiptHash:      common.HexToHash("0xcccc"),
		Bloom:            types.Bloom{},
		Difficulty:       common.Big0,
		Number:           common.Big1,
		GasLimit:         30_000_000,
		GasUsed:          21_000,
		Time:             1_700_000_000,
		Extra:            []byte{},
		MixDigest:        common.HexToHash("0xdddd"),
		BaseFee:          common.Big1,
		WithdrawalsHash:  &withdrawalsRoot,
		BlobGasUsed:      new(uint64),
		ExcessBlobGas:    new(uint64),
		ParentBeaconRoot: &beaconRoot,
		// An Isthmus block (WithdrawalsRoot set) always carries the empty requests hash,
		// and CheckBlockHash reconstructs it that way, so the fixture must match or the
		// comparison fails for a reason unrelated to the re-exec fields.
		RequestsHash:     &types.EmptyRequestsHash,
		InteractionRoot:  &interactionRoot,
		InteractionCount: &interactionCount,
		TypedOpRoot:      &typedOpRoot,
		TypedOpCount:     &typedOpCount,
	}
	if withReexec {
		reexecRoot := common.HexToHash("0xfeed0000000000000000000000000000000000000000000000000000000000ff")
		reexecCount := uint64(3)
		h.TypedReexecRoot = &reexecRoot
		h.TypedReexecCount = &reexecCount
	}
	return h
}

func reexecTestEnvelope(h *types.Header) *ExecutionPayloadEnvelope {
	baseFee, _ := uint256.FromBig(h.BaseFee)
	return &ExecutionPayloadEnvelope{
		ParentBeaconBlockRoot: h.ParentBeaconRoot,
		ExecutionPayload: &ExecutionPayload{
			ParentHash:       h.ParentHash,
			FeeRecipient:     h.Coinbase,
			StateRoot:        Bytes32(h.Root),
			ReceiptsRoot:     Bytes32(h.ReceiptHash),
			LogsBloom:        Bytes256(h.Bloom),
			PrevRandao:       Bytes32(h.MixDigest),
			BlockNumber:      Uint64Quantity(h.Number.Uint64()),
			GasLimit:         Uint64Quantity(h.GasLimit),
			GasUsed:          Uint64Quantity(h.GasUsed),
			Timestamp:        Uint64Quantity(h.Time),
			ExtraData:        h.Extra,
			BaseFeePerGas:    Uint256Quantity(*baseFee),
			BlockHash:        h.Hash(),
			Transactions:     []Data{},
			Withdrawals:      &types.Withdrawals{},
			BlobGasUsed:      (*Uint64Quantity)(h.BlobGasUsed),
			ExcessBlobGas:    (*Uint64Quantity)(h.ExcessBlobGas),
			WithdrawalsRoot:  h.WithdrawalsHash,
			InteractionRoot:  h.InteractionRoot,
			InteractionCount: (*Uint64Quantity)(h.InteractionCount),
			TypedOpRoot:      h.TypedOpRoot,
			TypedOpCount:     (*Uint64Quantity)(h.TypedOpCount),
			TypedReexecRoot:  h.TypedReexecRoot,
			TypedReexecCount: (*Uint64Quantity)(h.TypedReexecCount),
		},
	}
}

// TestAGNT2TypedReexecIsHashRelevant is the premise the rest of this file rests on: if the
// re-exec pair did not move the block hash there would be nothing for op-node to carry.
func TestAGNT2TypedReexecIsHashRelevant(t *testing.T) {
	require.NotEqual(t, reexecTestHeader(false).Hash(), reexecTestHeader(true).Hash(),
		"TypedReexecRoot/Count must be folded into the block hash")
}

// TestAGNT2CheckBlockHashWithTypedReexec is the regression guard for the CL-side gap.
//
// op-geth seals a six-AGNT2-field header. Before this fix op-node's ExecutionPayload had no
// field for the re-exec pair, so CheckBlockHash rebuilt a four-field header, derived a different
// hash, and gossip rejected every typed-op block. Nothing downstream would have caught it: the
// hash check IS the check.
func TestAGNT2CheckBlockHashWithTypedReexec(t *testing.T) {
	h := reexecTestHeader(true)
	actual, ok := reexecTestEnvelope(h).CheckBlockHash()
	require.True(t, ok,
		"op-node must reproduce the hash of a block carrying TypedReexecRoot/Count (got %s, want %s)",
		actual, h.Hash())
	require.Equal(t, h.Hash(), actual)

	// A block with no typed ops carries no re-exec fields, and must still hash correctly --
	// the absent case has to stay distinct from the present one rather than being coerced to
	// zeroes.
	h2 := reexecTestHeader(false)
	actual2, ok2 := reexecTestEnvelope(h2).CheckBlockHash()
	require.True(t, ok2)
	require.Equal(t, h2.Hash(), actual2)
	require.NotEqual(t, actual, actual2)
}

// TestAGNT2ExecutionPayloadSSZRoundTripWithReexec covers the wire format. The fields have to
// survive marshal/unmarshal, and -- the subtle half -- an ABSENT pair must come back as nil
// rather than as a zero root, because the SSZ buffer is pooled and would otherwise carry a
// previous payload's bytes.
func TestAGNT2ExecutionPayloadSSZRoundTripWithReexec(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withReexec bool
	}{
		{"with typed re-exec fields", true},
		{"without typed re-exec fields", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := reexecTestEnvelope(reexecTestHeader(tc.withReexec))
			var buf bytes.Buffer
			_, err := env.ExecutionPayload.MarshalSSZ(&buf)
			require.NoError(t, err)

			var got ExecutionPayload
			require.NoError(t, got.UnmarshalSSZ(env.ExecutionPayload.inferVersion(),
				uint32(buf.Len()), bytes.NewReader(buf.Bytes())))

			if tc.withReexec {
				require.NotNil(t, got.TypedReexecRoot, "re-exec root must survive the round trip")
				require.Equal(t, *env.ExecutionPayload.TypedReexecRoot, *got.TypedReexecRoot)
				require.NotNil(t, got.TypedReexecCount)
				require.Equal(t, *env.ExecutionPayload.TypedReexecCount, *got.TypedReexecCount)
			} else {
				require.Nil(t, got.TypedReexecRoot,
					"an absent re-exec root must decode back to nil, not to the zero hash")
				require.Nil(t, got.TypedReexecCount)
			}
			// The previously-wired fields must be unaffected by the new ones.
			require.Equal(t, *env.ExecutionPayload.TypedOpRoot, *got.TypedOpRoot)
			require.Equal(t, *env.ExecutionPayload.InteractionRoot, *got.InteractionRoot)
		})
	}
}
