package main

import (
	"errors"
	"fmt"

	"github.com/urfave/cli/v2"

	opservice "github.com/ethereum-optimism/optimism/op-service"
	oplog "github.com/ethereum-optimism/optimism/op-service/log"
)

const (
	ListenAddrFlagName        = "addr"
	PortFlagName              = "port"
	S3BucketFlagName          = "s3.bucket"
	S3EndpointFlagName        = "s3.endpoint"
	S3AccessKeyIDFlagName     = "s3.access-key-id"
	S3AccessKeySecretFlagName = "s3.access-key-secret"
	FileStorePathFlagName     = "file.path"
	GenericCommFlagName       = "generic-commitment"

	CelestiaRPCFlagName         = "celestia.rpc"
	CelestiaAuthTokenFlagName   = "celestia.auth-token"
	CelestiaNamespaceFlagName   = "celestia.namespace"
	CelestiaIndexDirFlagName    = "celestia.index-dir"
	CelestiaMaxInflightFlagName = "celestia.max-inflight"
)

const EnvVarPrefix = "OP_ALTDA_SERVER"

func prefixEnvVars(name string) []string {
	return opservice.PrefixEnvVar(EnvVarPrefix, name)
}

var (
	ListenAddrFlag = &cli.StringFlag{
		Name:    ListenAddrFlagName,
		Usage:   "server listening address",
		Value:   "127.0.0.1",
		EnvVars: prefixEnvVars("ADDR"),
	}
	PortFlag = &cli.IntFlag{
		Name:    PortFlagName,
		Usage:   "server listening port",
		Value:   3100,
		EnvVars: prefixEnvVars("PORT"),
	}
	FileStorePathFlag = &cli.StringFlag{
		Name:    FileStorePathFlagName,
		Usage:   "path to directory for file storage",
		EnvVars: prefixEnvVars("FILESTORE_PATH"),
	}
	GenericCommFlag = &cli.BoolFlag{
		Name:    GenericCommFlagName,
		Usage:   "enable generic commitments for testing. Not for production use.",
		EnvVars: prefixEnvVars("GENERIC_COMMITMENT"),
		Value:   false,
	}
	S3BucketFlag = &cli.StringFlag{
		Name:    S3BucketFlagName,
		Usage:   "bucket name for S3 storage",
		EnvVars: prefixEnvVars("S3_BUCKET"),
	}
	S3EndpointFlag = &cli.StringFlag{
		Name:    S3EndpointFlagName,
		Usage:   "endpoint for S3 storage",
		Value:   "",
		EnvVars: prefixEnvVars("S3_ENDPOINT"),
	}
	S3AccessKeyIDFlag = &cli.StringFlag{
		Name:    S3AccessKeyIDFlagName,
		Usage:   "access key id for S3 storage",
		Value:   "",
		EnvVars: prefixEnvVars("S3_ACCESS_KEY_ID"),
	}
	S3AccessKeySecretFlag = &cli.StringFlag{
		Name:    S3AccessKeySecretFlagName,
		Usage:   "access key secret for S3 storage",
		Value:   "",
		EnvVars: prefixEnvVars("S3_ACCESS_KEY_SECRET"),
	}
	CelestiaRPCFlag = &cli.StringFlag{
		Name:    CelestiaRPCFlagName,
		Usage:   "celestia-node JSON-RPC endpoint, e.g. http://localhost:26658",
		Value:   "",
		EnvVars: prefixEnvVars("CELESTIA_RPC"),
	}
	CelestiaAuthTokenFlag = &cli.StringFlag{
		Name:    CelestiaAuthTokenFlagName,
		Usage:   "bearer token for the celestia-node RPC (omit only if the node runs with --rpc.skip-auth)",
		Value:   "",
		EnvVars: prefixEnvVars("CELESTIA_AUTH_TOKEN"),
	}
	CelestiaNamespaceFlag = &cli.StringFlag{
		Name:    CelestiaNamespaceFlagName,
		Usage:   "celestia namespace as hex (0x optional), or base64 behind an explicit \"base64:\" prefix; inputs of 10 bytes or fewer are padded into a version-0 namespace",
		Value:   "",
		EnvVars: prefixEnvVars("CELESTIA_NAMESPACE"),
	}
	CelestiaIndexDirFlag = &cli.StringFlag{
		Name:    CelestiaIndexDirFlagName,
		Usage:   "directory holding the alt-DA key -> celestia blob ID index; must survive restarts or already-submitted batches become unreadable",
		Value:   "",
		EnvVars: prefixEnvVars("CELESTIA_INDEX_DIR"),
	}
	CelestiaMaxInflightFlag = &cli.IntFlag{
		Name:    CelestiaMaxInflightFlagName,
		Usage:   "maximum concurrent submissions; keep at or below celestia-node's --tx.worker.accounts, since a single signing account serialises PayForBlobs regardless",
		Value:   1,
		EnvVars: prefixEnvVars("CELESTIA_MAX_INFLIGHT"),
	}
)

