package dockerutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
)

// PullOptions configures image pull behavior
type PullOptions struct {
	MaxRetries      uint64        // Default: 3 (0 = unlimited until MaxElapsedTime)
	InitialInterval time.Duration // Default: 10s
	MaxInterval     time.Duration // Default: 60s
	MaxElapsedTime  time.Duration // Default: 0 (no limit)
	ClientTimeout   time.Duration // Default: 10m - HTTP client timeout per request
	OnProgress      func(current, total int64, status string)
	OnRetry         func(err error, duration time.Duration)
}

// DefaultPullOptions returns sensible defaults for large image pulls
func DefaultPullOptions() PullOptions {
	return PullOptions{
		MaxRetries:      3,
		InitialInterval: 10 * time.Second,
		MaxInterval:     60 * time.Second,
		MaxElapsedTime:  0,              // No time limit - large images can take hours on slow connections
		ClientTimeout:   3 * time.Minute, // HTTP timeout for headers - generous for high latency connections
	}
}

// PullImage pulls a Docker image with retry logic and verification
func PullImage(ctx context.Context, imageName string, opts PullOptions) error {
	// Apply defaults (note: MaxElapsedTime=0 means no limit, so don't override it)
	if opts.InitialInterval == 0 {
		opts.InitialInterval = 10 * time.Second
	}
	if opts.MaxInterval == 0 {
		opts.MaxInterval = 60 * time.Second
	}

	// Configure exponential backoff
	expBackoff := backoff.NewExponentialBackOff(
		backoff.WithInitialInterval(opts.InitialInterval),
		backoff.WithMaxInterval(opts.MaxInterval),
		backoff.WithMaxElapsedTime(opts.MaxElapsedTime),
	)

	// Apply max retries first, then context
	var b backoff.BackOff = expBackoff
	if opts.MaxRetries > 0 {
		b = backoff.WithMaxRetries(b, opts.MaxRetries)
	}
	b = backoff.WithContext(b, ctx)

	// Retry with notification
	operation := func() error {
		return doPullImage(ctx, imageName, opts)
	}

	notify := func(err error, duration time.Duration) {
		Logger.Warn().
			Err(err).
			Str("image", imageName).
			Dur("next_retry_in", duration).
			Msg("Image pull failed, retrying...")
		if opts.OnRetry != nil {
			opts.OnRetry(err, duration)
		}
	}

	return backoff.RetryNotify(operation, b, notify)
}

func doPullImage(ctx context.Context, imageName string, opts PullOptions) error {
	clientOpts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if opts.ClientTimeout > 0 {
		clientOpts = append(clientOpts, client.WithTimeout(opts.ClientTimeout))
	}
	cli, err := client.NewClientWithOpts(clientOpts...)
	if err != nil {
		return backoff.Permanent(fmt.Errorf("failed to create Docker client: %w", err))
	}
	defer cli.Close()

	// Check if image already exists
	if _, _, err := cli.ImageInspectWithRaw(ctx, imageName); err == nil {
		Logger.Debug().Str("image", imageName).Msg("Image already exists, skipping pull")
		return nil
	}

	Logger.Info().Str("image", imageName).Msg("Pulling Docker image")

	reader, err := cli.ImagePull(ctx, imageName, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("failed to initiate pull: %w", err)
	}
	defer reader.Close()

	// Track progress per layer to get cumulative total
	type layerProgress struct {
		current int64
		total   int64
	}
	layers := make(map[string]*layerProgress)

	// Process pull stream
	decoder := json.NewDecoder(reader)
	for {
		var msg jsonmessage.JSONMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to decode pull progress: %w", err)
		}

		// Check for layer errors
		if msg.Error != nil {
			return fmt.Errorf("pull error for layer %s: %s", msg.ID, msg.Error.Message)
		}

		// Track per-layer progress
		if msg.ID != "" && msg.Progress != nil {
			if layers[msg.ID] == nil {
				layers[msg.ID] = &layerProgress{}
			}
			layers[msg.ID].current = msg.Progress.Current
			layers[msg.ID].total = msg.Progress.Total
		}

		// Calculate cumulative progress across all layers
		var currentBytes int64
		for _, lp := range layers {
			currentBytes += lp.current
		}

		// Report cumulative progress if callback provided
		if opts.OnProgress != nil && currentBytes > 0 {
			opts.OnProgress(currentBytes, 0, msg.Status)
		}
	}

	// Verify image was pulled successfully
	if _, _, err := cli.ImageInspectWithRaw(ctx, imageName); err != nil {
		return fmt.Errorf("image verification failed after pull: %w", err)
	}

	Logger.Info().Str("image", imageName).Msg("Image pull complete and verified")
	return nil
}
