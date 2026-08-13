// l1cl -- a minimal L1 consensus layer for the AGNT2 WS1 harness.
//
// It drives a STOCK, UNMODIFIED op-geth over the authenticated engine API at a fixed block
// time, and serves the four beacon-API endpoints op-node actually calls, backed by the
// BlobsBundle that geth returns from engine_getPayloadV{3,4,5}.
//
// This replaces `geth --dev` (whose SimulatedBeacon computes blob hashes and then DISCARDS
// the blob bodies, leaving nothing for op-node to derive from) with the same FakePoS +
// fakebeacon pair the optimism devstack already uses against a geth subprocess.
//
// Nothing here is new logic: FakePoS, fakebeacon and blobstore are upstream op-e2e packages,
// and the engine client is the same JWT-authenticated RPC adapter as op-devstack/sysgo.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	gn "github.com/ethereum/go-ethereum/node"
	gethrpc "github.com/ethereum/go-ethereum/rpc"

	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/blobstore"
	"github.com/ethereum-optimism/optimism/op-e2e/e2eutils/fakebeacon"
	e2egeth "github.com/ethereum-optimism/optimism/op-e2e/e2eutils/geth"
	"github.com/ethereum-optimism/optimism/op-service/clock"
)

// ---- engine client: same adapter as op-devstack/sysgo/engine_client.go (unexported there) ----

type engineClient struct{ inner *gethrpc.Client }

var _ e2egeth.EngineAPI = (*engineClient)(nil)

func dialEngine(ctx context.Context, endpoint string, jwtSecret [32]byte) (*engineClient, error) {
	c, err := gethrpc.DialOptions(ctx, endpoint, gethrpc.WithHTTPAuth(gn.NewJWTAuth(jwtSecret)))
	if err != nil {
		return nil, err
	}
	return &engineClient{inner: c}, nil
}

func (e *engineClient) fcu(ctx context.Context, fs engine.ForkchoiceStateV1, pa *engine.PayloadAttributes, method string) (engine.ForkChoiceResponse, error) {
	var x engine.ForkChoiceResponse
	if err := e.inner.CallContext(ctx, &x, method, fs, pa); err != nil {
		return engine.ForkChoiceResponse{}, err
	}
	return x, nil
}

func (e *engineClient) ForkchoiceUpdatedV2(ctx context.Context, fs engine.ForkchoiceStateV1, pa *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	return e.fcu(ctx, fs, pa, "engine_forkchoiceUpdatedV2")
}

func (e *engineClient) ForkchoiceUpdatedV3(ctx context.Context, fs engine.ForkchoiceStateV1, pa *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	return e.fcu(ctx, fs, pa, "engine_forkchoiceUpdatedV3")
}

func (e *engineClient) getPayload(id engine.PayloadID, method string) (*engine.ExecutionPayloadEnvelope, error) {
	var r engine.ExecutionPayloadEnvelope
	if err := e.inner.CallContext(context.Background(), &r, method, id); err != nil {
		return nil, err
	}
	return &r, nil
}

func (e *engineClient) GetPayloadV2(id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	return e.getPayload(id, "engine_getPayloadV2")
}
func (e *engineClient) GetPayloadV3(id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	return e.getPayload(id, "engine_getPayloadV3")
}
func (e *engineClient) GetPayloadV4(id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	return e.getPayload(id, "engine_getPayloadV4")
}
func (e *engineClient) GetPayloadV5(id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	return e.getPayload(id, "engine_getPayloadV5")
}

func (e *engineClient) NewPayloadV2(ctx context.Context, data engine.ExecutableData) (engine.PayloadStatusV1, error) {
	var r engine.PayloadStatusV1
	err := e.inner.CallContext(ctx, &r, "engine_newPayloadV2", data)
	return r, err
}

func (e *engineClient) NewPayloadV3(ctx context.Context, data engine.ExecutableData, vh []common.Hash, br *common.Hash) (engine.PayloadStatusV1, error) {
	var r engine.PayloadStatusV1
	err := e.inner.CallContext(ctx, &r, "engine_newPayloadV3", data, vh, br)
	return r, err
}

func (e *engineClient) NewPayloadV4(ctx context.Context, data engine.ExecutableData, vh []common.Hash, br *common.Hash, reqs []hexutil.Bytes) (engine.PayloadStatusV1, error) {
	var r engine.PayloadStatusV1
	err := e.inner.CallContext(ctx, &r, "engine_newPayloadV4", data, vh, br, reqs)
	return r, err
}

// ---- main ----

func main() {
	var (
		ethRPC     = flag.String("eth-rpc", "http://127.0.0.1:8545", "geth user RPC (eth namespace)")
		engineRPC  = flag.String("engine-rpc", "http://127.0.0.1:8551", "geth authenticated engine RPC")
		jwtPath    = flag.String("jwt", "/cfg/jwt.txt", "hex-encoded 32-byte JWT secret file")
		genesisRaw = flag.String("l1-genesis", "/cfg/l1-genesis.json", "the SAME l1-genesis.json geth was init'd from")
		beaconAddr = flag.String("beacon-addr", "0.0.0.0:5052", "listen address for the beacon API")
		blockTime  = flag.Uint64("block-time", 2, "L1 block time in seconds; also SECONDS_PER_SLOT")
		finalDist  = flag.Uint64("finalized-distance", 20, "blocks behind head to report finalized")
	)
	flag.Parse()

	logger := log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stdout, log.LevelInfo, true))

	// Chain config and genesis time come from the SAME file geth was init'd from, so the
	// fork schedule the CL believes and the one the EL enforces cannot drift.
	gb, err := os.ReadFile(*genesisRaw)
	if err != nil {
		logger.Crit("read l1-genesis", "err", err)
	}
	var gen core.Genesis
	if err := json.Unmarshal(gb, &gen); err != nil {
		logger.Crit("parse l1-genesis", "err", err)
	}
	if gen.Config == nil {
		logger.Crit("l1-genesis has no config")
	}

	sb, err := os.ReadFile(*jwtPath)
	if err != nil {
		logger.Crit("read jwt", "err", err)
	}
	secret, err := hexutil.Decode("0x" + strings.TrimPrefix(strings.TrimSpace(string(sb)), "0x"))
	if err != nil || len(secret) != 32 {
		logger.Crit("jwt must be 32 hex-encoded bytes", "err", err, "len", len(secret))
	}
	var jwt [32]byte
	copy(jwt[:], secret)

	ctx := context.Background()
	backend, err := ethclient.DialContext(ctx, *ethRPC)
	if err != nil {
		logger.Crit("dial eth rpc", "err", err)
	}
	engineCl, err := dialEngine(ctx, *engineRPC, jwt)
	if err != nil {
		logger.Crit("dial engine rpc", "err", err)
	}

	store := blobstore.New()
	bcn := fakebeacon.NewBeacon(logger.New("component", "l1cl-beacon"), store, gen.Timestamp, *blockTime)
	if err := bcn.Start(*beaconAddr); err != nil {
		logger.Crit("start beacon api", "err", err)
	}
	fp := e2egeth.NewFakePoS(backend, engineCl, clock.SystemClock, logger.New("component", "l1cl-pos"),
		*blockTime, *finalDist, bcn, gen.Config)
	if err := fp.Start(); err != nil {
		logger.Crit("start fakepos", "err", err)
	}
	fmt.Printf("L1CL_UP beacon=%s genesis_time=%d seconds_per_slot=%d chain_id=%v\n",
		*beaconAddr, gen.Timestamp, *blockTime, gen.Config.ChainID)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	_ = fp.Stop()
	_ = bcn.Close()
}
