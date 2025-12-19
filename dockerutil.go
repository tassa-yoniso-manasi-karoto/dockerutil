// Package dockerutil provides reusable Docker container management functionality
// for transliteration services.
package dockerutil

/*
Package dockerutil provides reusable Docker container management functionality for
transliteration services. It handles container lifecycle management, including:

- Container initialization and setup
- Image pulling with progress tracking
- Starting and stopping containers
- Status monitoring

*/

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/adrg/xdg"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	"github.com/docker/docker/client"

	"github.com/gookit/color"
	"github.com/k0kubun/pp"
	"github.com/rs/zerolog"
)

// ServicePortKey is the context key for passing service port information
type contextKey string
const ServicePortKey contextKey = "service.port"

var (
	// ErrNotInitialized is returned when operations are attempted before initialization
	ErrNotInitialized = errors.New("project not initialized, was Init() called?")
	
	strFailedStacks = color.Red.Sprintf("Is the required dependency %s correctly installed? ", DockerBackendName()) + "failed to list stacks: %w"
	
	// logger internal to the library:
	Logger = zerolog.Nop()
	debug = false
)

// DockerManager handles Docker container lifecycle management
type DockerManager struct {
	service        api.Service
	ctx            context.Context
	logger         LogConsumer
	project        *types.Project
	projectName    string
	onPullProgress func(current, total int64, status string)
	Timeout        Timeout
}

// Config holds configuration options for DockerManager
type Config struct {
	ProjectName      string
	Project          *types.Project // Compose project defined in Go
	RequiredServices []string
	LogConsumer      LogConsumer
	Timeout          Timeout
	OnPullProgress   func(current, total int64, status string) // Progress callback for image pulls
}

type Timeout struct {
	Create		time.Duration
	Recreate	time.Duration
	// until containers reached the running|healthy state
	Start		time.Duration
}

func init() {
	if debug {
		Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.TimeOnly}).With().Timestamp().Logger()
	}
}

// NewDockerManager creates a new Docker service manager instance
func NewDockerManager(ctx context.Context, cfg Config) (*DockerManager, error) {
	if cfg.Project == nil {
		return nil, fmt.Errorf("Config.Project is required")
	}

	cli, err := command.NewDockerCli()
	if err != nil {
		return nil, fmt.Errorf("failed to spawn Docker CLI: %w", err)
	}

	if err := cli.Initialize(flags.NewClientOptions()); err != nil {
		return nil, fmt.Errorf("failed to initialize Docker CLI: %w", err)
	}

	service := compose.NewComposeService(cli)

	// Apply custom labels to services
	project := cfg.Project
	for name, s := range project.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  "",
			api.ConfigFilesLabel: "",
			api.OneoffLabel:      "False",
		}
		project.Services[name] = s
	}

	return &DockerManager{
		service:        service,
		ctx:            ctx,
		logger:         cfg.LogConsumer,
		project:        project,
		projectName:    cfg.ProjectName,
		onPullProgress: cfg.OnPullProgress,
		Timeout:        cfg.Timeout,
	}, nil
}

// Init builds and up the containers
func (dm *DockerManager) Init() error {
	return dm.initialize(false, false, false)
}

// InitQuiet initializes with reduced logging
func (dm *DockerManager) InitQuiet() error {
	return dm.initialize(false, true, false)
}

// InitRecreate remove existing containers, builds and up new containers
func (dm *DockerManager) InitRecreate() error {
	Logger.Debug().Str("project", dm.projectName).Msg("InitRecreate called")
	return dm.initialize(false, false, true)
}

// InitRecreateNoCache remove existing containers and downloads the lastest
// version of dependencies then builds and up the containers
func (dm *DockerManager) InitRecreateNoCache() error {
	return dm.initialize(true, false, true)
}

