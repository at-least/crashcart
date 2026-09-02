// Command testminio gets or creates a disposable MinIO container for the
// S3 blob-store tests and prints its endpoint URL on stdout — the
// cmd/testpg pattern: one reusable named container (crashcart-testminio),
// a Docker-assigned host port, `docker rm -f crashcart-testminio` to reset.
// Credentials are fixed (crashcart / crashcart12); the tests create the
// bucket they use.
package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/ory/dockertest/v3"
)

const containerName = "crashcart-testminio"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "testminio:", err)
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
			Repository:   "minio/minio",
			Tag:          "latest",
			Cmd:          []string{"server", "/data"},
			Env:          []string{"MINIO_ROOT_USER=crashcart", "MINIO_ROOT_PASSWORD=crashcart12"},
			ExposedPorts: []string{"9000/tcp"},
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

	hostPort := resource.GetHostPort("9000/tcp")
	if hostPort == "" {
		return fmt.Errorf("container %s published no port for 9000/tcp", containerName)
	}
	url := "http://" + hostPort
	if err := pool.Retry(func() error {
		res, err := http.Get(url + "/minio/health/live")
		if err != nil {
			return err
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return fmt.Errorf("health: %s", res.Status)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("minio never became ready: %w", err)
	}
	fmt.Println(url)
	return nil
}
