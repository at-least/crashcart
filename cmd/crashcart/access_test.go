package main

import (
	"context"
	"strconv"
	"testing"

	"github.com/at-least/crashcart/internal/config"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/testdb"
)

// TestProjectKeysCmd: `project-keys list <slug>` / `delete <slug> <id>` —
// the CLI parity for the retired-DSN-key list/delete flow.
func TestProjectKeysCmd(t *testing.T) {
	st := testdb.New(t)
	ctx := context.Background()
	p, err := st.CreateProject(ctx, sqlc.CreateProjectParams{Slug: "cli-keys", Name: "App", PublicKey: "k0"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{}

	if err := projectKeysCmd(ctx, st, cfg, []string{"list", "cli-keys"}); err != nil {
		t.Fatalf("list before any rotation: %v", err)
	}
	if err := projectKeysCmd(ctx, st, cfg, []string{"bogus", "cli-keys"}); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
	if err := projectKeysCmd(ctx, st, cfg, []string{"list", "no-such-project"}); err == nil {
		t.Fatal("expected an error for an unknown project")
	}

	if _, err := st.RotateProjectKey(ctx, p.ID, "k1"); err != nil {
		t.Fatal(err)
	}
	keys, err := st.ListProjectKeys(ctx, p.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("retired keys = %+v, err %v", keys, err)
	}
	if err := projectKeysCmd(ctx, st, cfg, []string{"list", "cli-keys"}); err != nil {
		t.Fatalf("list after rotation: %v", err)
	}
	if err := projectKeysCmd(ctx, st, cfg, []string{"delete", "cli-keys", "999999"}); err == nil {
		t.Fatal("expected an error deleting a nonexistent key id")
	}
	if err := projectKeysCmd(ctx, st, cfg, []string{"delete", "cli-keys", strconv.FormatInt(keys[0].ID, 10)}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if keys, err = st.ListProjectKeys(ctx, p.ID); err != nil || len(keys) != 0 {
		t.Fatalf("retired keys after delete = %+v, err %v", keys, err)
	}
}
