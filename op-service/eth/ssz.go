package eth

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

type BlockVersion int

const ( // iota is reset to 0
	BlockV1 BlockVersion = iota
	BlockV2
	BlockV3
	BlockV4
	// BlockV5 is V4 plus the AGNT2 header extensions: InteractionRoot/Count,
	// TypedOpRoot/Count, and TypedReexecRoot/Count. All six are committed to by the block
	// hash (see ExecutionPayload.CheckBlockHash), so they MUST be on the wire — without
	// them a receiver recomputes a different block hash than the producer sealed and
	// rejects every payload. op-geth sets InteractionRoot/Count on every block once the
	// AGNT2 (Optimism Isthmus) fork tag is active.
	//
	// The re-exec pair was added to this same version rather than to a V6. BlockV5 is
	// fork-local — it exists only in this tree, no external peer speaks it, and it has
	// never been deployed — so a second AGNT2 payload version would buy no compatibility
	// and would cost a duplicate gossip topic, a second sync path and a second raft decode
	// path to keep in step. The tradeoff is that V5 bytes written by an older build of this
	// fork no longer decode; that surfaces as a loud decode error, not as silent
	// corruption, and only ephemeral devnet state is ever encoded this way.
	BlockV5
)

// ExecutionPayload and ExecutionPayloadEnvelope are the only SSZ types we have to marshal/unmarshal,
// so instead of importing a SSZ lib we implement the bare minimum.
// This is more efficient than RLP, and matches the L1 consensus-layer encoding of ExecutionPayload.

var (
	// The payloads are small enough to read and write at once.
	// But this happens often enough that we want to avoid re-allocating buffers for this.
	payloadBufPool = sync.Pool{New: func() any {
		x := make([]byte, 0, 100_000)
		return &x
	}}

	// ErrExtraDataTooLarge occurs when the ExecutionPayload's ExtraData field
	// is too large to be properly represented in SSZ.
	ErrExtraDataTooLarge = errors.New("extra data too large")

	ErrBadTransactionOffset = errors.New("transactions offset is smaller than extra data offset, aborting")
	ErrBadWithdrawalsOffset = errors.New("withdrawals offset is smaller than transaction offset, aborting")
	ErrBadExtraDataOffset   = errors.New("unexpected extra data offset")
	ErrScopeTooSmall        = errors.New("scope too small to decode execution payload")

	ErrMissingData = errors.New("execution payload envelope is missing data")
)

const (
	// All fields (4s are offsets to dynamic data)
	blockV1FixedPart = 32 + 20 + 32 + 32 + 256 + 32 + 8 + 8 + 8 + 8 + 4 + 32 + 32 + 4

	// V1 + Withdrawals offset
	blockV2FixedPart = blockV1FixedPart + 4

	// V2 + BlobGasUsed + ExcessBlobGas
	blockV3FixedPart = blockV2FixedPart + 8 + 8

	// V3 + WithdrawalsRoot
	blockV4FixedPart = blockV3FixedPart + 32

	// V4 + InteractionRoot + InteractionCount + TypedOpRoot + TypedOpCount
	//    + TypedReexecRoot + TypedReexecCount.
	// All six are fixed-size and appended at the END of the fixed part, so the dynamic
	// offsets (ExtraData / Transactions / Withdrawals), which are all derived from
	// fixedSize, shift automatically and need no separate arithmetic.
	blockV5FixedPart = blockV4FixedPart + 32 + 8 + 32 + 8 + 32 + 8

	withdrawalSize = 8 + 8 + 20 + 8

	// MAX_TRANSACTIONS_PER_PAYLOAD in consensus spec
	// https://github.com/ethereum/consensus-specs/blob/ef434e87165e9a4c82a99f54ffd4974ae113f732/specs/bellatrix/beacon-chain.md#execution
	maxTransactionsPerPayload = 1 << 20

	// MAX_WITHDRAWALS_PER_PAYLOAD	 in consensus spec
	// https://github.com/ethereum/consensus-specs/blob/dev/specs/capella/beacon-chain.md#execution
	maxWithdrawalsPerPayload = 1 << 4
)

func (v BlockVersion) HasBlobProperties() bool {
	return v == BlockV3 || v == BlockV4 || v == BlockV5
}

