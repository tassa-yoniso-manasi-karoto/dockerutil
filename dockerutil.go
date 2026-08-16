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
	"github.com/docker/compose/v5/pkg/api"
	"github.com/docker/compose/v5/pkg/compose"
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
	service        api.Compose
	ctx            context.Context
	logger         LogConsumer
	project        *types.Project
	projectName    string
	projectLease   *ProjectLease
	requiredServices []string
	onPullProgress func(current, total int64, status string)
	Timeout        Timeout
}

// Config holds configuration options for DockerManager
type Config struct {
	ProjectName      string // Deprecated: the project identity is Config.Project.Name.
	Project          *types.Project // Compose project defined in Go
	ProjectLease     *ProjectLease
	RequiredServices []string
	LogConsumer      LogConsumer
	Timeout          Timeout
	OnPullProgress   func(current, total int64, status string) // Progress callback for image pulls
}

type Timeout struct {
	Create   time.Duration
	Recreate time.Duration
	// until containers reached the running|healthy state
	Start time.Duration
}

type DownOptions struct {
	RemoveOrphans bool
	RemoveVolumes bool
	RemoveImages  string
	Timeout       time.Duration
}

func SafeDownOptions() DownOptions {
	return DownOptions{
		RemoveOrphans: true,
		RemoveVolumes: false,
		RemoveImages:  "",
		Timeout:       60 * time.Second,
	}
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
	if cfg.ProjectName != "" && cfg.ProjectName != cfg.Project.Name {
		return nil, fmt.Errorf("Config.ProjectName %q does not match Config.Project.Name %q", cfg.ProjectName, cfg.Project.Name)
	}
	if !projectNamePattern.MatchString(cfg.Project.Name) {
		return nil, fmt.Errorf("invalid Compose project name %q", cfg.Project.Name)
	}
	if cfg.ProjectLease != nil {
		if err := cfg.ProjectLease.bindProject(cfg.Project); err != nil {
			return nil, err
		}
	}

	service, err := newComposeService()
	if err != nil {
		return nil, err
	}

	ownershipLabels := map[string]string{}
	if cfg.ProjectLease != nil {
		ownershipLabels = cfg.ProjectLease.Labels()
	}

	// Compose projects created programmatically do not pass through the loader,
	// so apply its standard labels while preserving ownership labels.
	project := cfg.Project
	for name, s := range project.Services {
		if s.CustomLabels == nil {
			s.CustomLabels = types.Labels{}
		}
		s.CustomLabels[api.ProjectLabel] = project.Name
		s.CustomLabels[api.ServiceLabel] = name
		s.CustomLabels[api.VersionLabel] = api.ComposeVersion
		s.CustomLabels[api.WorkingDirLabel] = ""
		s.CustomLabels[api.ConfigFilesLabel] = ""
		s.CustomLabels[api.OneoffLabel] = "False"
		for key, value := range ownershipLabels {
			s.CustomLabels[key] = value
		}
		project.Services[name] = s
	}
	for name, network := range project.Networks {
		if network.CustomLabels == nil {
			network.CustomLabels = types.Labels{}
		}
		for key, value := range ownershipLabels {
			network.CustomLabels[key] = value
		}
		project.Networks[name] = network
	}
	for name, volume := range project.Volumes {
		if volume.CustomLabels == nil {
			volume.CustomLabels = types.Labels{}
		}
		for key, value := range ownershipLabels {
			volume.CustomLabels[key] = value
		}
		project.Volumes[name] = volume
	}

	return &DockerManager{
		service:        service,
		ctx:            ctx,
		logger:         cfg.LogConsumer,
		project:        project,
		projectName:    project.Name,
		projectLease:   cfg.ProjectLease,
		requiredServices: append([]string(nil), cfg.RequiredServices...),
		onPullProgress: cfg.OnPullProgress,
		Timeout:        cfg.Timeout,
	}, nil
}

