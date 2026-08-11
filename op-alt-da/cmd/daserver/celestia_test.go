package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	altda "github.com/ethereum-optimism/optimism/op-alt-da"
)

func TestParseCelestiaNamespace(t *testing.T) {
	// A short ID is padded into a v0 namespace, so operators can write a readable name.
	short, err := ParseCelestiaNamespace(hex.EncodeToString([]byte("agnt2ws1")))
	require.NoError(t, err)
	raw, err := base64.StdEncoding.DecodeString(short)
	require.NoError(t, err)
	require.Len(t, raw, celestiaNamespaceLen)
	require.Equal(t, byte(0), raw[0], "padded namespaces must be version 0")
	require.Equal(t, []byte("agnt2ws1"), raw[celestiaNamespaceLen-8:], "ID must be right-aligned")

	// A full 29-byte namespace passes through unchanged, via hex or the explicit base64 form.
	full := make([]byte, celestiaNamespaceLen)
	copy(full[celestiaNamespaceLen-4:], []byte("abcd"))
	fromHex, err := ParseCelestiaNamespace(hex.EncodeToString(full))
	require.NoError(t, err)
	fromPrefixed, err := ParseCelestiaNamespace("base64:" + base64.StdEncoding.EncodeToString(full))
	require.NoError(t, err)
	require.Equal(t, fromHex, fromPrefixed, "both spellings must normalise identically")
	from0x, err := ParseCelestiaNamespace("0x" + hex.EncodeToString(full))
	require.NoError(t, err)
	require.Equal(t, fromHex, from0x)

	// Encoding is NOT sniffed. "abcdef12" is simultaneously valid hex (4 bytes) and valid
	// base64 (6 bytes), and BOTH readings are short enough to be accepted -- so a sniffing
	// parser would silently pick one, write to a namespace the operator never named, and
	// every read would come back empty with no error raised anywhere.
	const ambiguous = "abcdef12"
	asHex, err := ParseCelestiaNamespace(ambiguous)
	require.NoError(t, err)
	asB64, err := ParseCelestiaNamespace("base64:" + ambiguous)
	require.NoError(t, err)
	require.NotEqual(t, asHex, asB64,
		"both encodings parse but mean different namespaces, which is exactly why sniffing is unsafe")

	// Base64-only input without the prefix must be rejected, not silently mis-decoded.
	_, err = ParseCelestiaNamespace(base64.StdEncoding.EncodeToString(full))
	require.ErrorContains(t, err, "not valid hex")

	// Rejections that would otherwise surface only as a failed submit, or as a silent write
	// into a namespace nobody reads.
	bad := make([]byte, celestiaNamespaceLen)
	bad[0] = 1
	_, err = ParseCelestiaNamespace(hex.EncodeToString(bad))
	require.ErrorContains(t, err, "version-0")

	reserved := make([]byte, celestiaNamespaceLen)
	reserved[1] = 0xFF // inside the must-be-zero span
	_, err = ParseCelestiaNamespace(hex.EncodeToString(reserved))
	require.ErrorContains(t, err, "must be zero")

	_, err = ParseCelestiaNamespace("")
	require.Error(t, err)
}

func TestCelestiaStoreGetMissingKeyIsNotFound(t *testing.T) {
	// op-node distinguishes "not on DA yet" from "DA is broken" by this error. Returning a
	// generic error here would turn a normal not-yet-submitted read into a hard failure.
	s, err := NewCelestiaStore("http://127.0.0.1:1", "", "AAAA", t.TempDir(), 1)
	require.NoError(t, err)
	_, err = s.Get(context.Background(), []byte("never-written"))
	require.ErrorIs(t, err, altda.ErrNotFound)
}

