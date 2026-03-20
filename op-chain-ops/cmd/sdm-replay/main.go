package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/ethereum-optimism/optimism/op-chain-ops/pkg/sdmreplay"
)

func main() {
	rpcURL := flag.String("rpc", "http://localhost:8545", "RPC endpoint used for block, receipt, and debug_replaySdmBlock access")
	fromBlockStr := flag.String("from-block", "", "Start block (hex, decimal, 'latest', or 'latest-N')")
	toBlockStr := flag.String("to-block", "", "End block (hex, decimal, 'latest', or 'latest-N')")
	outPath := flag.String("out", "", "Output JSONL file path")
	comparePayload := flag.Bool("compare-payload", false, "Compare replay-derived refunds against any embedded SDM payload in the source block")
	compareRPCReceipts := flag.Bool("compare-rpc-receipts", false, "Compare replay-derived refunds against receipt opGasRefund values")
	failOnMismatch := flag.Bool("fail-on-mismatch", false, "Exit non-zero when mismatches are found")
	skipEmptyBlocks := flag.Bool("skip-empty-blocks", false, "Skip blocks with no user transactions after deposits/system txs")
	includeTrace := flag.Bool("include-trace", false, "Reserved for the legacy tracer path; unsupported with debug_replaySdmBlock")
	summaryOnly := flag.Bool("summary-only", false, "Emit only run_config, block, mismatch, and summary rows")
	workers := flag.Int("workers", 1, "Reserved for future bounded parallelism; only 1 is currently supported")
	format := flag.String("format", "jsonl", "Output format; only 'jsonl' is currently supported")
	flag.Parse()

	if *fromBlockStr == "" || *toBlockStr == "" || *outPath == "" {
		fmt.Fprintf(os.Stderr, "Usage: sdm-replay --rpc URL --from-block N --to-block M --out PATH [flags]\n")
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

	cfg := sdmreplay.Config{
		RPCURL:             *rpcURL,
		FromBlockSelector:  *fromBlockStr,
		ToBlockSelector:    *toBlockStr,
		FromBlock:          fromBlock,
		ToBlock:            toBlock,
		ComparePayload:     *comparePayload,
		CompareRPCReceipts: *compareRPCReceipts,
		FailOnMismatch:     *failOnMismatch,
		SkipEmptyBlocks:    *skipEmptyBlocks,
		IncludeTrace:       *includeTrace,
		SummaryOnly:        *summaryOnly,
		Workers:            *workers,
		Format:             *format,
	}

	result, err := sdmreplay.ReplayRange(ctx, client, cfg)
	if err != nil && result == nil {
		fmt.Fprintf(os.Stderr, "Replay failed: %v\n", err)
		os.Exit(1)
	}

	outFile, fileErr := os.Create(*outPath)
	if fileErr != nil {
		fmt.Fprintf(os.Stderr, "Error creating output file: %v\n", fileErr)
		os.Exit(1)
	}
	defer outFile.Close()

	if writeErr := sdmreplay.WriteJSONL(outFile, result, cfg.SummaryOnly); writeErr != nil {
		fmt.Fprintf(os.Stderr, "Error writing JSONL output: %v\n", writeErr)
		os.Exit(1)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Replay completed with mismatches: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Replayed blocks %d to %d using %s mode. Output: %s\n",
		result.Summary.FromBlock, result.Summary.ToBlock, result.RunConfig.ReplayMode, *outPath)
}