func (v BlockVersion) HasWithdrawals() bool {
	return v == BlockV2 || v == BlockV3 || v == BlockV4 || v == BlockV5
}

func (v BlockVersion) HasParentBeaconBlockRoot() bool {
	return v == BlockV3 || v == BlockV4 || v == BlockV5
}

func (v BlockVersion) HasWithdrawalsRoot() bool {
	return v == BlockV4 || v == BlockV5
}

// HasAGNT2Fields reports whether the version carries the AGNT2 header extensions
// (InteractionRoot/Count, TypedOpRoot/Count).
func (v BlockVersion) HasAGNT2Fields() bool {
	return v == BlockV5
}

func executionPayloadFixedPart(version BlockVersion) uint32 {
	if version == BlockV5 {
		return blockV5FixedPart
	} else if version == BlockV4 {
		return blockV4FixedPart
	} else if version == BlockV3 {
		return blockV3FixedPart
	} else if version == BlockV2 {
		return blockV2FixedPart
	} else {
		return blockV1FixedPart
	}
}

func (payload *ExecutionPayload) inferVersion() BlockVersion {
	// Checked first: once the AGNT2 fork tag is active op-geth stamps InteractionRoot on
	// every block, and those fields are committed to by the block hash, so a payload
	// carrying them must be encoded as V5 or it will fail CheckBlockHash on arrival.
	if payload.InteractionRoot != nil {
		return BlockV5
	} else if payload.WithdrawalsRoot != nil && *payload.WithdrawalsRoot != types.EmptyWithdrawalsHash {
		return BlockV4
	} else if payload.ExcessBlobGas != nil && payload.BlobGasUsed != nil {
		return BlockV3
	} else if payload.Withdrawals != nil {
		return BlockV2
	} else {
		return BlockV1
	}
}

func (payload *ExecutionPayload) SizeSSZ() (full uint32) {
	return executionPayloadFixedPart(payload.inferVersion()) + uint32(len(payload.ExtraData)) + payload.transactionSize() + payload.withdrawalSize()
}

func (payload *ExecutionPayload) withdrawalSize() uint32 {
	if payload.Withdrawals == nil {
		return 0
	}

	return uint32(len(*payload.Withdrawals) * withdrawalSize)
}

func (payload *ExecutionPayload) transactionSize() uint32 {
	// One offset to each transaction
	result := uint32(len(payload.Transactions)) * 4
	// Each transaction
	for _, tx := range payload.Transactions {
		result += uint32(len(tx))
	}
	return result
}

// marshalBytes32LE returns the value of z as a 32-byte little-endian array.
func marshalBytes32LE(out []byte, z *Uint256Quantity) {
	_ = out[31] // bounds check hint to compiler
	binary.LittleEndian.PutUint64(out[0:8], z[0])
	binary.LittleEndian.PutUint64(out[8:16], z[1])
	binary.LittleEndian.PutUint64(out[16:24], z[2])
	binary.LittleEndian.PutUint64(out[24:32], z[3])
}

func unmarshalBytes32LE(in []byte, z *Uint256Quantity) {
	_ = in[31] // bounds check hint to compiler
	z[0] = binary.LittleEndian.Uint64(in[0:8])
	z[1] = binary.LittleEndian.Uint64(in[8:16])
	z[2] = binary.LittleEndian.Uint64(in[16:24])
	z[3] = binary.LittleEndian.Uint64(in[24:32])
}

