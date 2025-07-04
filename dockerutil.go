// Package dockerutil provides reusable Docker container management functionality
// for transliteration services.
package dockerutil

/*
Package dockerutil provides reusable Docker container management functionality for
transliteration services. It handles container lifecycle management, including:

- Container initialization and setup
- Building and rebuilding containers
- Starting and stopping containers
- Status monitoring
- Repository management

*/

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"runtime"

	"github.com/docker/docker/client"
	"github.com/compose-spec/compose-go/v2/cli"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/flags"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/compose"
	
	"github.com/rs/zerolog"
	"github.com/adrg/xdg"
	"github.com/gookit/color"
	"github.com/k0kubun/pp"
)

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
	service     api.Service
	ctx         context.Context
	logger      LogConsumer
	project     *types.Project
	configDir   string
	projectName string
	git         *GitManager
	Timeout     Timeout
}

// Config holds configuration options for DockerManager
type Config struct {
	ProjectName    string
	ComposeFile    string
	RemoteRepo     string
	RequiredServices []string
	LogConsumer    LogConsumer
	Timeout	       Timeout
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
	cli, err := command.NewDockerCli()
	if err != nil {
		return nil, fmt.Errorf("failed to spawn Docker CLI: %w", err)
	}

	if err := cli.Initialize(flags.NewClientOptions()); err != nil {
		return nil, fmt.Errorf("failed to initialize Docker CLI: %w", err)
	}

	service := compose.NewComposeService(cli)
	
	configDir, err := GetConfigDir(cfg.ProjectName)
	if err != nil {
		return nil, fmt.Errorf("failed to get config directory: %w", err)
	}

	git := NewGitManager(cfg.RemoteRepo, configDir)

	return &DockerManager{
		service:     service,
		ctx:         ctx,
		logger:      cfg.LogConsumer,
		configDir:   configDir,
		projectName: cfg.ProjectName,
		git:         git,
		Timeout:     cfg.Timeout,
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
	
	return dm.initialize(false, false, true)
}

// InitRecreateNoCache remove existing containers and downloads the lastest
// version of dependencies then builds and up the containers
func (dm *DockerManager) InitRecreateNoCache() error {
	return dm.initialize(true, false, true)
}

// initialize handles the core initialization logic
func (dm *DockerManager) initialize(noCache, quiet, recreate bool) error {
	if err := dm.git.EnsureRepoExists(); err != nil {
		return fmt.Errorf("failed to ensure repository: %w", err)
	}
	needsUpdate, err := dm.git.CheckIfUpdateNeeded()
	if err != nil {
		return fmt.Errorf("failed to check repository status: %w", err)
	}
	if needsUpdate {
		recreate = true
		dm.git.pull()
	}
	if err := dm.setupProject(); err != nil {
		return fmt.Errorf("setupProject() returned an error: %w", err)
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
		if stack.Name == dm.projectName && standardizeStatus(stack.Status) == api.RUNNING {
			// Even if running, check if repository needs update
			needsUpdate, err := dm.git.CheckIfUpdateNeeded()
			if err != nil {
				return fmt.Errorf("failed to check repository status: %w", err)
			}
			if !needsUpdate {
				Logger.Info().Msgf("%s containers already running and up to date", dm.projectName)
				return nil
			}
			recreate = true
			break
		}
	}
	
	if err := dm.up(noCache, quiet, recreate); err != nil {
		return fmt.Errorf("up failed: %w", err)
	}

	return nil
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
	select {
	case <-dm.logger.GetInitChan():
		Logger.Info().Msg("container initialization complete")
	case err := <-upDone:
		if err != nil {
			return fmt.Errorf("container startup failed: %w", err)
		}
	case <-time.After(to + dm.Timeout.Start):
		return fmt.Errorf("timeout waiting for containers to START")
	case <-time.After(to):
		return fmt.Errorf("timeout waiting for containers to BUILD")
	case <-dm.ctx.Done():
		return dm.ctx.Err()
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
func (dm *DockerManager) Stop() error {
	return dm.service.Stop(dm.ctx, dm.projectName, api.StopOptions{})
}

// Close implements io.Closer
func (dm *DockerManager) Close() error {
	return dm.Stop()
}

func (dm *DockerManager) Down() error {
	//Logger.Info().Msg("removing ichiran containers and resources...")
	return dm.service.Down(dm.ctx, dm.projectName, api.DownOptions{
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

// setupProject initializes the Docker Compose project configuration
func (dm *DockerManager) setupProject() error {
	if dm.project != nil {
		return nil
	}
	
	composeYAMLpath, err := FindComposeFile(dm.configDir)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("No compose file found in project's repository")
		}
		return fmt.Errorf("error searching for compose file: %w", err)
	}

	options, err := cli.NewProjectOptions(
		[]string{composeYAMLpath},
		cli.WithOsEnv,
		cli.WithDotEnv,
		cli.WithName(dm.projectName),
		cli.WithWorkingDirectory(dm.configDir),
	)
	if err != nil {
		return fmt.Errorf("failed to create project options: %w", err)
	}

	project, err := cli.ProjectFromOptions(dm.ctx, options)
	if err != nil {
		return fmt.Errorf("failed to load project: %w", err)
	}

	for name, s := range project.Services {
		s.CustomLabels = map[string]string{
			api.ProjectLabel:     project.Name,
			api.ServiceLabel:     name,
			api.VersionLabel:     api.ComposeVersion,
			api.WorkingDirLabel:  dm.configDir,
			api.ConfigFilesLabel: strings.Join(project.ComposeFiles, ","),
			api.OneoffLabel:      "False",
		}
		project.Services[name] = s
	}
	
	// fixes this line in YAML: volumes:  - ${PWD}/docker/pgdata:/var/lib/postgresql/data
	// which uses shell-substition, completely ignoring both os.Chdir and project.WorkingDir
	if dm.projectName == "ichiran" {
		project.Services["pg"].Volumes[0].Source = filepath.Join(dm.configDir, "docker/pgdata")
		project.Environment["PWD"] = dm.configDir
	}
	
	dm.project = project
	return nil
}


func (dm *DockerManager) containersNotBuilt() bool {
	// Retrieve the list of containers for the project.
	containers, err := dm.service.Ps(dm.ctx, dm.projectName, api.PsOptions{})
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

// FindComposeFile searches for a Docker Compose file in the specified directory
// following the official Compose specification naming scheme.
// It returns the full path to the first matching file found and nil error if successful,
// or empty string and error if no compose file is found or if there's an error accessing the directory.
func FindComposeFile(dirPath string) (string, error) {
	// Valid filenames according to Compose specification
	composeFiles := []string{
		"docker-compose.yml",
		"docker-compose.yaml",
		"compose.yml",
		"compose.yaml",
	}

	// Check if directory exists and is accessible
	if _, err := os.Stat(dirPath); err != nil {
		return "", err
	}

	// Look for each possible filename
	for _, filename := range composeFiles {
		filePath := filepath.Join(dirPath, filename)
		if _, err := os.Stat(filePath); err == nil {
			return filePath, nil
		}
	}

	return "", os.ErrNotExist
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


