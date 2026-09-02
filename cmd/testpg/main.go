// Command testpg gets or creates a disposable Postgres 16 container for
// DB-backed tests and prints its connection URL on stdout.
//
// It reuses a single container named crashcart-testpg across invocations
// (`make test-db`, `make mutate`, CI) instead of starting a fresh one each
// time, and lets Docker assign the host port rather than a fixed one, so
// concurrent uses (a local run and a gremlins worktree run, say) never
// collide on a port. `docker rm -f crashcart-testpg` resets it — the next
// invocation recreates it and re-migrates its templates from scratch.
package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/ory/dockertest/v3"
)

const (
	containerName = "crashcart-testpg"
	pgUser        = "crashcart"
	pgPassword    = "crashcart"
	pgDatabase    = "crashcart"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "testpg:", err)
		os.Exit(1)
	}
}

func run() error {
	pool, err := dockertest.NewPool("")
	if err != nil {
		return fmt.Errorf("connect to docker: %w", err)
	}
	pool.MaxWait = 60 * time.Second

	resource, ok := pool.ContainerByName(containerName)
	if !ok {
		resource, err = pool.RunWithOptions(&dockertest.RunOptions{
			Name:         containerName,
			Repository:   "postgres",
			Tag:          "16-alpine",
			Env:          []string{"POSTGRES_USER=" + pgUser, "POSTGRES_PASSWORD=" + pgPassword, "POSTGRES_DB=" + pgDatabase},
			ExposedPorts: []string{"5432/tcp"},
		})
		if err != nil {
			return fmt.Errorf("run container: %w", err)
		}
	} else if !resource.Container.State.Running {
		if err := pool.Client.StartContainer(resource.Container.ID, nil); err != nil {
			return fmt.Errorf("start existing container: %w", err)
		}
		resource, ok = pool.ContainerByName(containerName)
		if !ok {
			return fmt.Errorf("container %s vanished after start", containerName)
		}
	}

	hostPort := resource.GetHostPort("5432/tcp")
	if hostPort == "" {
		return fmt.Errorf("container %s published no port for 5432/tcp", containerName)
	}
	url := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", pgUser, pgPassword, hostPort, pgDatabase)

	if err := pool.Retry(func() error {
		conn, err := sql.Open("pgx", url)
		if err != nil {
			return err
		}
		defer conn.Close()
		return conn.Ping()
	}); err != nil {
		return fmt.Errorf("postgres never became ready: %w", err)
	}

	fmt.Println(url)
	return nil
}
