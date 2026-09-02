package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/at-least/crashcart/internal/auth"
	"github.com/at-least/crashcart/internal/db/sqlc"
	"github.com/at-least/crashcart/internal/store"
)

// userCmd: `user add <email> [name]`, `user passwd <email>`.
func userCmd(ctx context.Context, st *store.Store, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: crashcart user add <email> [name] | user passwd <email>")
	}
	email := strings.ToLower(strings.TrimSpace(args[1]))
	switch args[0] {
	case "add":
		pw, err := password()
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		name := ""
		if len(args) > 2 {
			name = args[2]
		}
		u, err := st.CreateUser(ctx, sqlc.CreateUserParams{Email: email, Name: name, PasswordHash: hash})
		if err != nil {
			return err
		}
		fmt.Printf("user %d %s\n", u.ID, u.Email)
		return nil
	case "passwd":
		u, err := st.GetUserByEmail(ctx, email)
		if err != nil {
			return fmt.Errorf("user %q: %w", email, err)
		}
		pw, err := password()
		if err != nil {
			return err
		}
		hash, err := auth.HashPassword(pw)
		if err != nil {
			return err
		}
		_, err = st.SetUserPassword(ctx, sqlc.SetUserPasswordParams{ID: u.ID, PasswordHash: hash})
		return err
	}
	return fmt.Errorf("unknown user command %q", args[0])
}

// password reads CRASHCART_PASSWORD, else one line from stdin.
func password() (string, error) {
	pw := os.Getenv("CRASHCART_PASSWORD")
	if pw == "" {
		fmt.Fprint(os.Stderr, "password: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if len(pw) < 10 {
		return "", errors.New("password needs at least 10 characters")
	}
	return pw, nil
}

// apikeyCmd: `apikey create <name>`, `apikey list`, `apikey revoke <id>`.
func apikeyCmd(ctx context.Context, st *store.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: crashcart apikey create <name> | apikey list | apikey revoke <id>")
	}
	access := &auth.Access{Store: st}
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return errors.New("usage: crashcart apikey create <name>")
		}
		row, secret, err := access.CreateAPIKey(ctx, args[1], nil)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "API key %d %q created — the secret is shown once:\n", row.ID, row.Name)
		fmt.Println(secret)
		return nil
	case "list":
		keys, err := st.ListAPIKeys(ctx)
		if err != nil {
			return err
		}
		for _, k := range keys {
			state := "active"
			if k.RevokedAt != nil {
				state = "revoked " + k.RevokedAt.Format("2006-01-02")
			}
			used := "never used"
			if k.LastUsedAt != nil {
				used = "used " + k.LastUsedAt.Format("2006-01-02 15:04")
			}
			fmt.Printf("%d\t%s\t%s…\t%s\t%s\n", k.ID, k.Name, k.Prefix, state, used)
		}
		return nil
	case "revoke":
		if len(args) < 2 {
			return errors.New("usage: crashcart apikey revoke <id>")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return err
		}
		n, err := st.RevokeAPIKey(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no active key with id %d", id)
		}
		return nil
	}
	return fmt.Errorf("unknown apikey command %q", args[0])
}
