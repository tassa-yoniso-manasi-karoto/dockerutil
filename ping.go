package dockerutil

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/client"
)

// EngineIsReachable verifies if Docker daemon is running and accessible
// Returns nil if Docker is reachable, otherwise returns an error with details
func EngineIsReachable() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	_, err = cli.Ping(ctx)
	if err != nil {
		if isConnectionRefused(err) {
			return fmt.Errorf("Docker daemon is not running: %w", err)
		}
		return fmt.Errorf("Docker daemon is unreachable: %w", err)
	}

	return nil
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	errMsg := err.Error()
	return errMsg == "context deadline exceeded" ||
		errMsg == "connection refused" ||
		errMsg == "Cannot connect to the Docker daemon" ||
		err == context.DeadlineExceeded
}