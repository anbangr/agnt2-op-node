package main

import (
	"encoding/json"
	"io"
	"sort"
)

// writeSummaries writes top-N rankings, histograms, and per-address aggregations.
func writeSummaries(w io.Writer, profiles []TxProfile, topN int) error {
	enc := json.NewEncoder(w)

	// Top N by sstoreCount (most storage writes)
	if err := writeTopN(enc, profiles, topN, "top_sstore_count", func(a, b TxProfile) bool {
		return a.SstoreCount > b.SstoreCount
	}); err != nil {
		return err
	}

	// Top N by sstoreRatio (most storage-dominated)
	if err := writeTopN(enc, profiles, topN, "top_sstore_ratio", func(a, b TxProfile) bool {
		return a.SstoreRatio > b.SstoreRatio
	}); err != nil {
		return err
	}

	// Top N by gasUsed (most expensive)
	if err := writeTopN(enc, profiles, topN, "top_gas_used", func(a, b TxProfile) bool {
		return a.GasUsed > b.GasUsed
	}); err != nil {
		return err
	}

	// Top N by refundRatio DESC (most benefit from opgas)
	if err := writeTopN(enc, profiles, topN, "top_refund_ratio", func(a, b TxProfile) bool {
		return a.RefundRatio > b.RefundRatio
	}); err != nil {
		return err
	}

	// Top N by refundRatio ASC where storageHeavy=true (penalized by cutoff)
	var penalized []TxProfile
	for _, p := range profiles {
		if p.StorageHeavy {
			penalized = append(penalized, p)
		}
	}
	if err := writeTopN(enc, penalized, topN, "top_penalized_storage_heavy", func(a, b TxProfile) bool {
		// Sort by gasUsed DESC (largest penalty first)
		return a.GasUsed > b.GasUsed
	}); err != nil {
		return err
	}

	// Histograms
	if err := writeHistogram(enc, profiles, "sstore_count", func(p TxProfile) string {
		switch {
		case p.SstoreCount == 0:
			return "0"
		case p.SstoreCount <= 5:
			return "1-5"
		case p.SstoreCount <= 10:
			return "6-10"
		case p.SstoreCount <= 20:
			return "11-20"
		case p.SstoreCount <= 50:
			return "21-50"
		case p.SstoreCount <= 100:
			return "51-100"
		default:
			return "100+"
		}
	}, []string{"0", "1-5", "6-10", "11-20", "21-50", "51-100", "100+"}); err != nil {
		return err
	}

	if err := writeHistogram(enc, profiles, "sstore_ratio", func(p TxProfile) string {
		return ratioToBucket(p.SstoreRatio)
	}, ratioBucketLabels); err != nil {
		return err
	}

	if err := writeHistogram(enc, profiles, "refund_ratio", func(p TxProfile) string {
		return ratioToBucket(p.RefundRatio)
	}, ratioBucketLabels); err != nil {
		return err
	}

	// Per-address aggregation (top N by tx count)
	if err := writeAddressAgg(enc, profiles, topN); err != nil {
		return err
	}

	return nil
}

var ratioBucketLabels = []string{"0%", "0-10%", "10-25%", "25-50%", "50-75%", "75-100%"}

func ratioToBucket(r float64) string {
	switch {
	case r == 0:
		return "0%"
	case r < 0.10:
		return "0-10%"
	case r < 0.25:
		return "10-25%"
	case r < 0.50:
		return "25-50%"
	case r < 0.75:
		return "50-75%"
	default:
		return "75-100%"
	}
}

func writeTopN(enc *json.Encoder, profiles []TxProfile, n int, ranking string, less func(a, b TxProfile) bool) error {
	sorted := make([]TxProfile, len(profiles))
	copy(sorted, profiles)
	sort.Slice(sorted, func(i, j int) bool { return less(sorted[i], sorted[j]) })

	if n > len(sorted) {
		n = len(sorted)
	}
	for i := 0; i < n; i++ {
		rec := TopNRecord{
			Type:    "top_n",
			Ranking: ranking,
			Rank:    i + 1,
			Profile: sorted[i],
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}

func writeHistogram(enc *json.Encoder, profiles []TxProfile, field string, bucketFn func(TxProfile) string, orderedLabels []string) error {
	counts := make(map[string]int)
	for _, label := range orderedLabels {
		counts[label] = 0
	}
	for _, p := range profiles {
		label := bucketFn(p)
		counts[label]++
	}
	buckets := make([]HistogramBucket, 0, len(orderedLabels))
	for _, label := range orderedLabels {
		buckets = append(buckets, HistogramBucket{Label: label, Count: counts[label]})
	}
	return enc.Encode(HistogramRecord{
		Type:    "histogram",
		Field:   field,
		Buckets: buckets,
		Total:   len(profiles),
	})
}

func writeAddressAgg(enc *json.Encoder, profiles []TxProfile, topN int) error {
	type addrStats struct {
		txCount      int
		totalSstores uint64
		totalGas     uint64
		sumSstoreR   float64
		sumRefundR   float64
		heavyCount   int
	}
	m := make(map[string]*addrStats)
	for _, p := range profiles {
		addr := p.To
		if addr == "" {
			addr = "CREATE"
		}
		s, ok := m[addr]
		if !ok {
			s = &addrStats{}
			m[addr] = s
		}
		s.txCount++
		s.totalSstores += p.SstoreCount
		s.totalGas += p.GasUsed
		s.sumSstoreR += p.SstoreRatio
		s.sumRefundR += p.RefundRatio
		if p.StorageHeavy {
			s.heavyCount++
		}
	}

	// Sort by tx count desc, take top N
	type kv struct {
		addr string
		s    *addrStats
	}
	var sorted []kv
	for addr, s := range m {
		sorted = append(sorted, kv{addr, s})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].s.txCount > sorted[j].s.txCount })
	if topN > len(sorted) {
		topN = len(sorted)
	}

	for i := 0; i < topN; i++ {
		e := sorted[i]
		n := float64(e.s.txCount)
		rec := AddressAgg{
			Type:            "address_agg",
			Address:         e.addr,
			TxCount:         e.s.txCount,
			TotalSstores:    e.s.totalSstores,
			TotalGasUsed:    e.s.totalGas,
			AvgSstoreRatio:  e.s.sumSstoreR / n,
			AvgRefundRatio:  e.s.sumRefundR / n,
			StorageHeavyPct: float64(e.s.heavyCount) / n * 100,
		}
		if err := enc.Encode(rec); err != nil {
			return err
		}
	}
	return nil
}
