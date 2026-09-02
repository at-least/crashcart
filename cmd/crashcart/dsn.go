package main

import (
	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
)

func dsn(cfg config.Config, p sqlc.Project) string {
	base := cfg.PublicURL
	if base == "" {
		base = "http://localhost" + cfg.Addr
	}
	return auth.DSN(base, p.PublicKey, p.ID)
}
