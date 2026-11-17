//go:build windows

package main

import (
	"log"
	"os"

	"github.com/levoniust/goblin/host/internal/core"
	"github.com/levoniust/goblin/host/internal/term"
)

func runKeeper(home string) error {
	os.MkdirAll(home, 0o700)
	cfg, err := core.LoadConfig(home)
	if err != nil {
		return err
	}
	if lf, err := core.OpenLogFile(home, "keeper.log"); err == nil {
		log.SetOutput(lf)
	}
	log.Printf("keeper %s starting", core.Version)
	ctx, stop := stopContext("keeper")
	defer stop()
	return term.RunKeeper(ctx, home, cfg.KeeperPort)
}
