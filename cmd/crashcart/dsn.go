package main

import (
	"github.com/crashcartapp/crashcart/internal/auth"
	"github.com/crashcartapp/crashcart/internal/config"
	"github.com/crashcartapp/crashcart/internal/db/sqlc"
)

func dsn(cfg config.Config, p sqlc.Project) string {
	base := cfg.PublicURL
	if base == "" {
		base = "http://localhost" + cfg.Addr
	}
	return auth.DSN(base, p.PublicKey, p.ID)
}
