// Command shardweave is the MySQL sharding proxy process entrypoint (M1 passthrough).
// Layout mirrors kingshard/Gaea: load config → NewServer → signal loop → blocking Run.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/acorndb/shardweave/pkg/config"
	"github.com/acorndb/shardweave/pkg/log"
	"github.com/acorndb/shardweave/pkg/server"
)

func main() {
	configPath := flag.String("config", "etc/shardweave.yaml", "path to shardweave YAML config")
	flag.Parse()

	cfg, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config failed, error: %v\n", err)
		os.Exit(1)
	}
	log.Init(cfg.Log.Level)

	svr, err := server.New(cfg, *configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "new server failed, error: %v\n", err)
		os.Exit(1)
	}

	sc := make(chan os.Signal, 1)
	signal.Notify(sc,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGUSR1,
	)

	go func() {
		for sig := range sc {
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
				slog.Info("got signal, quit", "signal", sig.String())
				svr.Close()
				return
			case syscall.SIGUSR1:
				slog.Info("reload config", "path", *configPath)
				newCfg, err := config.LoadFile(*configPath)
				if err != nil {
					slog.Error("reload parse failed; keeping previous config", "err", err)
					continue
				}
				if err := svr.UpdateConfig(newCfg); err != nil {
					slog.Error("reload apply failed; keeping previous config", "err", err)
					continue
				}
				log.SetLevel(newCfg.Log.Level)
				slog.Info("reload ok", "users", len(newCfg.Users), "log_level", newCfg.Log.Level)
			}
		}
	}()

	slog.Info("server is listening", "addr", svr.Addr(), "config", *configPath)
	_ = svr.Run()
}
