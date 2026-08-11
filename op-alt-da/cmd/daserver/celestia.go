package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	altda "github.com/ethereum-optimism/optimism/op-alt-da"
)

// CelestiaStore implements altda.KVStore on top of a celestia-node JSON-RPC endpoint, so an
// OP-Stack batcher running with --altda.enabled writes its batches to Celestia and op-node
// reads them back through the same interface.
//
// WHY THIS EXISTS. The alt-DA framework is vendored here but ships only file and s3 backends.
// Without a real DA backend, "DA in the hot path" can only be approximated by a writer that
// FOLLOWS the sequencer -- which is what the earlier E-B experiment did, and its own evidence
// says so (op-geth --dev, "No op-node/op-batcher/L1 challenge path"; the writer polled
// debug_getRawBlock and idled 11.5 s/PFB waiting for blocks). Going through KVStore inverts
// that: op-batcher awaits Put before a batch counts as submitted, so DA latency lands ON the
// critical path instead of beside it.
//
// THE ADDRESSING MISMATCH, AND WHY AN INDEX EXISTS. alt-DA is key-addressed: the batcher
// derives a commitment from the batch bytes and later asks for exactly that key. Celestia is
// location-addressed: Submit returns opaque IDs (measured: 8-byte little-endian height ||
// 32-byte commitment) and Get needs those IDs plus the namespace. A Celestia locator cannot be
// computed from an alt-DA key, so Put persists key -> []ID and Get resolves through it. The
// index is fsync'd BEFORE Put returns: op-batcher treats a returned Put as "this batch is on
// DA", and an index lost after that point would strand a batch that really is on Celestia --
// to op-node, unreadable and unavailable are the same thing.
type CelestiaStore struct {
	rpc       string
	authToken string
	namespace string // base64 of the 29-byte v0 namespace
	indexDir  string
	client    *http.Client

	// Bounds in-flight submissions. Celestia serialises PFBs per ACCOUNT (CometBFT rejects
	// future nonces), so a single-signer node tops out near one PFB per Celestia block
	// (~6 s) no matter how many callers push. celestia-node's --tx.worker.accounts spreads
	// submissions over parallel-worker-* accounts and lifts that ceiling; this semaphore is
	// what lets the shim actually use the extra accounts. Sizing it above the node's worker
	// count only buys queueing, not throughput.
	inflight chan struct{}
}

func NewCelestiaStore(rpcURL, authToken, namespaceB64, indexDir string, maxInflight int) (*CelestiaStore, error) {
	if rpcURL == "" {
		return nil, fmt.Errorf("celestia rpc url is required")
	}
	if namespaceB64 == "" {
		return nil, fmt.Errorf("celestia namespace is required")
	}
	if maxInflight < 1 {
		maxInflight = 1
	}
	if err := os.MkdirAll(indexDir, 0o750); err != nil {
		return nil, fmt.Errorf("create celestia index dir: %w", err)
	}
	return &CelestiaStore{
		rpc:       rpcURL,
		authToken: authToken,
		namespace: namespaceB64,
		indexDir:  indexDir,
		client:    &http.Client{Timeout: 120 * time.Second},
		inflight:  make(chan struct{}, maxInflight),
	}, nil
}

func (s *CelestiaStore) indexPath(key []byte) string {
	return filepath.Join(s.indexDir, hex.EncodeToString(key)+".json")
}

// Put submits value to Celestia and durably records where it landed.
func (s *CelestiaStore) Put(ctx context.Context, key []byte, value []byte) error {
	select {
	case s.inflight <- struct{}{}:
		defer func() { <-s.inflight }()
	case <-ctx.Done():
		return ctx.Err()
	}

	// gasPrice -1 tells celestia-node to use its own estimate rather than a hardcoded price
	// that would go stale as the network's fee market moves.
	var ids []string
	if err := s.call(ctx, "da.Submit", []any{[]string{b64(value)}, -1, s.namespace}, &ids); err != nil {
		return fmt.Errorf("celestia da.Submit (%d bytes): %w", len(value), err)
	}
	if len(ids) == 0 {
		return fmt.Errorf("celestia da.Submit returned no IDs for %d bytes", len(value))
	}

	enc, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	// Write-then-rename so a crash mid-write cannot leave a half-written index that Get would
	// read as corrupt.
	tmp := s.indexPath(key) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(enc); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.indexPath(key))
}

