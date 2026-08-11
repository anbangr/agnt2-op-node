package main

import (
	"fmt"

	"github.com/urfave/cli/v2"

	altda "github.com/ethereum-optimism/optimism/op-alt-da"
	"github.com/ethereum-optimism/optimism/op-service/ctxinterrupt"
	oplog "github.com/ethereum-optimism/optimism/op-service/log"
)

func StartDAServer(cliCtx *cli.Context) error {
	if err := CheckRequired(cliCtx); err != nil {
		return err
	}

	cfg := ReadCLIConfig(cliCtx)
	if err := cfg.Check(); err != nil {
		return err
	}

	logCfg := oplog.ReadCLIConfig(cliCtx)

	l := oplog.NewLogger(oplog.AppOut(cliCtx), logCfg)
	oplog.SetGlobalLogHandler(l.Handler())

	l.Info("Initializing AltDA server...")

	var store altda.KVStore

	if cfg.FileStoreEnabled() {
		l.Info("Using file storage", "path", cfg.FileStoreDirPath)
		store = NewFileStore(cfg.FileStoreDirPath)
	} else if cfg.S3Enabled() {
		l.Info("Using S3 storage", "bucket", cfg.S3Config().Bucket)
		s3, err := NewS3Store(cfg.S3Config())
		if err != nil {
			return fmt.Errorf("failed to create S3 store: %w", err)
		}
		store = s3
	} else if cfg.CelestiaEnabled() {
		ns, err := ParseCelestiaNamespace(cfg.CelestiaNamespace)
		if err != nil {
			return fmt.Errorf("invalid celestia namespace: %w", err)
		}
		l.Info("Using Celestia storage", "rpc", cfg.CelestiaRPC, "namespace", ns,
			"index", cfg.CelestiaIndexDir, "max_inflight", cfg.CelestiaMaxInflight)
		cel, err := NewCelestiaStore(cfg.CelestiaRPC, cfg.CelestiaAuthToken, ns,
			cfg.CelestiaIndexDir, cfg.CelestiaMaxInflight)
		if err != nil {
			return fmt.Errorf("failed to create Celestia store: %w", err)
		}
		store = cel
	}

	server := altda.NewDAServer(cliCtx.String(ListenAddrFlagName), cliCtx.Int(PortFlagName), store, l, cfg.UseGenericComm)

	if err := server.Start(); err != nil {
		return fmt.Errorf("failed to start the DA server")
	} else {
		l.Info("Started DA Server")
	}

	defer func() {
		if err := server.Stop(); err != nil {
			l.Error("failed to stop DA server", "err", err)
		}
	}()

	return ctxinterrupt.Wait(cliCtx.Context)
}
