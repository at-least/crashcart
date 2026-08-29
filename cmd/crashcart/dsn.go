package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
)

func newKey() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func dsn(cfg config.Config, p sqlc.Project) string {
	base := cfg.PublicURL
	if base == "" {
		base = "http://localhost" + cfg.Addr
	}
	scheme, host, _ := strings.Cut(base, "://")
	if host == "" {
		scheme, host = "http", base
	}
	return fmt.Sprintf("%s://%s@%s/%d", scheme, p.PublicKey, host, p.ID)
}