// MarshalSSZ encodes the ExecutionPayload as SSZ type
func (payload *ExecutionPayload) MarshalSSZ(w io.Writer) (n int, err error) {
	fixedSize := executionPayloadFixedPart(payload.inferVersion())
	transactionSize := payload.transactionSize()

	// Cast to uint32 to enable 32-bit MIPS support where math.MaxUint32-executionPayloadFixedPart is too big for int
	// In that case, len(payload.ExtraData) can't be longer than an int so this is always false anyway.
	extraDataSize := uint32(len(payload.ExtraData))
	if extraDataSize > math.MaxUint32-fixedSize {
		return 0, ErrExtraDataTooLarge
	}

	scope := payload.SizeSSZ()

	buf := *payloadBufPool.Get().(*[]byte)
	if uint32(cap(buf)) < scope {
		buf = make([]byte, scope)
	} else {
		buf = buf[:scope]
	}
	defer payloadBufPool.Put(&buf)

	offset := uint32(0)
	copy(buf[offset:offset+32], payload.ParentHash[:])
	offset += 32
	copy(buf[offset:offset+20], payload.FeeRecipient[:])
	offset += 20
	copy(buf[offset:offset+32], payload.StateRoot[:])
	offset += 32
	copy(buf[offset:offset+32], payload.ReceiptsRoot[:])
	offset += 32
	copy(buf[offset:offset+256], payload.LogsBloom[:])
	offset += 256
	copy(buf[offset:offset+32], payload.PrevRandao[:])
	offset += 32
	binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(payload.BlockNumber))
	offset += 8
	binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(payload.GasLimit))
	offset += 8
	binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(payload.GasUsed))
	offset += 8
	binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(payload.Timestamp))
	offset += 8
	// offset to ExtraData
	binary.LittleEndian.PutUint32(buf[offset:offset+4], fixedSize)
	offset += 4
	marshalBytes32LE(buf[offset:offset+32], &payload.BaseFeePerGas)
	offset += 32
	copy(buf[offset:offset+32], payload.BlockHash[:])
	offset += 32
	// offset to Transactions
	binary.LittleEndian.PutUint32(buf[offset:offset+4], fixedSize+extraDataSize)
	offset += 4

	if payload.Withdrawals == nil && offset != fixedSize {
		panic("transactions - fixed part size is inconsistent")
	}

	if payload.Withdrawals != nil {
		binary.LittleEndian.PutUint32(buf[offset:offset+4], fixedSize+extraDataSize+transactionSize)
		offset += 4
	}

	payloadVersion := payload.inferVersion()
	if payloadVersion.HasBlobProperties() {
		if payload.BlobGasUsed == nil || payload.ExcessBlobGas == nil {
			return 0, errors.New("cannot encode ecotone payload without dencun header attributes")
		}
		binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(*payload.BlobGasUsed))
		offset += 8
		binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(*payload.ExcessBlobGas))
		offset += 8
	}

	if payloadVersion.HasWithdrawalsRoot() {
		if payload.WithdrawalsRoot == nil {
			return 0, errors.New("cannot encode Isthmus payload without withdrawals root")
		}
		copy(buf[offset:offset+32], (*payload.WithdrawalsRoot)[:])
		offset += 32
	}

	if payloadVersion.HasAGNT2Fields() {
		// InteractionRoot/Count are always present in V5 (op-geth stamps them on every
		// post-Isthmus block); inferVersion only selects V5 when InteractionRoot != nil.
		if payload.InteractionRoot == nil || payload.InteractionCount == nil {
			return 0, errors.New("cannot encode AGNT2 payload without interaction root and count")
		}
		copy(buf[offset:offset+32], (*payload.InteractionRoot)[:])
		offset += 32
		binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(*payload.InteractionCount))
		offset += 8
		// TypedOpRoot/Count are optional: op-geth only sets them when the block carries
		// typed ops (count > 0), leaving them nil otherwise so the optional RLP header
		// fields stay absent. Encode "absent" as an all-zero root with count 0 — sound
		// because op-geth never sets the root with a zero count, so the decoder can
		// recover nil-ness exactly (nil-ness is hash-relevant: CheckBlockHash folds these
		// pointers into the reconstructed header).
		if payload.TypedOpRoot != nil && payload.TypedOpCount != nil {
			copy(buf[offset:offset+32], (*payload.TypedOpRoot)[:])
			offset += 32
			binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(*payload.TypedOpCount))
			offset += 8
		} else {
			// zero root + zero count == absent. Zero explicitly: buf comes from a pool and
			// may hold stale bytes, so skipping the write would serialize garbage.
			clear(buf[offset : offset+32+8])
			offset += 32 + 8
		}
		// TypedReexecRoot/Count (B2' Stage 2) are optional on the same terms, and are set
		// independently of TypedOpRoot: op-geth stamps them when the typed RE-EXECUTION
		// fold yields a leaf, which a block can do while some other typed op is skipped.
		// Same absent encoding, same reasoning about the pooled buffer.
		if payload.TypedReexecRoot != nil && payload.TypedReexecCount != nil {
			copy(buf[offset:offset+32], (*payload.TypedReexecRoot)[:])
			offset += 32
			binary.LittleEndian.PutUint64(buf[offset:offset+8], uint64(*payload.TypedReexecCount))
			offset += 8
		} else {
			clear(buf[offset : offset+32+8])
			offset += 32 + 8
		}
	}

	if payload.Withdrawals != nil && offset != fixedSize {
		panic("withdrawals - fixed part size is inconsistent")
	}

	// dynamic value 1: ExtraData
	copy(buf[offset:offset+extraDataSize], payload.ExtraData[:])
	offset += extraDataSize
	// dynamic value 2: Transactions
	marshalTransactions(buf[offset:offset+transactionSize], payload.Transactions)
	offset += transactionSize
	// dynamic value 3: Withdrawals
	if payload.Withdrawals != nil {
		marshalWithdrawals(buf[offset:], *payload.Withdrawals)
	}

	return w.Write(buf)
}

