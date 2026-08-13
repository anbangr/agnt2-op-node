// Command blobgate is the GO/NO-GO gate for the AGNT2 WS1 blob arm.
//
// It sends ONE real EIP-4844 type-3 transaction to a replica L1 (geth, no --dev, driven by
// the l1cl shim), then drives op-node's REAL sources.L1BeaconClient against the shim's beacon
// API to fetch the blob back by versioned hash. If this prints GATE PASS, every link in the
// chain that the blob arm depends on is proven: non-dev block production at Prague, blobpool
// acceptance of a version-0 sidecar, BlobsBundle capture out of engine_getPayload, the four
// beacon endpoints, slot arithmetic, and op-node's own KZG verification.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/holiman/uint256"

	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/sources"
	"github.com/ethereum-optimism/optimism/op-service/txmgr"
)

func main() {
	ethRPC := flag.String("eth-rpc", "http://127.0.0.1:18645", "L1 user RPC")
	beacon := flag.String("beacon", "http://127.0.0.1:15052", "l1cl beacon API")
	// The batcher key from batcher.yaml; prefunded 1000 ETH in the emitted l1-genesis.json.
	pk := flag.String("key", "5de4111afa1a4b94908f83103eb1f1706367c2e68ca870fc3fb9a804cdab365a", "sender key")
	inboxHex := flag.String("inbox", "0x00289c189bee4e70334629f04cd5ed602b6600eb", "batch inbox from rollup.json")
	flag.Parse()

	ctx := context.Background()
	lgr := log.NewLogger(log.NewTerminalHandler(os.Stdout, false))

	ec, err := ethclient.DialContext(ctx, *ethRPC)
	must(err, "dial l1")
	chainID, err := ec.ChainID(ctx)
	must(err, "chainid")
	key, err := crypto.HexToECDSA(*pk)
	must(err, "key")
	from := crypto.PubkeyToAddress(key.PublicKey)
	bal, err := ec.BalanceAt(ctx, from, nil)
	must(err, "balance")
	fmt.Printf("chainID=%v sender=%s balance=%v wei\n", chainID, from.Hex(), bal)
	if bal.Sign() == 0 {
		fail("sender is not prefunded on this chain")
	}

	payload := eth.Data(fmt.Sprintf("agnt2 ws1 blob-arm gate %d", time.Now().Unix()))
	var blob eth.Blob
	must(blob.FromData(payload), "encode blob")

	// cellProofs=false -> sidecar version 0, which is what op-batcher builds by default
	// (txmgr.CellProofTime defaults to MaxUint64) and the only version this L1 accepts,
	// because the emitted l1-genesis.json schedules Prague but never Osaka.
	sidecar, hashes, err := txmgr.MakeSidecar([]*eth.Blob{&blob}, false)
	must(err, "MakeSidecar")
	fmt.Printf("sidecar.Version=%d versionedHash=%s\n", sidecar.Version, hashes[0].Hex())

	head, err := ec.HeaderByNumber(ctx, nil)
	must(err, "head")
	nonce, err := ec.PendingNonceAt(ctx, from)
	must(err, "nonce")
	tip := big.NewInt(1e9)
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(4)), tip)
	tx, err := types.SignNewTx(key, types.NewCancunSigner(chainID), &types.BlobTx{
		ChainID:    u256(chainID),
		Nonce:      nonce,
		GasTipCap:  u256(tip),
		GasFeeCap:  u256(feeCap),
		Gas:        250000,
		To:         common.HexToAddress(*inboxHex),
		BlobFeeCap: u256(big.NewInt(1e9)),
		BlobHashes: hashes,
		Sidecar:    sidecar,
	})
	must(err, "sign")
	must(ec.SendTransaction(ctx, tx), "send blob tx")
	fmt.Printf("blob tx accepted: %s\n", tx.Hash().Hex())

	var rcpt *types.Receipt
	for i := 0; i < 30; i++ {
		rcpt, err = ec.TransactionReceipt(ctx, tx.Hash())
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if rcpt == nil {
		fail("blob tx never mined")
	}
	blk, err := ec.HeaderByHash(ctx, rcpt.BlockHash)
	must(err, "block header")
	fmt.Printf("MINED block=%d status=%d execGasUsed=%d blobGasUsed=%d blobGasPrice=%v blockTime=%d excessBlobGas=%v\n",
		rcpt.BlockNumber, rcpt.Status, rcpt.GasUsed, rcpt.BlobGasUsed, rcpt.BlobGasPrice, blk.Time, *blk.ExcessBlobGas)

	// Now the part that matters: op-node's own client, against the shim's beacon.
	bcl := sources.NewL1BeaconClient(
		sources.NewBeaconHTTPClient(client.NewBasicHTTPClient(*beacon, lgr)),
		sources.L1BeaconClientConfig{})
	v, err := bcl.GetVersion(ctx)
	fmt.Printf("beacon GetVersion=%q err=%v\n", v, err)

	var blobs []*eth.Blob
	for i := 0; i < 10; i++ {
		blobs, err = bcl.GetBlobsByHash(ctx, blk.Time, []common.Hash{hashes[0]})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		fail(fmt.Sprintf("GetBlobsByHash: %v", err))
	}
	got, err := blobs[0].ToData()
	must(err, "decode blob")
	if string(got) != string(payload) {
		fail(fmt.Sprintf("payload mismatch: %q != %q", string(got), string(payload)))
	}
	fmt.Printf("beacon returned %d blob(s); decoded payload=%q\n", len(blobs), string(got))
	fmt.Println("GATE PASS")
}

func u256(b *big.Int) *uint256.Int { v, _ := uint256.FromBig(b); return v }

func must(err error, what string) {
	if err != nil {
		fail(what + ": " + err.Error())
	}
}

func fail(msg string) {
	fmt.Println("GATE FAIL:", msg)
	os.Exit(1)
}
