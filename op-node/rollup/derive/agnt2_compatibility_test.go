package derive

import (
	"bytes"
	"context"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/stretchr/testify/require"

	"github.com/ethereum-optimism/optimism/op-node/rollup"
	"github.com/ethereum-optimism/optimism/op-service/eth"
)

// mockAttributesBuilder is a simple mock to provide PayloadAttributes
type mockAttributesBuilder struct {
	attrs *eth.PayloadAttributes
}

func (m *mockAttributesBuilder) PreparePayloadAttributes(ctx context.Context, l2Parent eth.L2BlockRef, epoch eth.BlockID) (*eth.PayloadAttributes, error) {
	return m.attrs, nil
}

// mockSingularBatchProvider provides the test batch
type mockSingularBatchProvider struct {
	batch *SingularBatch
}

func (m *mockSingularBatchProvider) NextBatch(ctx context.Context, parent eth.L2BlockRef) (*SingularBatch, bool, error) {
	return m.batch, false, nil
}
func (m *mockSingularBatchProvider) Origin() eth.L1BlockRef { return eth.L1BlockRef{} }
func (m *mockSingularBatchProvider) FlushChannel()          {}
func (m *mockSingularBatchProvider) Reset(ctx context.Context, base eth.L1BlockRef, cfg eth.SystemConfig) error {
	return nil
}

func TestAGNT2SingularBatchEncoding(t *testing.T) {
	interactionRoot := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	payloadCommitment := hexutil.Bytes{0xde, 0xad, 0xbe, 0xef}
	daTarget := uint8(1)

	// Create a dummy SingularBatch with the new AGNT2 fields populated
	original := &SingularBatch{
		ParentHash:        common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111"),
		EpochNum:          12345,
		EpochHash:         common.HexToHash("0x2222222222222222222222222222222222222222222222222222222222222222"),
		Timestamp:         1680000000,
		Transactions:      []hexutil.Bytes{{0x01, 0x02, 0x03}, {0x04, 0x05}},
		InteractionRoot:   &interactionRoot,
		PayloadCommitment: &payloadCommitment,
		DATarget:          &daTarget,
	}

	// 1. Encode the batch to bytes
	var buf bytes.Buffer
	err := rlp.Encode(&buf, original)
	require.NoError(t, err, "failed to encode SingularBatch")

	encodedBytes := buf.Bytes()
	require.NotEmpty(t, encodedBytes, "encoded bytes should not be empty")

	// 2. Decode the bytes back into a new SingularBatch
	decoded := new(SingularBatch)
	err = rlp.DecodeBytes(encodedBytes, decoded)
	require.NoError(t, err, "failed to decode SingularBatch")

	// 3. Assert all fields match exactly, ensuring byte stability and proper serialization
	require.Equal(t, original.ParentHash, decoded.ParentHash, "ParentHash mismatch")
	require.Equal(t, original.Timestamp, decoded.Timestamp, "Timestamp mismatch")
	require.Equal(t, original.Transactions, decoded.Transactions, "Transactions mismatch")
	require.Equal(t, original.InteractionRoot, decoded.InteractionRoot, "InteractionRoot mismatch")
	require.Equal(t, original.PayloadCommitment, decoded.PayloadCommitment, "PayloadCommitment mismatch")
	require.Equal(t, original.DATarget, decoded.DATarget, "DATarget mismatch")
}

func TestAGNT2AttributesDerivation(t *testing.T) {
	interactionRoot := common.HexToHash("0x3333333333333333333333333333333333333333333333333333333333333333")
	payloadCommitment := hexutil.Bytes{0xde, 0xad, 0xbe, 0xef}
	daTarget := uint8(1)

	parentHash := common.HexToHash("0x1111111111111111111111111111111111111111111111111111111111111111")
	parentTime := uint64(1680000000)
	blockTime := uint64(2)

	batch := &SingularBatch{
		ParentHash:        parentHash,
		Timestamp:         parentTime + blockTime,
		Transactions:      []hexutil.Bytes{{0x01, 0x02, 0x03}},
		InteractionRoot:   &interactionRoot,
		PayloadCommitment: &payloadCommitment,
		DATarget:          &daTarget,
	}

	builder := &mockAttributesBuilder{
		attrs: &eth.PayloadAttributes{
			Transactions: []hexutil.Bytes{},
		},
	}
	provider := &mockSingularBatchProvider{batch: batch}

	cfg := &rollup.Config{BlockTime: blockTime}
	queue := NewAttributesQueue(log.New(), cfg, builder, provider)

	parent := eth.L2BlockRef{
		Hash: parentHash,
		Time: parentTime,
	}

	// Run derivation
	attrsWithParent, err := queue.NextAttributes(context.Background(), parent)
	require.NoError(t, err, "failed to derive attributes")

	// Assert that AGNT2 fields are successfully wired into the PayloadAttributes
	attrs := attrsWithParent.Attributes
	require.NotNil(t, attrs.InteractionRoot, "InteractionRoot should not be nil")
	require.Equal(t, interactionRoot, *attrs.InteractionRoot, "InteractionRoot mismatch")

	require.NotNil(t, attrs.PayloadCommitment, "PayloadCommitment should not be nil")
	require.Equal(t, payloadCommitment, *attrs.PayloadCommitment, "PayloadCommitment mismatch")

	require.NotNil(t, attrs.DATarget, "DATarget should not be nil")
	require.Equal(t, daTarget, *attrs.DATarget, "DATarget mismatch")
}
