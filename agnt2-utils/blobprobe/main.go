// Command blobprobe sends an EIP-4844 type-3 transaction to a dev L1 to determine
// exactly which sidecar version the node's txpool accepts. Read-only against the
// chain except for the single test tx it sends. Intended for a throwaway dev chain.
package main

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"

	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
)

func main() {
	url := os.Args[1]
	rc, err := rpc.Dial(url)
	if err != nil {
		panic(err)
	}
	ec := ethclient.NewClient(rc)
	ctx := context.Background()

	chainID, err := ec.ChainID(ctx)
	must(err, "chainid")
	fmt.Printf("chainID=%v\n", chainID)

	// fresh throwaway key, funded from the unlocked dev account
	key, _ := crypto.GenerateKey()
	addr := crypto.PubkeyToAddress(key.PublicKey)

	var devAccts []common.Address
	must(rc.CallContext(ctx, &devAccts, "eth_accounts"), "eth_accounts")
	fmt.Printf("dev account=%s  test account=%s\n", devAccts[0].Hex(), addr.Hex())

	var fundHash common.Hash
	must(rc.CallContext(ctx, &fundHash, "eth_sendTransaction", map[string]any{
		"from":  devAccts[0].Hex(),
		"to":    addr.Hex(),
		"value": "0xde0b6b3a7640000", // 1 ETH
	}), "fund")
	waitMined(ctx, ec, fundHash, "funding")

	head, err := ec.HeaderByNumber(ctx, nil)
	must(err, "header")
	fmt.Printf("head=%d baseFee=%v excessBlobGas=%v requestsHash=%v\n",
		head.Number, head.BaseFee, head.ExcessBlobGas, head.RequestsHash != nil)

	var blobBaseFee string
	if err := rc.CallContext(ctx, &blobBaseFee, "eth_blobBaseFee"); err != nil {
		fmt.Printf("eth_blobBaseFee ERROR: %v\n", err)
	} else {
		fmt.Printf("eth_blobBaseFee=%s\n", blobBaseFee)
	}

	inbox := common.HexToAddress("0x00289c189bee4e70334629f04cd5ed602b6600eb")

	// Attempt 1: legacy version-0 sidecar (what op-batcher builds by default,
	// because txmgr.CellProofTime defaults to math.MaxUint64).
	trySend(ctx, ec, rc, key, addr, chainID, inbox, head, false, 0)
	// Attempt 2: version-1 cell-proof sidecar (Fusaka/Osaka form).
	trySend(ctx, ec, rc, key, addr, chainID, inbox, head, true, 0)
}

func trySend(ctx context.Context, ec *ethclient.Client, rc *rpc.Client, key *ecdsa.PrivateKey,
	addr common.Address, chainID *big.Int, inbox common.Address, head *types.Header,
	cellProofs bool, nonceOff uint64,
) {
	label := "sidecar VERSION 0 (legacy blob proofs)"
	if cellProofs {
		label = "sidecar VERSION 1 (cell proofs)"
	}
	fmt.Printf("\n===== attempting %s =====\n", label)

	var blob eth.Blob
	must(blob.FromData(eth.Data("agnt2 blob probe")), "encode blob")

	sidecar, hashes, err := txmgr.MakeSidecar([]*eth.Blob{&blob}, cellProofs)
	if err != nil {
		fmt.Printf("MakeSidecar FAILED: %v\n", err)
		return
	}
	fmt.Printf("sidecar.Version=%d blobs=%d commitments=%d proofs=%d versionedHash=%s\n",
		sidecar.Version, len(sidecar.Blobs), len(sidecar.Commitments), len(sidecar.Proofs), hashes[0].Hex())

	nonce, err := ec.PendingNonceAt(ctx, addr)
	must(err, "nonce")

	tip := big.NewInt(1e9)
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(4)), tip)
	txd := &types.BlobTx{
		ChainID:    uint256FromBig(chainID),
		Nonce:      nonce + nonceOff,
		GasTipCap:  uint256FromBig(tip),
		GasFeeCap:  uint256FromBig(feeCap),
		Gas:        250000,
		To:         inbox,
		BlobFeeCap: uint256FromBig(big.NewInt(1e9)),
		BlobHashes: hashes,
		Sidecar:    sidecar,
	}
	tx, err := types.SignNewTx(key, types.NewCancunSigner(chainID), txd)
	must(err, "sign")

	err = ec.SendTransaction(ctx, tx)
	if err != nil {
		fmt.Printf(">>> REJECTED by L1: %v\n", err)
		return
	}
	fmt.Printf(">>> ACCEPTED into txpool: %s\n", tx.Hash().Hex())
	waitMined(ctx, ec, tx.Hash(), "blob tx")
}

func waitMined(ctx context.Context, ec *ethclient.Client, h common.Hash, what string) {
	for i := 0; i < 20; i++ {
		r, err := ec.TransactionReceipt(ctx, h)
		if err == nil {
			fmt.Printf("%s mined: block=%d status=%d blobGasUsed=%d\n", what, r.BlockNumber, r.Status, r.BlobGasUsed)
			return
		}
		time.Sleep(time.Second)
	}
	fmt.Printf(">>> %s NEVER MINED (stuck in pool) after 20s\n", what)
}

func uint256FromBig(b *big.Int) *uint256.Int {
	v, _ := uint256.FromBig(b)
	return v
}

func must(err error, what string) {
	if err != nil {
		fmt.Printf("FATAL %s: %v\n", what, err)
		os.Exit(1)
	}
}