// Get resolves an alt-DA key back to its Celestia blobs and returns the bytes.
func (s *CelestiaStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	raw, err := os.ReadFile(s.indexPath(key))
	if err != nil {
		if os.IsNotExist(err) {
			// The same signal the file backend gives, which op-node reads as "not available
			// yet" rather than as a hard failure.
			return nil, altda.ErrNotFound
		}
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err != nil {
		return nil, fmt.Errorf("corrupt celestia index for key %x: %w", key, err)
	}

	var blobs []string
	if err := s.call(ctx, "da.Get", []any{ids, s.namespace}, &blobs); err != nil {
		return nil, fmt.Errorf("celestia da.Get (%d ids): %w", len(ids), err)
	}
	var out []byte
	for i, b := range blobs {
		chunk, err := unb64(b)
		if err != nil {
			return nil, fmt.Errorf("decode celestia blob %d: %w", i, err)
		}
		out = append(out, chunk...)
	}
	if len(out) == 0 {
		return nil, altda.ErrNotFound
	}
	return out, nil
}

func (s *CelestiaStore) call(ctx context.Context, method string, params []any, out any) error {
	body, err := json.Marshal(map[string]any{
		"id": 1, "jsonrpc": "2.0", "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.rpc, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.authToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("celestia rpc %s: http %d: %s", method, resp.StatusCode, truncate(raw))
	}
	// A JSON-RPC error arrives with HTTP 200, so it has to be checked separately or every
	// failed submit would look like a success with an empty result.
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("celestia rpc %s: malformed response: %s", method, truncate(raw))
	}
	if envelope.Error != nil {
		return fmt.Errorf("celestia rpc %s: %d %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("celestia rpc %s: empty result", method)
	}
	return json.Unmarshal(envelope.Result, out)
}

func truncate(b []byte) string {
	const max = 512
	if len(b) > max {
		return string(b[:max]) + "..."
	}
	return string(b)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func unb64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// ParseCelestiaNamespace normalises an operator-supplied namespace to the base64 form the node
// RPC expects, accepting either hex (the form Celestia docs and explorers use) or base64 (the
// form the RPC uses). It validates the v0 layout rather than passing bytes through, because a
// malformed namespace does not fail at config time -- it fails later as a submit error, or
// worse, succeeds against a namespace nobody is reading.
func ParseCelestiaNamespace(s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("namespace is empty")
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(s); err != nil {
			return "", fmt.Errorf("namespace %q is neither hex nor base64", s)
		}
	}
	// A short input is treated as just the 10-byte user ID and padded into a v0 namespace, so
	// operators can write --celestia.namespace agnt2ws1 instead of 58 hex characters.
	if len(raw) <= celestiaNamespaceIDLen {
		id := make([]byte, celestiaNamespaceIDLen)
		copy(id[celestiaNamespaceIDLen-len(raw):], raw) // right-aligned, per the v0 convention
		full := make([]byte, celestiaNamespaceLen)
		copy(full[celestiaNamespaceLen-celestiaNamespaceIDLen:], id)
		return b64(full), nil
	}
	if len(raw) != celestiaNamespaceLen {
		return "", fmt.Errorf("namespace must be %d bytes (or <=%d to be padded), got %d",
			celestiaNamespaceLen, celestiaNamespaceIDLen, len(raw))
	}
	if raw[0] != 0 {
		return "", fmt.Errorf("only version-0 namespaces are supported, got version %d", raw[0])
	}
	for _, b := range raw[1 : celestiaNamespaceLen-celestiaNamespaceIDLen] {
		if b != 0 {
			return "", fmt.Errorf("version-0 namespace must be zero except for its trailing %d ID bytes",
				celestiaNamespaceIDLen)
		}
	}
	return b64(raw), nil
}

const (
	celestiaNamespaceLen   = 29 // 1 version byte + 28 ID bytes
	celestiaNamespaceIDLen = 10 // user-settable trailing bytes of a v0 namespace
)
