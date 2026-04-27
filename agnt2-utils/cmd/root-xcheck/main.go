package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"

	agnt2utils "github.com/ethereum-optimism/optimism/agnt2-utils"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// locked TS-computed interaction roots from chain-config/repro/verify-trace-log.csv
var traces = []struct {
	traceID  string
	fileName string // basename without extension (file names lack -001 suffix)
	tsRoot   string
}{
	{
		traceID:  "agentcity-governance-001",
		fileName: "agentcity-governance",
		tsRoot:   "0xc95c861ea60a794f6f559f41538a292c3a8af582350a36bf1557e31d7650b64a",
	},
	{
		traceID:  "mitosis-research-001",
		fileName: "mitosis-research",
		tsRoot:   "0x1d143247209debb3e33f624d456efc0024a2c4b26c5f780f295cccebf89c26fb",
	},
	{
		traceID:  "synthetic-shared-role-contention-001",
		fileName: "synthetic-shared-role-contention",
		tsRoot:   "0xb9d689de0e2bcba9ebba8f24ca60629ce9d8e61783056a33b7afb5838bc3a84d",
	},
}

type step struct {
	StepID       string      `json:"step_id"`
	AgentRole    string      `json:"agent_role"`
	Deps         []string    `json:"deps"`
	PayoutAmount json.Number `json:"payout_amount"`
}

type workflowTrace struct {
	TraceID string `json:"trace_id"`
	Steps   []step `json:"steps"`
}

func main() {
	tracesDir := flag.String("traces-dir", "../../workload-capture/traces/converted", "directory containing .workflow-trace-v3.json files")
	outCSV := flag.String("out-csv", "../../chain-config/repro/root-xcheck-week13.csv", "output CSV path")
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*outCSV), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create output dir: %v\n", err)
		os.Exit(1)
	}
	csvFile, err := os.Create(*outCSV)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create output csv: %v\n", err)
		os.Exit(1)
	}
	defer csvFile.Close()
	fmt.Fprintln(csvFile, "trace_id,go_root,ts_root,match")

	allMatch := true
	for _, tr := range traces {
		filePath := filepath.Join(*tracesDir, tr.fileName+".workflow-trace-v3.json")
		data, err := os.ReadFile(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to read %s: %v\n", filePath, err)
			os.Exit(1)
		}

		var trace workflowTrace
		if err := json.Unmarshal(data, &trace); err != nil {
			fmt.Fprintf(os.Stderr, "failed to parse %s: %v\n", filePath, err)
			os.Exit(1)
		}

		leaves, err := computeLeaves(trace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "leaf computation failed for %s: %v\n", tr.traceID, err)
			os.Exit(1)
		}

		state := agnt2utils.IncrementalAppend(agnt2utils.EmptyAccumulatorState(), leaves)
		goRootHex := state.Root.Hex()
		match := goRootHex == tr.tsRoot
		if !match {
			allMatch = false
		}

		fmt.Printf("XCHECK trace_id=%s go_root=%s ts_root=%s match=%t\n", tr.traceID, goRootHex, tr.tsRoot, match)
		fmt.Fprintf(csvFile, "%s,%s,%s,%t\n", tr.traceID, goRootHex, tr.tsRoot, match)
		if !match {
			fmt.Fprintf(os.Stderr, "MISMATCH trace_id=%s go_root=%s ts_root=%s\n", tr.traceID, goRootHex, tr.tsRoot)
		}
	}

	if !allMatch {
		os.Exit(1)
	}
	fmt.Println("XCHECK ALL PASS: Go roots match locked TS roots for all 3 traces")
}

// computeLeaves produces the MMR leaf sequence for a workflow trace using the
// canonical AGNT2 leaf encoding: each leaf = keccak256(wfIdHash ++ stepIdHash ++
// agentRoleHash ++ payout[BE32] ++ prevLeafHash). Steps are processed in
// topological order (Kahn's BFS preserving original array order for same-level ties).
func computeLeaves(trace workflowTrace) ([][32]byte, error) {
	workflowIDHash := crypto.Keccak256Hash([]byte(trace.TraceID))

	// build indegree and adjacency in original array order
	stepMap := make(map[string]step, len(trace.Steps))
	indegree := make(map[string]int, len(trace.Steps))
	adjacency := make(map[string][]string, len(trace.Steps))
	for _, s := range trace.Steps {
		stepMap[s.StepID] = s
		indegree[s.StepID] = len(s.Deps)
		adjacency[s.StepID] = nil
	}
	for _, s := range trace.Steps {
		for _, dep := range s.Deps {
			adjacency[dep] = append(adjacency[dep], s.StepID)
		}
	}

	// Kahn's BFS: seed with zero-indegree steps in original array order
	queue := make([]string, 0, len(trace.Steps))
	for _, s := range trace.Steps {
		if indegree[s.StepID] == 0 {
			queue = append(queue, s.StepID)
		}
	}

	var ordered []step
	for len(queue) > 0 {
		currID := queue[0]
		queue = queue[1:]
		ordered = append(ordered, stepMap[currID])
		// visit successors in original array order
		for _, s := range trace.Steps {
			for _, dep := range s.Deps {
				if dep == currID {
					indegree[s.StepID]--
					if indegree[s.StepID] == 0 {
						queue = append(queue, s.StepID)
					}
					break
				}
			}
		}
	}

	if len(ordered) != len(trace.Steps) {
		return nil, fmt.Errorf("cycle or missing steps: got %d ordered, expected %d", len(ordered), len(trace.Steps))
	}

	var prevLeafHash common.Hash // starts as 0x00...00
	leaves := make([][32]byte, 0, len(ordered))
	for _, s := range ordered {
		stepIDHash := crypto.Keccak256Hash([]byte(s.StepID))
		agentRoleHash := crypto.Keccak256Hash([]byte(s.AgentRole))

		payoutStr := s.PayoutAmount.String()
		if payoutStr == "" {
			payoutStr = "0"
		}
		payout, ok := new(big.Int).SetString(payoutStr, 10)
		if !ok {
			return nil, fmt.Errorf("invalid payout %q in step %s", payoutStr, s.StepID)
		}
		payoutBE := make([]byte, 32)
		payout.FillBytes(payoutBE)

		leafData := make([]byte, 0, 160)
		leafData = append(leafData, workflowIDHash.Bytes()...)
		leafData = append(leafData, stepIDHash.Bytes()...)
		leafData = append(leafData, agentRoleHash.Bytes()...)
		leafData = append(leafData, payoutBE...)
		leafData = append(leafData, prevLeafHash.Bytes()...)

		leafHash := crypto.Keccak256Hash(leafData)
		leaves = append(leaves, leafHash)
		prevLeafHash = leafHash
	}
	return leaves, nil
}
