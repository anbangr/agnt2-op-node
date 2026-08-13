// Command beaconprobe stands up a minimal fake Beacon API and drives op-node's
// real sources.L1BeaconClient against it, to prove exactly which endpoints and
// JSON shapes op-node requires for EIP-4844 blob derivation.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/log"

	"github.com/ethereum-optimism/optimism/op-service/client"
	"github.com/ethereum-optimism/optimism/op-service/eth"
	"github.com/ethereum-optimism/optimism/op-service/sources"
)

const (
	genesisTime    = 1700000000
	secondsPerSlot = 2
)

func main() {
	lgr := log.NewLogger(log.NewTerminalHandler(os.Stdout, false))

	// The blob whose body the beacon will serve.
	var blob eth.Blob
	if err := blob.FromData(eth.Data("agnt2 beacon probe payload")); err != nil {
		panic(err)
	}
	commitment, err := blob.ComputeKZGCommitment()
	if err != nil {
		panic(err)
	}
	versionedHash := eth.KZGToVersionedHash(commitment)
	fmt.Printf("serving blob with versionedHash=%s\n", versionedHash.Hex())

	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path+"?"+r.URL.RawQuery)
		fmt.Printf("  BEACON HIT: %s %s?%s\n", r.Method, r.URL.Path, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/eth/v1/node/version":
			_, _ = w.Write([]byte(`{"data":{"version":"agnt2-beacon-shim/v0.1.0"}}`))
		case r.URL.Path == "/eth/v1/config/spec":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"SECONDS_PER_SLOT":"%d"}}`, secondsPerSlot)))
		case r.URL.Path == "/eth/v1/beacon/genesis":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"genesis_time":"%d"}}`, genesisTime)))
		case len(r.URL.Path) > len("/eth/v1/beacon/blobs/") && r.URL.Path[:len("/eth/v1/beacon/blobs/")] == "/eth/v1/beacon/blobs/":
			want := r.URL.Query()["versioned_hashes"]
			fmt.Printf("    slot=%s versioned_hashes=%v\n", r.URL.Path[len("/eth/v1/beacon/blobs/"):], want)
			out := struct {
				Data []*eth.Blob `json:"data"`
			}{Data: []*eth.Blob{&blob}}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	httpCl := client.NewBasicHTTPClient(srv.URL, lgr)
	bcl := sources.NewL1BeaconClient(sources.NewBeaconHTTPClient(httpCl), sources.L1BeaconClientConfig{})
	ctx := context.Background()

	v, err := bcl.GetVersion(ctx)
	fmt.Printf("GetVersion -> %q err=%v\n", v, err)

	// L1 block time -> slot. slot = (time - genesis_time)/SECONDS_PER_SLOT
	l1BlockTime := uint64(genesisTime + 2*777)
	fmt.Printf("\nGetBlobsByHash(l1BlockTime=%d, hashes=[%s])\n", l1BlockTime, versionedHash.Hex())
	blobs, err := bcl.GetBlobsByHash(ctx, l1BlockTime, []common.Hash{versionedHash})
	if err != nil {
		fmt.Printf("RESULT: ERROR %v\n", err)
	} else {
		fmt.Printf("RESULT: OK, got %d blob(s), first 16 bytes %x\n", len(blobs), blobs[0][:16])
		d, err := blobs[0].ToData()
		fmt.Printf("decoded payload=%q err=%v\n", string(d), err)
	}

	// Negative test: serve a blob that does not match the requested hash.
	fmt.Printf("\n--- negative test: request a hash the beacon does not have ---\n")
	bogus := common.HexToHash("0x01deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe")
	_, err = bcl.GetBlobsByHash(ctx, l1BlockTime, []common.Hash{bogus})
	fmt.Printf("RESULT: err=%v\n", err)

	time.Sleep(50 * time.Millisecond)
	fmt.Printf("\nendpoints actually requested by op-node:\n")
	for _, h := range hits {
		fmt.Printf("  %s\n", h)
	}
}