// initialize handles the core initialization logic
func (dm *DockerManager) initialize(noCache, quiet, recreate bool) error {
	// Pull images first with progress tracking
	images := dm.getImageNames()
	if len(images) > 0 {
		opts := DefaultPullOptions()
		if dm.onPullProgress != nil {
			opts.OnProgress = dm.onPullProgress
		}
		if err := PullImages(dm.ctx, images, opts); err != nil {
			return fmt.Errorf("failed to pull images: %w", err)
		}
	}

	if dm.containersNotBuilt() {
		recreate = true
	}

	// Check if project is already running
	stacks, err := dm.service.List(dm.ctx, api.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf(strFailedStacks, err)
	}
	for _, stack := range stacks {
		if stack.Name == dm.projectName {
			isRunning := standardizeStatus(stack.Status) == api.RUNNING
			// If recreate was explicitly requested, tear down first to avoid orphan conflicts
			if recreate {
				Logger.Info().Msgf("%s: recreate requested, tearing down existing containers", dm.projectName)
				if err := dm.Down(); err != nil {
					Logger.Warn().Err(err).Msg("Down() failed, continuing anyway")
				}
				break
			}
			// Skip if already running
			if isRunning {
				Logger.Info().Msgf("%s containers already running", dm.projectName)
				return nil
			}
			break
		}
	}

	if err := dm.up(noCache, quiet, recreate); err != nil {
		return fmt.Errorf("up failed: %w", err)
	}

	Logger.Debug().Str("project", dm.projectName).Msg("initialize completed successfully")
	return nil
}

// getImageNames extracts image names from the compose project
func (dm *DockerManager) getImageNames() []string {
	var images []string
	for _, svc := range dm.project.Services {
		if svc.Image != "" {
			images = append(images, svc.Image)
		}
	}
	return images
}


// up starts the containers and waits for initialization
func (dm *DockerManager) up(noCache, quiet, recreate bool) error {
	if dm.project == nil {
		return ErrNotInitialized
	}
	r := api.RecreateNever
	to := dm.Timeout.Create
	if recreate {
		r = api.RecreateForce
		to = dm.Timeout.Recreate
	}
	if debug {
		color.Redln("noCache?", noCache)
		color.Redln("quiet?", quiet)
		color.Redln("recreate?", recreate)
		
		color.Redln("CreateTimeout", to)
		color.Redln("StartTimeout", dm.Timeout.Start)
	}
	
	upDone := make(chan error, 1)
	go func() {
		err := dm.service.Up(dm.ctx, dm.project, api.UpOptions{
			Create: api.CreateOptions{
				Build:         &api.BuildOptions{
						NoCache:  noCache,
						Quiet:    quiet,
						Services: dm.project.ServiceNames(),
						Deps:     false,
				},
				Services:      dm.project.ServiceNames(),
				RemoveOrphans: true,
				Recreate:      r,
				Timeout:       &to,
			},
			Start: api.StartOptions{
				Wait:         true,
				WaitTimeout:  dm.Timeout.Start,
				Project:      dm.project,
				Services:     dm.project.ServiceNames(),
				Attach:       dm.logger,
			},
		})
		upDone <- err
	}()
	Logger.Debug().Str("project", dm.projectName).Str("initMessage", dm.logger.GetInitMessage()).Msg("up: waiting in select")
	select {
	case <-dm.logger.GetInitChan():
		Logger.Debug().Msg("up: received from InitChan - container initialization complete")
	case err := <-upDone:
		Logger.Debug().Err(err).Msg("up: received from upDone")
		if err != nil {
			return fmt.Errorf("container startup failed: %w", err)
		}
	case <-time.After(to + dm.Timeout.Start):
		Logger.Debug().Msg("up: timeout waiting for containers to START")
		return fmt.Errorf("timeout waiting for containers to START")
	case <-time.After(to):
		Logger.Debug().Msg("up: timeout waiting for containers to BUILD")
		return fmt.Errorf("timeout waiting for containers to BUILD")
	case <-dm.ctx.Done():
		Logger.Debug().Msg("up: context cancelled")
		return dm.ctx.Err()
	}

	Logger.Debug().Str("project", dm.projectName).Msg("up: select completed")

	// Skip running check for pythainlp - it uses interactive mode
	if strings.Contains(dm.projectName, "pythainlp") {
		Logger.Debug().Msg("up: skipping running state check for pythainlp (interactive mode)")
		return nil
	}
	
	status, err := dm.Status()
	if err != nil {
		return fmt.Errorf("status check failed: %w", err)
	}
	if status != api.RUNNING {
		return fmt.Errorf("services failed to reach running state for %s, current status: %s", dm.project, status)
	}

	return nil
}