func marshalWithdrawals(out []byte, withdrawals types.Withdrawals) {
	offset := uint32(0)

	for _, withdrawal := range withdrawals {
		binary.LittleEndian.PutUint64(out[offset:offset+8], withdrawal.Index)
		offset += 8
		binary.LittleEndian.PutUint64(out[offset:offset+8], withdrawal.Validator)
		offset += 8
		copy(out[offset:offset+20], withdrawal.Address[:])
		offset += 20
		binary.LittleEndian.PutUint64(out[offset:offset+8], withdrawal.Amount)
		offset += 8
	}
}

func marshalTransactions(out []byte, txs []Data) {
	offset := uint32(0)
	txOffset := uint32(len(txs)) * 4
	for _, tx := range txs {
		binary.LittleEndian.PutUint32(out[offset:offset+4], txOffset)
		offset += 4
		nextTxOffset := txOffset + uint32(len(tx))
		copy(out[txOffset:nextTxOffset], tx)
		txOffset = nextTxOffset
	}
}

// UnmarshalSSZ decodes the ExecutionPayload as SSZ type
func (payload *ExecutionPayload) UnmarshalSSZ(version BlockVersion, scope uint32, r io.Reader) error {
	fixedSize := executionPayloadFixedPart(version)

	if scope < fixedSize {
		return fmt.Errorf("scope too small to decode execution payload: %d, version is: %v", scope, version)
	}

	buf := *payloadBufPool.Get().(*[]byte)
	if uint32(cap(buf)) < scope {
		buf = make([]byte, scope)
	} else {
		buf = buf[:scope]
	}
	defer payloadBufPool.Put(&buf)

	if _, err := io.ReadFull(r, buf); err != nil {
		return fmt.Errorf("failed to read fixed-size part of ExecutionPayload: %w", err)
	}
	offset := uint32(0)
	copy(payload.ParentHash[:], buf[offset:offset+32])
	offset += 32
	copy(payload.FeeRecipient[:], buf[offset:offset+20])
	offset += 20
	copy(payload.StateRoot[:], buf[offset:offset+32])
	offset += 32
	copy(payload.ReceiptsRoot[:], buf[offset:offset+32])
	offset += 32
	copy(payload.LogsBloom[:], buf[offset:offset+256])
	offset += 256
	copy(payload.PrevRandao[:], buf[offset:offset+32])
	offset += 32
	payload.BlockNumber = Uint64Quantity(binary.LittleEndian.Uint64(buf[offset : offset+8]))
	offset += 8
	payload.GasLimit = Uint64Quantity(binary.LittleEndian.Uint64(buf[offset : offset+8]))
	offset += 8
	payload.GasUsed = Uint64Quantity(binary.LittleEndian.Uint64(buf[offset : offset+8]))
	offset += 8
	payload.Timestamp = Uint64Quantity(binary.LittleEndian.Uint64(buf[offset : offset+8]))
	offset += 8
	extraDataOffset := binary.LittleEndian.Uint32(buf[offset : offset+4])
	if extraDataOffset != fixedSize {
		return fmt.Errorf("%w: %d <> %d", ErrBadExtraDataOffset, extraDataOffset, fixedSize)
	}
	offset += 4
	unmarshalBytes32LE(buf[offset:offset+32], &payload.BaseFeePerGas)
	offset += 32
	copy(payload.BlockHash[:], buf[offset:offset+32])
	offset += 32

	transactionsOffset := binary.LittleEndian.Uint32(buf[offset : offset+4])
	if transactionsOffset < extraDataOffset {
		return ErrBadTransactionOffset
	}
	offset += 4
	if version == BlockV1 && offset != fixedSize {
		panic("fixed part size is inconsistent")
	}

	withdrawalsOffset := scope
	if version.HasWithdrawals() {
		withdrawalsOffset = binary.LittleEndian.Uint32(buf[offset : offset+4])
		offset += 4

		if withdrawalsOffset < transactionsOffset {
			return ErrBadWithdrawalsOffset
		}
		if withdrawalsOffset > scope {
			return fmt.Errorf("withdrawals offset is too large: %d", withdrawalsOffset)
		}
	}

	if version.HasBlobProperties() {
		blobGasUsed := binary.LittleEndian.Uint64(buf[offset : offset+8])
		payload.BlobGasUsed = (*Uint64Quantity)(&blobGasUsed)
		offset += 8
		excessBlobGas := binary.LittleEndian.Uint64(buf[offset : offset+8])
		payload.ExcessBlobGas = (*Uint64Quantity)(&excessBlobGas)
		offset += 8
	}

	if version.HasWithdrawalsRoot() {
		withdrawalsRoot := common.Hash{}
		copy(withdrawalsRoot[:], buf[offset:offset+32])
		payload.WithdrawalsRoot = &withdrawalsRoot
		offset += 32
	}

	if version.HasAGNT2Fields() {
		interactionRoot := common.Hash{}
		copy(interactionRoot[:], buf[offset:offset+32])
		payload.InteractionRoot = &interactionRoot
		offset += 32
		interactionCount := Uint64Quantity(binary.LittleEndian.Uint64(buf[offset : offset+8]))
		payload.InteractionCount = &interactionCount
		offset += 8

		typedOpRoot := common.Hash{}
		copy(typedOpRoot[:], buf[offset:offset+32])
		offset += 32
		typedOpCount := Uint64Quantity(binary.LittleEndian.Uint64(buf[offset : offset+8]))
		offset += 8
		// count == 0 means the producer had no typed ops and left both fields nil; keep
		// them nil so the reconstructed header hashes identically (see MarshalSSZ).
		if typedOpCount > 0 {
			payload.TypedOpRoot = &typedOpRoot
			payload.TypedOpCount = &typedOpCount
		}

		typedReexecRoot := common.Hash{}
		copy(typedReexecRoot[:], buf[offset:offset+32])
		offset += 32
		typedReexecCount := Uint64Quantity(binary.LittleEndian.Uint64(buf[offset : offset+8]))
		offset += 8
		if typedReexecCount > 0 {
			payload.TypedReexecRoot = &typedReexecRoot
			payload.TypedReexecCount = &typedReexecCount
		}
	}

	_ = offset // for future extensions: we keep the offset accurate for extensions

	if transactionsOffset > extraDataOffset+32 || transactionsOffset > scope {
		return fmt.Errorf("extra-data is too large: %d", transactionsOffset-extraDataOffset)
	}

	extraDataSize := transactionsOffset - extraDataOffset
	payload.ExtraData = make(BytesMax32, extraDataSize)
	copy(payload.ExtraData, buf[extraDataOffset:transactionsOffset])

	txs, err := unmarshalTransactions(buf[transactionsOffset:withdrawalsOffset])
	if err != nil {
		return fmt.Errorf("failed to unmarshal transactions list: %w", err)
	}
	payload.Transactions = txs

	if version.HasWithdrawals() {
		withdrawals, err := unmarshalWithdrawals(buf[withdrawalsOffset:])
		if err != nil {
			return fmt.Errorf("failed to unmarshal withdrawals list: %w", err)
		}
		payload.Withdrawals = &withdrawals
	}

	return nil
}