// TestCelestiaStoreGetRefusesTruncatedBatch covers the one silent-corruption path in this
// backend. Nothing downstream would catch a short read: the alt-DA HTTP server writes store
// bytes straight to the response without inspecting them, and a Celestia backend runs in
// generic-commitment mode where GenericCommitment.Verify returns nil unconditionally. So if
// da.Get returned fewer blobs than were requested, op-node would receive a truncated batch
// that looks complete.
func TestCelestiaStoreGetRefusesTruncatedBatch(t *testing.T) {
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(body, &req))
		method = req.Method
		switch req.Method {
		case "da.Submit":
			// Two IDs for one Put, as a multi-blob submission would produce.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":["aWQx","aWQy"]}`)
		case "da.Get":
			// Only ONE blob comes back for the two IDs: the short read.
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":[%q]}`,
				base64.StdEncoding.EncodeToString([]byte("first-half-only")))
		default:
			t.Fatalf("unexpected method %q", req.Method)
		}
	}))
	defer srv.Close()

	s, err := NewCelestiaStore(srv.URL, "", "AAAA", t.TempDir(), 1)
	require.NoError(t, err)
	ctx := context.Background()
	key := []byte("truncation-key")

	require.NoError(t, s.Put(ctx, key, []byte("some batch")))
	require.Equal(t, "da.Submit", method)

	got, err := s.Get(ctx, key)
	require.Error(t, err, "a short read must be an error, never a partial batch")
	require.ErrorContains(t, err, "truncated")
	require.Nil(t, got, "no bytes may be handed back when the read was short")
}

// TestCelestiaStoreRoundTripLive is the Phase 0 kill-gate: a blob must round-trip through this
// KVStore implementation to real Celestia and back, byte-identical.
//
// It is env-gated because it spends real (testnet) TIA and needs a synced celestia-node. Run:
//
//	CELESTIA_RPC=http://localhost:26658 \
//	CELESTIA_AUTH_TOKEN=$(celestia light auth admin --p2p.network mocha \
//	    --node.store ~/.celestia-light-mocha-4-fresh) \
//	CELESTIA_NAMESPACE=$(printf 'agnt2ws1' | xxd -p) \
//	go test ./op-alt-da/cmd/daserver/ -run TestCelestiaStoreRoundTripLive -v -timeout 10m
func TestCelestiaStoreRoundTripLive(t *testing.T) {
	rpc := os.Getenv("CELESTIA_RPC")
	if rpc == "" {
		t.Skip("CELESTIA_RPC not set; skipping live Celestia round-trip")
	}
	ns, err := ParseCelestiaNamespace(os.Getenv("CELESTIA_NAMESPACE"))
	require.NoError(t, err)

	indexDir := t.TempDir()
	s, err := NewCelestiaStore(rpc, os.Getenv("CELESTIA_AUTH_TOKEN"), ns, indexDir, 1)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Random payload, so a pass cannot come from reading back some earlier run's blob that
	// happens to sit in the same namespace.
	value := make([]byte, 4096)
	_, err = rand.Read(value)
	require.NoError(t, err)
	key := make([]byte, 32)
	_, err = rand.Read(key)
	require.NoError(t, err)

	start := time.Now()
	require.NoError(t, s.Put(ctx, key, value), "submitting to Celestia must succeed")
	putLatency := time.Since(start)

	// The index is the load-bearing part of this design, so assert it exists on disk rather
	// than trusting that Put's success implies durability.
	idx, err := os.ReadFile(s.indexPath(key))
	require.NoError(t, err, "Put must persist the key -> blob-ID index before returning")
	t.Logf("PUT: %d bytes in %s; index=%s", len(value), putLatency.Round(time.Millisecond), string(idx))

	start = time.Now()
	got, err := s.Get(ctx, key)
	require.NoError(t, err)
	getLatency := time.Since(start)
	require.True(t, bytes.Equal(value, got),
		"round-tripped bytes must be identical (sent %d, got %d)", len(value), len(got))
	t.Logf("GET: %d bytes in %s", len(got), getLatency.Round(time.Millisecond))

	// A fresh store over the SAME index directory must also resolve the key: this is what a
	// daserver restart looks like, and it is the failure mode that would strand batches.
	s2, err := NewCelestiaStore(rpc, os.Getenv("CELESTIA_AUTH_TOKEN"), ns, indexDir, 1)
	require.NoError(t, err)
	got2, err := s2.Get(ctx, key)
	require.NoError(t, err, "a restarted daserver must still resolve keys it submitted earlier")
	require.True(t, bytes.Equal(value, got2))

	t.Logf("ROUND-TRIP OK: put=%s get=%s size=%d", putLatency.Round(time.Millisecond),
		getLatency.Round(time.Millisecond), len(value))
}
