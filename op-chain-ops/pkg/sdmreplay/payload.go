package sdmreplay

import (
	"fmt"

	"github.com/ethereum/go-ethereum/rlp"
)

const SDMTxType = 0x7d

// SDMGasEntry is one per-transaction refund entry inside the SDM portion of a post-exec payload.
type SDMGasEntry struct {
	Index     uint64 `json:"index"`
	GasRefund uint64 `json:"gas_refund"`
}

// PostExecPayload is the decoded RLP payload carried by the synthetic post-exec tx.
// Today this contains the SDM gas refund entries and the L2 block number the payload is anchored to.
// Older payloads may omit BlockNumber.
type PostExecPayload struct {
	Version          uint64        `json:"version"`
	BlockNumber      uint64        `json:"block_number,omitempty"`
	GasRefundEntries []SDMGasEntry `json:"gas_refund_entries"`
}

// GasRefundForIndex returns the refund for the given block tx index.
func (p *PostExecPayload) GasRefundForIndex(index uint64) (uint64, bool) {
	if p == nil {
		return 0, false
	}
	for _, entry := range p.GasRefundEntries {
		if entry.Index == index {
			return entry.GasRefund, true
		}
	}
	return 0, false
}

// DecodePayload decodes an RLP-encoded post-exec payload from the post-exec tx input.
func DecodePayload(input []byte) (*PostExecPayload, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("empty post-exec payload")
	}

	var payload PostExecPayload
	if err := rlp.DecodeBytes(input, &payload); err == nil {
		return &payload, nil
	}

	// Backward compatibility for older payloads that encoded only version + refund entries.
	var legacy struct {
		Version          uint64
		GasRefundEntries []SDMGasEntry
	}
	if err := rlp.DecodeBytes(input, &legacy); err != nil {
		return nil, fmt.Errorf("decode post-exec payload: %w", err)
	}
	return &PostExecPayload{
		Version:          legacy.Version,
		GasRefundEntries: legacy.GasRefundEntries,
	}, nil
}