func unmarshalWithdrawals(in []byte) (types.Withdrawals, error) {
	result := types.Withdrawals{} // empty list by default, intentionally non-nil

	if len(in)%withdrawalSize != 0 {
		return nil, errors.New("invalid withdrawals data")
	}

	withdrawalCount := len(in) / withdrawalSize

	if withdrawalCount > maxWithdrawalsPerPayload {
		return nil, fmt.Errorf("too many withdrawals: %d > %d", withdrawalCount, maxWithdrawalsPerPayload)
	}

	offset := 0

	for i := 0; i < withdrawalCount; i++ {
		withdrawal := &types.Withdrawal{}

		withdrawal.Index = binary.LittleEndian.Uint64(in[offset : offset+8])
		offset += 8

		withdrawal.Validator = binary.LittleEndian.Uint64(in[offset : offset+8])
		offset += 8

		copy(withdrawal.Address[:], in[offset:offset+20])
		offset += 20

		withdrawal.Amount = binary.LittleEndian.Uint64(in[offset : offset+8])
		offset += 8

		result = append(result, withdrawal)
	}

	return result, nil
}

func unmarshalTransactions(in []byte) (txs []Data, err error) {
	scope := uint32(len(in))
	if scope == 0 { // empty txs list
		return make([]Data, 0), nil
	}
	if scope < 4 {
		return nil, fmt.Errorf("not enough scope to read first tx offset: %d", scope)
	}
	offset := uint32(0)
	firstTxOffset := binary.LittleEndian.Uint32(in[offset : offset+4])
	offset += 4
	if firstTxOffset%4 != 0 {
		return nil, fmt.Errorf("invalid first tx offset: %d, not a multiple of offset size", firstTxOffset)
	}
	if firstTxOffset > scope {
		return nil, fmt.Errorf("invalid first tx offset: %d, out of scope %d", firstTxOffset, scope)
	}
	txCount := firstTxOffset / 4
	if txCount == 0 && scope > 0 {
		return nil, fmt.Errorf("invalid first tx offset: %d, no transactions in scope %d", firstTxOffset, scope)
	}
	if txCount > maxTransactionsPerPayload {
		return nil, fmt.Errorf("too many transactions: %d > %d", txCount, maxTransactionsPerPayload)
	}
	txs = make([]Data, txCount)
	currentTxOffset := firstTxOffset
	for i := uint32(0); i < txCount; i++ {
		nextTxOffset := scope
		if i+1 < txCount {
			nextTxOffset = binary.LittleEndian.Uint32(in[offset : offset+4])
			offset += 4
		}
		if nextTxOffset < currentTxOffset || nextTxOffset > scope {
			return nil, fmt.Errorf("tx %d has bad next offset: %d, current is %d, scope is %d", i, nextTxOffset, currentTxOffset, scope)
		}
		currentTxSize := nextTxOffset - currentTxOffset
		txs[i] = make(Data, currentTxSize)
		copy(txs[i], in[currentTxOffset:nextTxOffset])
		currentTxOffset = nextTxOffset
	}
	return txs, nil
}

