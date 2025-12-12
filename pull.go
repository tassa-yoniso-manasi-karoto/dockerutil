package dockerutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"
)

// layerInfo tracks the download state of a single layer
type layerInfo struct {
	size     int64 // Total size of layer (learned from Progress.Total)
	current  int64 // Bytes downloaded so far
	complete bool  // True if "Download complete" or "Already exists"
}

// pullState tracks layer progress across retry attempts
type pullState struct {
	layers map[string]*layerInfo
	mu     sync.Mutex
}

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

// GetImagePullBaseline returns bytes already downloaded for an image's layers.
// It starts a brief pull to discover cached layers ("Already exists") and learn
// their sizes from the progress stream. Called internally by PullImage to
// initialize progress tracking.
//
// Note: Layers marked "Already exists" that we haven't seen downloading before
// will contribute 0 bytes since Docker doesn't report their size in that status.
func GetImagePullBaseline(ctx context.Context, imageName string) (int64, error) {
	Logger.Debug().Str("image", imageName).Msg("Checking baseline for cached layers")

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return 0, fmt.Errorf("failed to create Docker client: %w", err)
	}
	defer cli.Close()

	// Check if image already exists completely
	if imgInfo, _, err := cli.ImageInspectWithRaw(ctx, imageName); err == nil {
		Logger.Debug().
			Str("image", imageName).
			Int64("size", imgInfo.Size).
			Msg("Image already fully exists")
		return imgInfo.Size, nil
	}

	// Use a timeout context to limit how long we scan the stream
	scanCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	Logger.Debug().Str("image", imageName).Msg("Starting brief pull to discover cached layers")

	reader, err := cli.ImagePull(scanCtx, imageName, image.PullOptions{})
	if err != nil {
		Logger.Debug().Err(err).Str("image", imageName).Msg("Failed to initiate baseline pull")
		return 0, fmt.Errorf("failed to initiate pull: %w", err)
	}
	defer reader.Close()

	layers := make(map[string]*layerInfo)
	var alreadyExistsCount int

	decoder := json.NewDecoder(reader)
	for {
		var msg jsonmessage.JSONMessage
		if err := decoder.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			// Context canceled or timeout - that's fine, we have what we need
			if scanCtx.Err() != nil {
				Logger.Debug().Msg("Baseline scan complete (timeout/cancel)")
				break
			}
			return 0, fmt.Errorf("failed to decode pull progress: %w", err)
		}

		if msg.ID == "" {
			continue
		}

		if layers[msg.ID] == nil {
			layers[msg.ID] = &layerInfo{}
		}
		layer := layers[msg.ID]

		// Learn size from Progress.Total
		if msg.Progress != nil && msg.Progress.Total > 0 {
			layer.size = msg.Progress.Total
			Logger.Trace().
				Str("layer", msg.ID).
				Int64("size", layer.size).
				Msg("Learned layer size")
		}

		// Mark complete if "Already exists"
		if msg.Status == "Already exists" && !layer.complete {
			layer.complete = true
			alreadyExistsCount++
			Logger.Debug().
				Str("layer", msg.ID).
				Int64("size", layer.size).
				Msg("Layer already exists (cached)")
		}

		// If we see Downloading start, we've discovered all cached layers
		if msg.Status == "Downloading" {
			Logger.Debug().
				Int("cached_layers", alreadyExistsCount).
				Int("total_layers", len(layers)).
				Msg("Download starting, baseline scan complete")
			cancel()
			break
		}
	}

	// Sum up completed layers
	var baseline int64
	var knownSizeCount int
	for _, l := range layers {
		if l.complete {
			if l.size > 0 {
				baseline += l.size
				knownSizeCount++
			}
		}
	}

	Logger.Debug().
		Int64("baseline_bytes", baseline).
		Int("cached_layers", alreadyExistsCount).
		Int("with_known_size", knownSizeCount).
		Msg("Baseline calculation complete")

	return baseline, nil
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

	// Shared state across retries - preserves layer progress info
	state := &pullState{
		layers: make(map[string]*layerInfo),
	}

	// Get baseline from any previously cached layers to initialize progress correctly
	baseline, err := GetImagePullBaseline(ctx, imageName)
	if err != nil {
		Logger.Debug().Err(err).Msg("Failed to get baseline, starting from 0")
	} else if baseline > 0 {
		// Report initial baseline progress if callback provided
		if opts.OnProgress != nil {
			opts.OnProgress(baseline, 0, "Already exists")
		}
	}

	// Retry with notification
	operation := func() error {
		return doPullImage(ctx, imageName, opts, state)
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

func doPullImage(ctx context.Context, imageName string, opts PullOptions, state *pullState) error {
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

		// Track layer state (persists across retries via shared state)
		if msg.ID != "" {
			state.mu.Lock()
			if state.layers[msg.ID] == nil {
				state.layers[msg.ID] = &layerInfo{}
			}
			layer := state.layers[msg.ID]

			// Learn size from Progress.Total when downloading
			if msg.Progress != nil && msg.Progress.Total > 0 {
				layer.size = msg.Progress.Total
				layer.current = msg.Progress.Current
			}

			// Mark layer as complete on these statuses
			if msg.Status == "Download complete" || msg.Status == "Already exists" || msg.Status == "Pull complete" {
				layer.complete = true
				// For completed layers, current should equal size
				if layer.size > 0 {
					layer.current = layer.size
				}
			}
			state.mu.Unlock()
		}

		// Calculate cumulative progress: completed layers + in-progress layers
		state.mu.Lock()
		var completedBytes, inProgressBytes int64
		for _, l := range state.layers {
			if l.complete {
				completedBytes += l.size
			} else {
				inProgressBytes += l.current
			}
		}
		totalBytes := completedBytes + inProgressBytes
		state.mu.Unlock()

		// Report cumulative progress if callback provided
		if opts.OnProgress != nil && totalBytes > 0 {
			opts.OnProgress(totalBytes, 0, msg.Status)
		}
	}

	// Verify image was pulled successfully
	if _, _, err := cli.ImageInspectWithRaw(ctx, imageName); err != nil {
		return fmt.Errorf("image verification failed after pull: %w", err)
	}

	Logger.Info().Str("image", imageName).Msg("Image pull complete and verified")
	return nil
}
