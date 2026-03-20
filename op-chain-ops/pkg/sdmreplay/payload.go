package sdmreplay

import (
	"fmt"

	"github.com/ethereum/go-ethereum/rlp"
)

const SDMTxType = 0x7d

// SDMGasEntry is one per-transaction refund entry inside an SDM payload.
type SDMGasEntry struct {
	Index     uint64 `json:"index"`
	GasRefund uint64 `json:"gas_refund"`
}

// SDMPayload is the decoded RLP payload carried by the synthetic SDM tx.
type SDMPayload struct {
	Version uint64        `json:"version"`
	Entries []SDMGasEntry `json:"entries"`
}

// GasRefundForIndex returns the refund for the given block tx index.
func (p *SDMPayload) GasRefundForIndex(index uint64) (uint64, bool) {
	if p == nil {
		return 0, false
	}
	for _, entry := range p.Entries {
		if entry.Index == index {
			return entry.GasRefund, true
		}
	}
	return 0, false
}

// DecodePayload decodes an RLP-encoded SDM payload from the SDM tx input.
func DecodePayload(input []byte) (*SDMPayload, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("empty SDM payload")
	}
	var payload SDMPayload
	if err := rlp.DecodeBytes(input, &payload); err != nil {
		return nil, fmt.Errorf("decode SDM payload: %w", err)
	}
	return &payload, nil
}
