package dockerutil

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/jsonmessage"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
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
		MaxElapsedTime:  0, // No time limit - large images can take hours on slow connections
		ClientTimeout:   0, // No HTTP client timeout - body streaming can take arbitrarily long on slow connections
	}
}

// fetchImageLayers fetches remote manifest and returns raw layer objects.
// This is the single source of truth for manifest fetching.
func fetchImageLayers(ctx context.Context, imageName string) ([]v1.Layer, error) {
	ref, err := name.ParseReference(imageName)
	if err != nil {
		return nil, fmt.Errorf("failed to parse image reference: %w", err)
	}

	platform := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	remoteImg, err := remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest: %w", err)
	}

	layers, err := remoteImg.Layers()
	if err != nil {
		return nil, fmt.Errorf("failed to get layers from manifest: %w", err)
	}

	return layers, nil
}

// getManifestLayers fetches remote manifest and returns layer DiffID -> Size mapping.
// Used by GetImagePullBaseline for matching against local Docker cache.
func getManifestLayers(ctx context.Context, imageName string) (diffIDToSize map[string]int64, totalSize int64, err error) {
	layers, err := fetchImageLayers(ctx, imageName)
	if err != nil {
		return nil, 0, err
	}

	diffIDToSize = make(map[string]int64, len(layers))
	for _, l := range layers {
		diffID, err := l.DiffID()
		if err != nil {
			Logger.Debug().Err(err).Msg("Failed to get layer DiffID, skipping")
			continue
		}
		size, err := l.Size()
		if err != nil {
			Logger.Debug().Err(err).Str("diffID", diffID.String()).Msg("Failed to get layer size, skipping")
			continue
		}
		diffIDToSize[diffID.String()] = size
		totalSize += size
	}

	Logger.Debug().
		Str("image", imageName).
		Int("layer_count", len(layers)).
		Int64("total_size", totalSize).
		Msg("Fetched remote manifest")

	return diffIDToSize, totalSize, nil
}

// GetImagePullBaseline calculates bytes already cached locally for an image.
// It fetches the remote manifest (metadata only, no layer downloads) and compares
// layer DiffIDs against locally cached layers via Docker's ImageInspect API.
//
// Returns (baseline, totalSize, error) where:
// - baseline: bytes already cached locally
// - totalSize: total compressed size of all layers
func GetImagePullBaseline(ctx context.Context, imageName string) (baseline int64, totalSize int64, err error) {
	Logger.Debug().Str("image", imageName).Msg("Checking baseline for cached layers via manifest")

	diffIDToSize, totalSize, err := getManifestLayers(ctx, imageName)
	if err != nil {
		Logger.Debug().Err(err).Str("image", imageName).Msg("Failed to fetch remote manifest")
		return 0, 0, err
	}

	// Check local cache via Docker SDK
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		Logger.Debug().Err(err).Msg("Failed to create Docker client, assuming no local cache")
		return 0, totalSize, nil
	}
	defer cli.Close()

	info, _, err := cli.ImageInspectWithRaw(ctx, imageName)
	if err != nil {
		Logger.Debug().Str("image", imageName).Msg("Image not in local cache")
		return 0, totalSize, nil
	}

	// Image fully exists locally
	if info.RootFS.Layers == nil || len(info.RootFS.Layers) == 0 {
		Logger.Debug().Str("image", imageName).Int64("size", totalSize).Msg("Image fully cached (no layer info)")
		return totalSize, totalSize, nil
	}

	// Match local DiffIDs against remote to calculate cached bytes
	var matchedLayers int
	for _, localDiffID := range info.RootFS.Layers {
		if size, ok := diffIDToSize[localDiffID]; ok {
			baseline += size
			matchedLayers++
		}
	}

	Logger.Debug().
		Int64("baseline_bytes", baseline).
		Int64("total_bytes", totalSize).
		Int("matched_layers", matchedLayers).
		Int("total_layers", len(diffIDToSize)).
		Msg("Baseline calculation complete")

	return baseline, totalSize, nil
}

// PullImage pulls a Docker image with retry logic and verification
func PullImage(ctx context.Context, imageName string, opts PullOptions) error {
	// Get baseline and totalSize from manifest
	baseline, totalSize, err := GetImagePullBaseline(ctx, imageName)
	if err != nil {
		Logger.Debug().Err(err).Msg("Failed to get baseline, starting from 0")
	} else if baseline > 0 && opts.OnProgress != nil {
		// Report initial baseline progress:
		// this is pretty much never seen by the user as if incomplete
		// "Downloading" status will replace it instantly and thus this
		// is realistically only visible in case all layers are already pulled
		opts.OnProgress(baseline, totalSize, "Already installed")
	}

	// Wrap OnProgress to inject correct totalSize (doPullImage reports 0 for total)
	pullOpts := opts
	if opts.OnProgress != nil && totalSize > 0 {
		pullOpts.OnProgress = func(current, _ int64, status string) {
			opts.OnProgress(current, totalSize, status)
		}
	}

	// Create state and pull with retry
	state := &pullState{layers: make(map[string]*layerInfo)}
	return pullWithRetry(ctx, imageName, pullOpts, state)
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
		var completedLayers, totalLayers int
		for _, l := range state.layers {
			totalLayers++
			if l.complete {
				completedLayers++
				completedBytes += l.size
			} else {
				inProgressBytes += l.current
			}
		}
		totalBytes := completedBytes + inProgressBytes
		state.mu.Unlock()

		// Report cumulative progress if callback provided
		if opts.OnProgress != nil && totalBytes > 0 {
			status := fmt.Sprintf("Downloading (%d/%d layers)", completedLayers+1, totalLayers)
			switch msg.Status {
			case "Extracting":
				status = fmt.Sprintf("Extracting (%d/%d layers)", completedLayers+1, totalLayers)
			case "Pull complete":
				status = "Pull complete"
			}
			opts.OnProgress(totalBytes, 0, status)
		}
	}

	// Verify image was pulled successfully
	if _, _, err := cli.ImageInspectWithRaw(ctx, imageName); err != nil {
		return fmt.Errorf("image verification failed after pull: %w", err)
	}

	Logger.Info().Str("image", imageName).Msg("Image pull complete and verified")
	return nil
}

