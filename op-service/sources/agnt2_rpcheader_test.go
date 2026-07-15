package sources

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// TestRPCHeaderAgnt2FieldsRoundTrip verifies that a header carrying the AGNT2
// typed-consensus fields round-trips through RPCHeader -> CreateGethHeader with an
// IDENTICAL hash. Without this, the op-batcher reconstructs post-Isthmus blocks
// with the wrong hash and stalls in an ErrReorg / clear-state loop.
func TestRPCHeaderAgnt2FieldsRoundTrip(t *testing.T) {
	mk := func(withAgnt2 bool) *types.Header {
		h := &types.Header{
			ParentHash:  common.HexToHash("0x01"),
			UncleHash:   types.EmptyUncleHash,
			Root:        common.HexToHash("0x02"),
			TxHash:      common.HexToHash("0x03"),
			ReceiptHash: common.HexToHash("0x04"),
			Number:      big.NewInt(89),
			GasLimit:    60_000_000,
			GasUsed:     59_500_000,
			Time:        1784000000,
			Difficulty:  big.NewInt(0),
			BaseFee:     big.NewInt(1_000_000_000),
		}
		if withAgnt2 {
			ir := common.HexToHash("0xc5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470")
			var ic uint64 = 1173
			h.InteractionRoot = &ir
			h.InteractionCount = &ic
		}
		return h
	}
	for _, withAgnt2 := range []bool{false, true} {
		h := mk(withAgnt2)
		ref := h.Hash()
		js, err := json.Marshal(h)
		if err != nil {
			t.Fatalf("withAgnt2=%v marshal: %v", withAgnt2, err)
		}
		var rpc RPCHeader
		if err := json.Unmarshal(js, &rpc); err != nil {
			t.Fatalf("withAgnt2=%v unmarshal: %v", withAgnt2, err)
		}
		if got := rpc.CreateGethHeader().Hash(); got != ref {
			t.Fatalf("withAgnt2=%v: reconstructed hash %s != real %s; json=%s", withAgnt2, got, ref, js)
		}
	}
}
