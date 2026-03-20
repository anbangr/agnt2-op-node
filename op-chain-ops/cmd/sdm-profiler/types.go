package main

import "github.com/ethereum-optimism/optimism/op-chain-ops/pkg/sdmreplay"

// TxProfile is the per-transaction profiling record written to JSONL output.
type TxProfile struct {
	Type            string  `json:"type"`
	BlockNum        uint64  `json:"blockNum"`
	TxIndex         int     `json:"txIndex"`
	TxHash          string  `json:"txHash"`
	From            string  `json:"from"`
	To              string  `json:"to"`
	GasUsed         uint64  `json:"gasUsed"`
	OPGasRefund     uint64  `json:"opGasRefund"`
	EffectiveGas    uint64  `json:"effectiveGas"`
	RefundRatio     float64 `json:"refundRatio"`
	SstoreCount     uint64  `json:"sstoreCount"`
	SstoreGas       uint64  `json:"sstoreGas"`
	SstoreRatio     float64 `json:"sstoreRatio"`
	StorageHeavy    bool    `json:"storageHeavy"`
	WallClockMicros int64   `json:"wallClockMicros"`
	CalldataLen     int     `json:"calldataLen"`
	Status          uint64  `json:"status"`
}

type TracerResult = sdmreplay.TracerResult

// TopNRecord is a summary record identifying a top-N transaction.
type TopNRecord struct {
	Type    string    `json:"type"`
	Ranking string    `json:"ranking"`
	Rank    int       `json:"rank"`
	Profile TxProfile `json:"profile"`
}

// HistogramBucket is a single bucket in a distribution histogram.
type HistogramBucket struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

// HistogramRecord is a histogram summary record.
type HistogramRecord struct {
	Type    string            `json:"type"`
	Field   string            `json:"field"`
	Buckets []HistogramBucket `json:"buckets"`
	Total   int               `json:"total"`
}

// AddressAgg is the per-address aggregation record.
type AddressAgg struct {
	Type            string  `json:"type"`
	Address         string  `json:"address"`
	TxCount         int     `json:"txCount"`
	TotalSstores    uint64  `json:"totalSstores"`
	TotalGasUsed    uint64  `json:"totalGasUsed"`
	AvgSstoreRatio  float64 `json:"avgSstoreRatio"`
	AvgRefundRatio  float64 `json:"avgRefundRatio"`
	StorageHeavyPct float64 `json:"storageHeavyPct"`
}