var requiredFlags = []cli.Flag{
	ListenAddrFlag,
	PortFlag,
}

var optionalFlags = []cli.Flag{
	FileStorePathFlag,
	S3BucketFlag,
	S3EndpointFlag,
	S3AccessKeyIDFlag,
	S3AccessKeySecretFlag,
	GenericCommFlag,
	CelestiaRPCFlag,
	CelestiaAuthTokenFlag,
	CelestiaNamespaceFlag,
	CelestiaIndexDirFlag,
	CelestiaMaxInflightFlag,
}

func init() {
	optionalFlags = append(optionalFlags, oplog.CLIFlags(EnvVarPrefix)...)
	Flags = append(requiredFlags, optionalFlags...)
}

// Flags contains the list of configuration options available to the binary.
var Flags []cli.Flag

type CLIConfig struct {
	FileStoreDirPath  string
	S3Bucket          string
	S3Endpoint        string
	S3AccessKeyID     string
	S3AccessKeySecret string
	UseGenericComm    bool

	CelestiaRPC         string
	CelestiaAuthToken   string
	CelestiaNamespace   string
	CelestiaIndexDir    string
	CelestiaMaxInflight int
}

func ReadCLIConfig(ctx *cli.Context) CLIConfig {
	return CLIConfig{
		FileStoreDirPath:  ctx.String(FileStorePathFlagName),
		S3Bucket:          ctx.String(S3BucketFlagName),
		S3Endpoint:        ctx.String(S3EndpointFlagName),
		S3AccessKeyID:     ctx.String(S3AccessKeyIDFlagName),
		S3AccessKeySecret: ctx.String(S3AccessKeySecretFlagName),
		UseGenericComm:    ctx.Bool(GenericCommFlagName),

		CelestiaRPC:         ctx.String(CelestiaRPCFlagName),
		CelestiaAuthToken:   ctx.String(CelestiaAuthTokenFlagName),
		CelestiaNamespace:   ctx.String(CelestiaNamespaceFlagName),
		CelestiaIndexDir:    ctx.String(CelestiaIndexDirFlagName),
		CelestiaMaxInflight: ctx.Int(CelestiaMaxInflightFlagName),
	}
}

func (c CLIConfig) Check() error {
	enabled := 0
	for _, on := range []bool{c.S3Enabled(), c.FileStoreEnabled(), c.CelestiaEnabled()} {
		if on {
			enabled++
		}
	}
	if enabled == 0 {
		return errors.New("at least one storage backend must be enabled")
	}
	if enabled > 1 {
		return errors.New("only one storage backend can be enabled")
	}
	if c.S3Enabled() && (c.S3Bucket == "" || c.S3Endpoint == "" || c.S3AccessKeyID == "" || c.S3AccessKeySecret == "") {
		return errors.New("all S3 flags must be set")
	}
	if c.CelestiaEnabled() {
		if c.CelestiaNamespace == "" {
			return fmt.Errorf("--%s is required when celestia storage is enabled", CelestiaNamespaceFlagName)
		}
		// Without a durable index, a restart orphans every batch already on Celestia: the
		// blobs remain, but nothing maps an alt-DA key back to them.
		if c.CelestiaIndexDir == "" {
			return fmt.Errorf("--%s is required when celestia storage is enabled", CelestiaIndexDirFlagName)
		}
	}
	return nil
}

func (c CLIConfig) CelestiaEnabled() bool {
	return c.CelestiaRPC != ""
}

func (c CLIConfig) S3Enabled() bool {
	return !(c.S3Bucket == "" && c.S3Endpoint == "" && c.S3AccessKeyID == "" && c.S3AccessKeySecret == "")
}

func (c CLIConfig) S3Config() S3Config {
	return S3Config{
		Bucket:          c.S3Bucket,
		Endpoint:        c.S3Endpoint,
		AccessKeyID:     c.S3AccessKeyID,
		AccessKeySecret: c.S3AccessKeySecret,
	}
}

func (c CLIConfig) FileStoreEnabled() bool {
	return c.FileStoreDirPath != ""
}

func CheckRequired(ctx *cli.Context) error {
	for _, f := range requiredFlags {
		if !ctx.IsSet(f.Names()[0]) {
			return fmt.Errorf("flag %s is required", f.Names()[0])
		}
	}
	return nil
}
