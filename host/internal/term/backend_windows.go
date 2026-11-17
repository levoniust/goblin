//go:build windows

package term

import "github.com/levoniust/goblin/host/internal/core"

// NewBackend on Windows: the chats live in `goblin keeper`.
func NewBackend(cfg *core.Config) Backend { return NewKeeperBackend(core.Home(), cfg.KeeperPort) }
