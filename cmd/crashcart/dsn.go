package main

import (
	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/store"
)

func dsn(cfg config.Config, p store.Project) string {
	return dsnFor(cfg, p.ID, p.PublicKey)
}

// dsnFor builds a DSN for any key of a project, current or retired.
func dsnFor(cfg config.Config, projectID int64, publicKey string) string {
	base := cfg.PublicURL
	if base == "" {
		base = "http://localhost" + cfg.Addr
	}
	return auth.DSN(base, publicKey, projectID)
}