// pullWithRetry wraps doPullImage with exponential backoff retry logic.
// This is the single source of truth for retry configuration.
func pullWithRetry(ctx context.Context, imageName string, opts PullOptions, state *pullState) error {
	// Apply defaults
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

	var b backoff.BackOff = expBackoff
	if opts.MaxRetries > 0 {
		b = backoff.WithMaxRetries(b, opts.MaxRetries)
	}
	b = backoff.WithContext(b, ctx)

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

// ImageManifestInfo contains size information for an image
type ImageManifestInfo struct {
	ImageName  string
	TotalSize  int64
	LayerCount int
	Layers     []LayerDigestSize
}

// LayerDigestSize contains digest and size for a layer
type LayerDigestSize struct {
	Digest string
	Size   int64
}

// GetImageManifestInfo fetches manifest to get exact layer sizes without pulling.
// Uses Digest for layer identification (suitable for deduplication across images).
func GetImageManifestInfo(ctx context.Context, imageName string) (*ImageManifestInfo, error) {
	layers, err := fetchImageLayers(ctx, imageName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch manifest for %s: %w", imageName, err)
	}

	info := &ImageManifestInfo{
		ImageName:  imageName,
		LayerCount: len(layers),
		Layers:     make([]LayerDigestSize, 0, len(layers)),
	}

	for _, layer := range layers {
		digest, err := layer.Digest()
		if err != nil {
			Logger.Debug().Err(err).Msg("Failed to get layer digest, skipping")
			continue
		}
		size, err := layer.Size()
		if err != nil {
			Logger.Debug().Err(err).Str("digest", digest.String()).Msg("Failed to get layer size, skipping")
			continue
		}
		info.TotalSize += size
		info.Layers = append(info.Layers, LayerDigestSize{
			Digest: digest.String(),
			Size:   size,
		})
	}

	Logger.Debug().
		Str("image", imageName).
		Int64("total_size", info.TotalSize).
		Int("layer_count", info.LayerCount).
		Msg("Fetched manifest info")

	return info, nil
}

// GetImagesManifestInfo fetches manifests for multiple images, deduplicating shared layers.
// Returns the total size of unique layers across all images.
func GetImagesManifestInfo(ctx context.Context, imageNames []string) (totalUniqueBytes int64, layers map[string]int64, err error) {
	layers = make(map[string]int64)

	for _, imgName := range imageNames {
		info, err := GetImageManifestInfo(ctx, imgName)
		if err != nil {
			return 0, nil, fmt.Errorf("failed to get manifest for %s: %w", imgName, err)
		}

		for _, layer := range info.Layers {
			// Layers are naturally deduplicated by digest key
			if _, exists := layers[layer.Digest]; !exists {
				layers[layer.Digest] = layer.Size
				totalUniqueBytes += layer.Size
			}
		}
	}

	Logger.Debug().
		Int("image_count", len(imageNames)).
		Int("unique_layers", len(layers)).
		Int64("total_unique_bytes", totalUniqueBytes).
		Msg("Fetched manifests for multiple images")

	return totalUniqueBytes, layers, nil
}

// PullImages pulls multiple Docker images with unified progress tracking.
// Layers shared between images are deduplicated automatically.
// Progress callback receives accurate (current, total) across all images.
func PullImages(ctx context.Context, imageNames []string, opts PullOptions) error {
	if len(imageNames) == 0 {
		return nil
	}

	// For single image, just use PullImage
	if len(imageNames) == 1 {
		return PullImage(ctx, imageNames[0], opts)
	}

	// Fetch manifests for accurate total size
	var totalSize int64
	totalSize, _, err := GetImagesManifestInfo(ctx, imageNames)
	if err != nil {
		Logger.Debug().Err(err).Msg("Failed to fetch manifests, progress total will be 0")
		totalSize = 0
	}

	// Shared state across ALL images - layers dedupe naturally by digest
	state := &pullState{
		layers: make(map[string]*layerInfo),
	}

	// Wrap OnProgress to inject totalSize
	pullOpts := opts
	if opts.OnProgress != nil && totalSize > 0 {
		pullOpts.OnProgress = func(current, _ int64, status string) {
			opts.OnProgress(current, totalSize, status)
		}
	}

	// Pull each image with shared state
	for _, imgName := range imageNames {
		if err := pullWithRetry(ctx, imgName, pullOpts, state); err != nil {
			return fmt.Errorf("failed to pull %s: %w", imgName, err)
		}
	}

	return nil
}