func newComposeService() (api.Compose, error) {
	cli, err := command.NewDockerCli()
	if err != nil {
		return nil, fmt.Errorf("failed to spawn Docker CLI: %w", err)
	}
	if err := cli.Initialize(flags.NewClientOptions()); err != nil {
		return nil, fmt.Errorf("failed to initialize Docker CLI: %w", err)
	}
	service, err := compose.NewComposeService(cli)
	if err != nil {
		return nil, fmt.Errorf("failed to create Compose service: %w", err)
	}
	return service, nil
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
	unlockSharedOperation := func() {}
	if dm.projectLease != nil && dm.projectLease.Lifecycle() == LifecycleShared {
		var err error
		unlockSharedOperation, err = dm.projectLease.lockSharedOperation(dm.ctx, recreate)
		if err != nil {
			return fmt.Errorf("shared project %s is busy: %w", dm.projectName, err)
		}
		defer unlockSharedOperation()
	}

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
			if dm.projectLease != nil && dm.projectLease.Lifecycle() == LifecycleShared {
				managed, ownershipErr := dm.hasManagedOwnership()
				if ownershipErr != nil {
					return ownershipErr
				}
				if !managed {
					dm.projectLease.markLegacy()
					if recreate {
						return fmt.Errorf("%w: stop and remove project %s manually before recreating it", ErrLegacyProject, dm.projectName)
					}
					if isRunning {
						Logger.Warn().Str("project", dm.projectName).Msg("Attached to legacy shared project; automatic teardown is disabled")
						return nil
					}
				}
			}
			// If recreate was explicitly requested, tear down first to avoid orphan conflicts
			if recreate {
				Logger.Info().Msgf("%s: recreate requested, tearing down existing containers", dm.projectName)
				if err := dm.down(dm.ctx, SafeDownOptions()); err != nil {
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

	if len(dm.requiredServices) > 0 {
		return dm.validateRequiredServices()
	}
	
	status, err := dm.Status()
	if err != nil {
		return fmt.Errorf("status check failed: %w", err)
	}
	if status != api.RUNNING {
		return fmt.Errorf("services failed to reach running state for %s, current status: %s", dm.projectName, status)
	}

	return nil
}

func (dm *DockerManager) validateRequiredServices() error {
	containers, err := dm.service.Ps(dm.ctx, dm.projectName, api.PsOptions{
		All:      true,
		Services: dm.requiredServices,
	})
	if err != nil {
		return fmt.Errorf("list required services: %w", err)
	}
	states := make(map[string]string, len(containers))
	for _, container := range containers {
		states[container.Service] = string(container.State)
	}
	for _, serviceName := range dm.requiredServices {
		if states[serviceName] != "running" {
			return fmt.Errorf("service %s failed to reach running state, current state: %s", serviceName, states[serviceName])
		}
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

// Stop stops all running containers without removing the Compose project.
func (dm *DockerManager) Stop() error {
	return dm.StopWithContext(context.Background())
}

// StopWithContext stops all running containers with a caller-provided context.
func (dm *DockerManager) StopWithContext(ctx context.Context) error {
	unlockSharedOperation := func() {}
	if dm.projectLease != nil && dm.projectLease.Lifecycle() == LifecycleShared {
		var err error
		unlockSharedOperation, err = dm.projectLease.lockSharedOperation(ctx, true)
		if err != nil {
			return fmt.Errorf("shared project %s cannot be stopped: %w", dm.projectName, err)
		}
		defer unlockSharedOperation()
	}
	ctx, cancel := context.WithTimeout(cleanupParent(ctx), 30*time.Second)
	defer cancel()
	return dm.service.Stop(ctx, dm.projectName, api.StopOptions{})
}

// Close releases this manager's project lease using a background context.
func (dm *DockerManager) Close() error {
	return dm.CloseWithContext(context.Background())
}

// CloseWithContext releases this manager's project lease. The final owner
// removes the Compose project with safe teardown options.
func (dm *DockerManager) CloseWithContext(ctx context.Context) error {
	down := func(closeCtx context.Context) error {
		if dm.projectLease != nil && !dm.projectLease.isLegacy() {
			if err := validateProjectOwnership(closeCtx, dm.projectName, dm.projectLease.Labels()); err != nil {
				return fmt.Errorf("refusing to remove Docker project %s: %w", dm.projectName, err)
			}
		}
		return dm.down(closeCtx, SafeDownOptions())
	}
	if dm.projectLease != nil {
		return dm.projectLease.release(ctx, down)
	}
	return down(ctx)
}

// Down removes the Compose project without deleting images or volumes.
func (dm *DockerManager) Down() error {
	return dm.DownWithOptions(context.Background(), SafeDownOptions())
}

// DownWithOptions removes the Compose project using explicit cleanup options.
func (dm *DockerManager) DownWithOptions(ctx context.Context, opts DownOptions) error {
	unlockSharedOperation := func() {}
	if dm.projectLease != nil && dm.projectLease.Lifecycle() == LifecycleShared {
		var err error
		unlockSharedOperation, err = dm.projectLease.lockSharedOperation(ctx, true)
		if err != nil {
			return fmt.Errorf("shared project %s cannot be removed: %w", dm.projectName, err)
		}
		defer unlockSharedOperation()
	}
	if dm.projectLease != nil && !dm.projectLease.isLegacy() {
		if err := validateProjectOwnership(cleanupParent(ctx), dm.projectName, dm.projectLease.Labels()); err != nil {
			return fmt.Errorf("refusing to remove Docker project %s: %w", dm.projectName, err)
		}
	}
	return dm.down(ctx, opts)
}

func (dm *DockerManager) down(ctx context.Context, opts DownOptions) error {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(cleanupParent(ctx), opts.Timeout)
	defer cancel()
	return dm.service.Down(ctx, dm.projectName, api.DownOptions{
		Project:       dm.project,
		RemoveOrphans: opts.RemoveOrphans,
		Volumes:       opts.RemoveVolumes,
		Images:        opts.RemoveImages,
		Timeout:       &opts.Timeout,
	})
}

func downComposeProject(ctx context.Context, projectName string, project *types.Project, opts DownOptions) error {
	service, err := newComposeService()
	if err != nil {
		return err
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(cleanupParent(ctx), opts.Timeout)
	defer cancel()
	return service.Down(ctx, projectName, api.DownOptions{
		Project:       project,
		RemoveOrphans: opts.RemoveOrphans,
		Volumes:       opts.RemoveVolumes,
		Images:        opts.RemoveImages,
		Timeout:       &opts.Timeout,
	})
}

func (dm *DockerManager) hasManagedOwnership() (bool, error) {
	if dm.projectLease == nil {
		return false, nil
	}
	if err := validateProjectOwnership(dm.ctx, dm.projectName, dm.projectLease.Labels()); err != nil {
		if errors.Is(err, ErrOwnershipMismatch) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (dm *DockerManager) ContainerID(ctx context.Context, serviceName string) (string, error) {
	containers, err := dm.service.Ps(ctx, dm.projectName, api.PsOptions{
		All:      true,
		Services: []string{serviceName},
	})
	if err != nil {
		return "", err
	}
	if len(containers) == 0 {
		return "", fmt.Errorf("service %s has no container in project %s", serviceName, dm.projectName)
	}
	return containers[0].ID, nil
}

func (dm *DockerManager) PublishedPort(ctx context.Context, serviceName string, targetPort uint16) (string, int, error) {
	return dm.service.Port(ctx, dm.projectName, serviceName, targetPort, api.PortOptions{Protocol: "tcp", Index: 1})
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
