package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/ethereum-optimism/optimism/op-chain-ops/pkg/sdmreplay"
)

func main() {
	rpcURL := flag.String("rpc", "http://localhost:8545", "RPC endpoint of local modified op-geth")
	fromBlockStr := flag.String("from-block", "", "Start block (hex, decimal, 'latest', or 'latest-N')")
	toBlockStr := flag.String("to-block", "", "End block (hex, decimal, 'latest', or 'latest-N')")
	topN := flag.Int("top", 10, "Number of top transactions per ranking")
	output := flag.String("output", "/tmp/sdm_profile.jsonl", "Output JSONL file path")
	flag.Parse()

	if *fromBlockStr == "" || *toBlockStr == "" {
		fmt.Fprintf(os.Stderr, "Usage: sdm-profiler --from-block N --to-block M [flags]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	ctx := context.Background()
	client := sdmreplay.NewClient(*rpcURL)

	fromBlock, err := sdmreplay.ResolveBlockNum(ctx, client, *fromBlockStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving --from-block: %v\n", err)
		os.Exit(1)
	}
	toBlock, err := sdmreplay.ResolveBlockNum(ctx, client, *toBlockStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error resolving --to-block: %v\n", err)
		os.Exit(1)
	}

	if fromBlock > toBlock {
		fmt.Fprintf(os.Stderr, "Error: --from-block (%d) > --to-block (%d)\n", fromBlock, toBlock)
		os.Exit(1)
	}

	outFile, err := os.Create(*output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", err)
		os.Exit(1)
	}
	defer outFile.Close()

	enc := json.NewEncoder(outFile)
	var allProfiles []TxProfile
	totalTxs := 0

	fmt.Fprintf(os.Stderr, "Profiling blocks %d to %d (%d blocks)\n", fromBlock, toBlock, toBlock-fromBlock+1)

	for blockNum := fromBlock; blockNum <= toBlock; blockNum++ {
		fmt.Fprintf(os.Stderr, "  Block %d ...", blockNum)

		tracerResults, err := client.TraceBlock(ctx, blockNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, " error: %v\n", err)
			os.Exit(1)
		}

		fmt.Fprintf(os.Stderr, " %d txs\n", len(tracerResults))

		for i, tr := range tracerResults {
			to := ""
			if tr.To != nil {
				to = *tr.To
			}

			profile := TxProfile{
				Type:            "tx",
				BlockNum:        blockNum,
				TxIndex:         i,
				TxHash:          tr.TxHash,
				From:            tr.From,
				To:              to,
				GasUsed:         tr.GasUsed,
				OPGasRefund:     tr.OPGasRefund,
				EffectiveGas:    tr.EffectiveGas,
				RefundRatio:     tr.RefundRatio,
				SstoreCount:     tr.SstoreCount,
				SstoreGas:       tr.SstoreGas,
				SstoreRatio:     tr.SstoreRatio,
				StorageHeavy:    tr.StorageHeavy,
				WallClockMicros: tr.WallClockMicros,
				CalldataLen:     tr.CalldataLen,
				Status:          tr.Status,
			}

			if err := enc.Encode(profile); err != nil {
				fmt.Fprintf(os.Stderr, "Error writing profile: %v\n", err)
				os.Exit(1)
			}
			allProfiles = append(allProfiles, profile)
			totalTxs++
		}
	}

	// Write summaries
	fmt.Fprintf(os.Stderr, "Writing summaries for %d transactions (top %d per ranking)...\n", totalTxs, *topN)
	if err := writeSummaries(outFile, allProfiles, *topN); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing summaries: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Done. Output: %s (%d txs across %d blocks)\n", *output, totalTxs, toBlock-fromBlock+1)
}