// GetClient returns the underlying Docker client
func (dm *DockerManager) GetClient() (*client.Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("failed to create Docker client: %w", err)
	}
	return cli, nil
}

// Stop stops all running containers
// Uses a fresh context to ensure cleanup succeeds even if original context was canceled
func (dm *DockerManager) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return dm.service.Stop(ctx, dm.projectName, api.StopOptions{})
}

// Close implements io.Closer
func (dm *DockerManager) Close() error {
	return dm.Stop()
}

func (dm *DockerManager) Down() error {
	// Uses a fresh context to ensure cleanup succeeds even if original context was canceled
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return dm.service.Down(ctx, dm.projectName, api.DownOptions{
		RemoveOrphans: true,
		Volumes:       true,    // Remove volumes as well
		Images:        "local", // Remove locally built images
	})
}

// Status returns the current status of containers
func (dm *DockerManager) Status() (string, error) {
	stacks, err := dm.service.List(dm.ctx, api.ListOptions{})
	if err != nil {
		return "", fmt.Errorf(strFailedStacks, err)
	}

	for _, stack := range stacks {
		if stack.Name == dm.projectName {
			return standardizeStatus(stack.Status), nil
		}
	}
	return api.UNKNOWN, nil
}

func (dm *DockerManager) containersNotBuilt() bool {
	// Retrieve the list of containers for the project (including stopped ones).
	containers, err := dm.service.Ps(dm.ctx, dm.projectName, api.PsOptions{All: true})
	if err != nil {
		return false
	}
	return len(containers) == 0
}

// GetConfigDir returns the platform-specific configuration directory
func GetConfigDir(projectName string) (string, error) {
	configPath, err := xdg.ConfigFile(projectName)
	if err != nil {
		return "", fmt.Errorf("failed to get config directory: %w", err)
	}
	if err := os.MkdirAll(configPath, 0755); err != nil {
		return "", fmt.Errorf("failed to create config directory: %w", err)
	}
	return configPath, nil
}

// standardizeStatus converts various status formats to standard api status constants
// fmt of status isn't that of api constants, I've had: running(2), Unknown
func standardizeStatus(status string) string {
	status = strings.ToUpper(status)
	switch {
	case strings.HasPrefix(status, "RUNNING"):
		return api.RUNNING
	case strings.HasPrefix(status, "STARTING"):
		return api.STARTING
	case strings.HasPrefix(status, "UPDATING"):
		return api.UPDATING
	case strings.HasPrefix(status, "REMOVING"):
		return api.REMOVING
	case strings.HasPrefix(status, "UNKNOWN"):
		return api.UNKNOWN
	default:
		return api.FAILED
	}
}

func DockerBackendName() string {
	os := strings.ToLower(runtime.GOOS)
	
	switch os {
	case "darwin", "windows":
		return "Docker Desktop"
	default:
		return "Docker Engine"
	}
}



func placeholder3456543() {
	color.Redln(" 𝒻*** 𝓎ℴ𝓊 𝒸ℴ𝓂𝓅𝒾𝓁ℯ𝓇")
	pp.Println("𝓯*** 𝔂𝓸𝓾 𝓬𝓸𝓶𝓹𝓲𝓵𝓮𝓻")
}