// UnmarshalSSZ decodes the ExecutionPayloadEnvelope as SSZ type
func (envelope *ExecutionPayloadEnvelope) UnmarshalSSZ(version BlockVersion, scope uint32, r io.Reader) error {
	if scope < common.HashLength {
		return fmt.Errorf("%w: %d", ErrScopeTooSmall, scope)
	}

	data := make([]byte, common.HashLength)
	n, err := r.Read(data)
	if err != nil || n != common.HashLength {
		return err
	}

	envelope.ParentBeaconBlockRoot = &common.Hash{}
	copy(envelope.ParentBeaconBlockRoot[:], data)

	var payload ExecutionPayload
	err = payload.UnmarshalSSZ(version, scope-32, r)
	if err != nil {
		return err
	}

	envelope.ExecutionPayload = &payload
	return nil
}

// MarshalSSZ encodes the ExecutionPayload as SSZ type
func (envelope *ExecutionPayloadEnvelope) MarshalSSZ(w io.Writer) (n int, err error) {
	if envelope.ExecutionPayload == nil || envelope.ParentBeaconBlockRoot == nil {
		return 0, ErrMissingData
	}

	// write parent beacon block root
	hashSize, err := w.Write(envelope.ParentBeaconBlockRoot[:])
	if err != nil || hashSize != common.HashLength {
		return 0, errors.New("unable to write parent beacon block hash")
	}

	payloadSize, err := envelope.ExecutionPayload.MarshalSSZ(w)
	if err != nil {
		return 0, err
	}

	return hashSize + payloadSize, nil
}
