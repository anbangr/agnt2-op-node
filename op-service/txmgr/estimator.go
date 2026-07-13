package txmgr

import (
	"context"
	"errors"
	"math/big"
)

type GasPriceEstimatorFn func(ctx context.Context, backend ETHBackend) (*big.Int, *big.Int, *big.Int, error)

func DefaultGasPriceEstimatorFn(ctx context.Context, backend ETHBackend) (*big.Int, *big.Int, *big.Int, error) {
	tip, err := backend.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, nil, err
	}

	head, err := backend.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	if head.BaseFee == nil {
		return nil, nil, nil, errors.New("txmgr does not support pre-london blocks that do not have a base fee")
	}

	blobBaseFee, err := backend.BlobBaseFee(ctx)
	if err != nil {
		// AGNT2 devnet fallback: the pinned L1 geth (v1.13.15) predates the
		// eth_blobBaseFee RPC (added in geth v1.14). When it is unavailable,
		// compute the EIP-4844 blob base fee locally from the header's excess
		// blob gas — the exact value the RPC would return on a Cancun L1. In
		// calldata-DA mode this value is not used to build the tx, so it only
		// needs to be present and non-erroring; we keep it correct regardless.
		if head.ExcessBlobGas == nil {
			return nil, nil, nil, err
		}
		blobBaseFee = calcBlobBaseFeeCancun(*head.ExcessBlobGas)
	}

	return tip, head.BaseFee, blobBaseFee, nil
}

// calcBlobBaseFeeCancun implements the EIP-4844 blob base fee formula using the
// Cancun update fraction. It is a self-contained fallback used only when the L1
// does not expose the eth_blobBaseFee RPC (see DefaultGasPriceEstimatorFn).
func calcBlobBaseFeeCancun(excessBlobGas uint64) *big.Int {
	const blobBaseFeeUpdateFractionCancun = 3338477 // EIP-4844 (Cancun)
	return fakeExponential(
		big.NewInt(1), // MIN_BLOB_BASE_FEE
		new(big.Int).SetUint64(excessBlobGas),
		big.NewInt(blobBaseFeeUpdateFractionCancun),
	)
}

// fakeExponential approximates factor * e ** (numerator / denominator) using
// the integer series defined by EIP-4844.
func fakeExponential(factor, numerator, denominator *big.Int) *big.Int {
	output := new(big.Int)
	accum := new(big.Int).Mul(factor, denominator)
	for i := 1; accum.Sign() > 0; i++ {
		output.Add(output, accum)
		accum.Mul(accum, numerator)
		accum.Div(accum, denominator)
		accum.Div(accum, big.NewInt(int64(i)))
	}
	return output.Div(output, denominator)
}
