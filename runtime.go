package dockerutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	containertypes "github.com/docker/docker/api/types/container"
	networktypes "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/api/types/filters"
	"github.com/gofrs/flock"
	"github.com/google/uuid"
)

const (
	ManagedLabel     = "io.dockerutil.managed"
	ApplicationLabel = "io.dockerutil.application"
	LifecycleLabel   = "io.dockerutil.lifecycle"
	ProjectKindLabel = "io.dockerutil.project-kind"
	InstanceLabel    = "io.dockerutil.instance"
)

var (
	ErrRuntimeClosed = errors.New("docker runtime is closed")
	ErrProjectInUse  = errors.New("docker project is in use by another process")
	ErrLegacyProject = errors.New("legacy Docker project is not ownership-managed")
	ErrOwnershipMismatch = errors.New("Docker project ownership labels do not match")

	projectKindPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	projectNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

type Lifecycle string

const (
	LifecycleOwned  Lifecycle = "owned"
	LifecycleShared Lifecycle = "shared"
)

type RuntimeConfig struct {
	Application string
	RootDir     string
}

type ProjectSpec struct {
	BaseName       string
	Kind           string
	Lifecycle      Lifecycle
	CleanupService string
	CleanupCommand []string
}

type Runtime struct {
	mu          sync.Mutex
	application string
	rootDir     string
	instanceID  string
	instanceDir string
	lease       *flock.Flock
	startedAt   time.Time
	projects    map[string]*runtimeProject
	recoveryErr error
	closed      bool
}

type runtimeProject struct {
	manifest    projectManifest
	refs        int
	clientLease *flock.Flock
	project     *types.Project
	legacy      bool
	closing     bool
}

type ProjectLease struct {
	runtime  *Runtime
	key      string
	name     string
	kind     string
	lifecycle Lifecycle
	released bool
	mu       sync.Mutex
}

type runtimeManifest struct {
	Version     int               `json:"version"`
	Application string            `json:"application"`
	InstanceID string             `json:"instance_id"`
	PID        int                `json:"pid"`
	StartedAt  time.Time          `json:"started_at"`
	Projects   []projectManifest  `json:"projects"`
}

type projectManifest struct {
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	Lifecycle      Lifecycle `json:"lifecycle"`
	ScratchDir     string    `json:"scratch_dir,omitempty"`
	CleanupService string    `json:"cleanup_service,omitempty"`
	CleanupCommand []string  `json:"cleanup_command,omitempty"`
}

type runtimeContextKey struct{}

func WithRuntime(ctx context.Context, runtime *Runtime) context.Context {
	return context.WithValue(ctx, runtimeContextKey{}, runtime)
}

func RuntimeFromContext(ctx context.Context) *Runtime {
	if ctx == nil {
		return nil
	}
	runtime, _ := ctx.Value(runtimeContextKey{}).(*Runtime)
	return runtime
}

func NewRuntime(ctx context.Context, cfg RuntimeConfig) (*Runtime, error) {
	if strings.TrimSpace(cfg.Application) == "" {
		return nil, fmt.Errorf("runtime application is required")
	}
	if strings.TrimSpace(cfg.RootDir) == "" {
		return nil, fmt.Errorf("runtime root directory is required")
	}

	rootDir, err := filepath.Abs(cfg.RootDir)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime root: %w", err)
	}
	instancesDir := filepath.Join(rootDir, "instances")
	if err := os.MkdirAll(instancesDir, 0700); err != nil {
		return nil, fmt.Errorf("create runtime instances directory: %w", err)
	}

	instanceID := uuid.NewString()
	instanceDir := filepath.Join(instancesDir, instanceID)
	if err := os.Mkdir(instanceDir, 0700); err != nil {
		return nil, fmt.Errorf("create runtime instance directory: %w", err)
	}
	lease := flock.New(filepath.Join(instanceDir, "lease.lock"))
	if err := lease.Lock(); err != nil {
		_ = os.RemoveAll(instanceDir)
		return nil, fmt.Errorf("lock runtime instance lease: %w", err)
	}

	runtime := &Runtime{
		application: cfg.Application,
		rootDir:     rootDir,
		instanceID:  instanceID,
		instanceDir: instanceDir,
		lease:       lease,
		startedAt:   time.Now().UTC(),
		projects:    make(map[string]*runtimeProject),
	}
	if err := runtime.writeManifestLocked(); err != nil {
		_ = lease.Unlock()
		_ = os.RemoveAll(instanceDir)
		return nil, err
	}
	if err := runtime.reapStale(ctx); err != nil {
		runtime.recoveryErr = err
		Logger.Warn().Err(err).Msg("Docker orphan reaping was deferred")
	}
	return runtime, nil
}

func (r *Runtime) InstanceID() string {
	return r.instanceID
}

// RecoveryError reports a startup cleanup failure without disabling the runtime.
func (r *Runtime) RecoveryError() error {
	return r.recoveryErr
}

func (r *Runtime) AcquireProject(ctx context.Context, spec ProjectSpec) (*ProjectLease, error) {
	if !projectNamePattern.MatchString(spec.BaseName) {
		return nil, fmt.Errorf("invalid Docker project base name %q", spec.BaseName)
	}
	if !projectKindPattern.MatchString(spec.Kind) {
		return nil, fmt.Errorf("invalid Docker project kind %q", spec.Kind)
	}
	if spec.Lifecycle != LifecycleOwned && spec.Lifecycle != LifecycleShared {
		return nil, fmt.Errorf("invalid Docker project lifecycle %q", spec.Lifecycle)
	}
	if (spec.CleanupService == "") != (len(spec.CleanupCommand) == 0) {
		return nil, fmt.Errorf("cleanup service and command must be configured together")
	}
	if spec.CleanupService != "" && !projectKindPattern.MatchString(spec.CleanupService) {
		return nil, fmt.Errorf("invalid cleanup service %q", spec.CleanupService)
	}
	if spec.Lifecycle == LifecycleShared && spec.CleanupService != "" {
		return nil, fmt.Errorf("shared projects cannot configure instance scratch cleanup")
	}

	key := string(spec.Lifecycle) + ":" + spec.Kind + ":" + spec.BaseName
	projectName := spec.BaseName
	if spec.Lifecycle == LifecycleOwned {
		projectName += "-" + strings.ReplaceAll(r.instanceID, "-", "")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrRuntimeClosed
	}
	if existing := r.projects[key]; existing != nil {
		if existing.closing {
			r.mu.Unlock()
			return nil, ErrProjectInUse
		}
		existing.refs++
		name := existing.manifest.Name
		r.mu.Unlock()
		return &ProjectLease{runtime: r, key: key, name: name, kind: spec.Kind, lifecycle: spec.Lifecycle}, nil
	}
	for _, existing := range r.projects {
		if existing.manifest.Name == projectName {
			r.mu.Unlock()
			return nil, fmt.Errorf("Docker project name %s is already registered with another project kind", projectName)
		}
	}
	r.mu.Unlock()

	project := &runtimeProject{
		manifest: projectManifest{
			Name:           projectName,
			Kind:           spec.Kind,
			Lifecycle:      spec.Lifecycle,
			CleanupService: spec.CleanupService,
			CleanupCommand: append([]string(nil), spec.CleanupCommand...),
		},
		refs: 1,
	}
	if spec.Lifecycle == LifecycleShared {
		clientLease, err := r.acquireSharedClientLease(ctx, projectName)
		if err != nil {
			return nil, err
		}
		project.clientLease = clientLease
		if err := r.writeSharedMetadata(project.manifest); err != nil {
			_ = clientLease.Unlock()
			_ = os.Remove(clientLease.Path())
			return nil, err
		}
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		if project.clientLease != nil {
			_ = project.clientLease.Unlock()
			_ = os.Remove(project.clientLease.Path())
		}
		return nil, ErrRuntimeClosed
	}
	if existing := r.projects[key]; existing != nil {
		if existing.closing {
			r.mu.Unlock()
			if project.clientLease != nil {
				_ = project.clientLease.Unlock()
				_ = os.Remove(project.clientLease.Path())
			}
			return nil, ErrProjectInUse
		}
		existing.refs++
		r.mu.Unlock()
		if project.clientLease != nil {
			_ = project.clientLease.Unlock()
			_ = os.Remove(project.clientLease.Path())
		}
		return &ProjectLease{runtime: r, key: key, name: existing.manifest.Name, kind: spec.Kind, lifecycle: spec.Lifecycle}, nil
	}
	for _, existing := range r.projects {
		if existing.manifest.Name == projectName {
			r.mu.Unlock()
			if project.clientLease != nil {
				_ = project.clientLease.Unlock()
				_ = os.Remove(project.clientLease.Path())
			}
			return nil, fmt.Errorf("Docker project name %s is already registered with another project kind", projectName)
		}
	}
	r.projects[key] = project
	if err := r.writeManifestLocked(); err != nil {
		delete(r.projects, key)
		r.mu.Unlock()
		if project.clientLease != nil {
			_ = project.clientLease.Unlock()
			_ = os.Remove(project.clientLease.Path())
		}
		return nil, err
	}
	r.mu.Unlock()

	return &ProjectLease{runtime: r, key: key, name: projectName, kind: spec.Kind, lifecycle: spec.Lifecycle}, nil
}

func (l *ProjectLease) Name() string {
	return l.name
}

func (l *ProjectLease) Lifecycle() Lifecycle {
	return l.lifecycle
}

// Release relinquishes a lease before a Docker manager has taken ownership of it.
func (l *ProjectLease) Release(ctx context.Context) error {
	return l.release(ctx, nil)
}

func (l *ProjectLease) Labels() map[string]string {
	labels := map[string]string{
		ManagedLabel:     "true",
		ApplicationLabel: l.runtime.application,
		LifecycleLabel:   string(l.lifecycle),
		ProjectKindLabel: l.kind,
	}
	if l.lifecycle == LifecycleOwned {
		labels[InstanceLabel] = l.runtime.instanceID
	}
	return labels
}

func (l *ProjectLease) ScratchDir() (string, error) {
	if l.lifecycle != LifecycleOwned {
		return "", fmt.Errorf("shared project %s cannot own an instance scratch directory", l.name)
	}
	scratchDir := filepath.Join(l.runtime.instanceDir, "projects", l.kind+"--"+l.name)
	if err := l.runtime.validateScratchPath(scratchDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(scratchDir, 0700); err != nil {
		return "", fmt.Errorf("create project scratch directory: %w", err)
	}

	l.runtime.mu.Lock()
	project := l.runtime.projects[l.key]
	if project != nil {
		rel, err := filepath.Rel(l.runtime.instanceDir, scratchDir)
		if err != nil {
			l.runtime.mu.Unlock()
			return "", err
		}
		project.manifest.ScratchDir = rel
		if err := l.runtime.writeManifestLocked(); err != nil {
			l.runtime.mu.Unlock()
			return "", err
		}
	}
	l.runtime.mu.Unlock()
	return scratchDir, nil
}

func (l *ProjectLease) bindProject(project *types.Project) error {
	if project == nil || project.Name != l.name {
		return fmt.Errorf("Compose project name does not match project lease %q", l.name)
	}
	l.runtime.mu.Lock()
	defer l.runtime.mu.Unlock()
	registered := l.runtime.projects[l.key]
	if registered == nil {
		return fmt.Errorf("project lease %s is not registered", l.name)
	}
	registered.project = project
	return nil
}

func (l *ProjectLease) markLegacy() {
	l.runtime.mu.Lock()
	if project := l.runtime.projects[l.key]; project != nil {
		project.legacy = true
	}
	l.runtime.mu.Unlock()
}

func (l *ProjectLease) isLegacy() bool {
	l.runtime.mu.Lock()
	defer l.runtime.mu.Unlock()
	project := l.runtime.projects[l.key]
	return project != nil && project.legacy
}

func (l *ProjectLease) lockSharedOperation(ctx context.Context, exclusive bool) (func(), error) {
	if l.lifecycle != LifecycleShared {
		return func() {}, nil
	}
	l.runtime.mu.Lock()
	project := l.runtime.projects[l.key]
	var ownLeasePath string
	if project != nil && project.clientLease != nil {
		ownLeasePath = project.clientLease.Path()
	}
	if exclusive && project != nil && project.refs > 1 {
		l.runtime.mu.Unlock()
		return nil, ErrProjectInUse
	}
	l.runtime.mu.Unlock()

	sharedDir := filepath.Join(l.runtime.rootDir, "shared", l.name)
	operation := flock.New(filepath.Join(sharedDir, "operation.lock"))
	lockCtx, cancel := context.WithTimeout(cleanupParent(ctx), 60*time.Second)
	locked, err := operation.TryLockContext(lockCtx, 100*time.Millisecond)
	if err != nil || !locked {
		cancel()
		if err == nil {
			err = ErrProjectInUse
		}
		return nil, err
	}
	if exclusive {
		l.runtime.mu.Lock()
		project = l.runtime.projects[l.key]
		tooManyLocalOwners := project != nil && project.refs > 1
		l.runtime.mu.Unlock()
		if tooManyLocalOwners {
			_ = operation.Unlock()
			cancel()
			return nil, ErrProjectInUse
		}
	}
	clientsDir := filepath.Join(sharedDir, "clients")
	if exclusive {
		active, err := l.runtime.removeStaleClientLeases(clientsDir, ownLeasePath)
		if err != nil || active {
			_ = operation.Unlock()
			cancel()
			if err != nil {
				return nil, err
			}
			return nil, ErrProjectInUse
		}
	}
	return func() {
		_ = operation.Unlock()
		cancel()
	}, nil
}

func (l *ProjectLease) release(ctx context.Context, down func(context.Context) error) error {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	l.mu.Unlock()

	r := l.runtime
	r.mu.Lock()
	project := r.projects[l.key]
	if project == nil {
		r.mu.Unlock()
		return nil
	}
	project.refs--
	if project.refs > 0 {
		if err := r.writeManifestLocked(); err != nil {
			r.mu.Unlock()
			return err
		}
		r.mu.Unlock()
		return nil
	}
	legacy := project.legacy
	clientLease := project.clientLease
	manifest := project.manifest
	project.closing = true
	r.mu.Unlock()
	releaseCtx, cancel := context.WithTimeout(cleanupParent(ctx), 2*time.Minute)
	defer cancel()

	var cleanupErr error
	if l.lifecycle == LifecycleOwned {
		if down != nil {
			if err := validateProjectOwnership(releaseCtx, manifest.Name, l.Labels()); err != nil {
				cleanupErr = fmt.Errorf("refusing to remove Docker project %s: %w", manifest.Name, err)
			} else {
				if err := r.runProjectCleanup(releaseCtx, manifest); err != nil {
					cleanupErr = err
				} else {
					cleanupErr = down(releaseCtx)
				}
			}
		}
		if cleanupErr == nil {
			cleanupErr = r.removeScratch(manifest)
		}
	} else {
		cleanupErr = r.releaseSharedClient(releaseCtx, manifest, clientLease, legacy, down)
	}
	if cleanupErr != nil {
		r.mu.Lock()
		project.refs = 0
		project.closing = false
		_ = r.writeManifestLocked()
		r.mu.Unlock()
		return cleanupErr
	}
	r.mu.Lock()
	if r.projects[l.key] == project && project.refs == 0 {
		delete(r.projects, l.key)
	}
	finalManifestErr := r.writeManifestLocked()
	r.mu.Unlock()
	if finalManifestErr != nil {
		return finalManifestErr
	}
	return nil
}

func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	projects := make([]*runtimeProject, 0, len(r.projects))
	for _, project := range r.projects {
		projects = append(projects, project)
	}
	r.projects = make(map[string]*runtimeProject)
	r.mu.Unlock()

	var lastErr error
	for _, project := range projects {
		manifest := project.manifest
		if manifest.Lifecycle == LifecycleOwned {
			if !project.legacy {
				expected := map[string]string{
					ManagedLabel:     "true",
					ApplicationLabel: r.application,
					LifecycleLabel:   string(LifecycleOwned),
					ProjectKindLabel: manifest.Kind,
					InstanceLabel:    r.instanceID,
				}
				if err := validateProjectOwnership(ctx, manifest.Name, expected); err != nil {
					lastErr = err
					continue
				}
				if err := r.runProjectCleanup(ctx, manifest); err != nil {
					lastErr = err
					continue
				}
				if err := downComposeProject(ctx, manifest.Name, project.project, SafeDownOptions()); err != nil {
					lastErr = err
					continue
				}
			}
			if err := r.removeScratch(manifest); err != nil {
				lastErr = err
			}
		} else if err := r.releaseSharedClient(ctx, manifest, project.clientLease, project.legacy, func(closeCtx context.Context) error {
			expected := map[string]string{
				ManagedLabel:     "true",
				ApplicationLabel: r.application,
				LifecycleLabel:   string(LifecycleShared),
				ProjectKindLabel: manifest.Kind,
			}
			if err := validateProjectOwnership(closeCtx, manifest.Name, expected); err != nil {
				return err
			}
			return downComposeProject(closeCtx, manifest.Name, project.project, SafeDownOptions())
		}); err != nil {
			lastErr = err
		}
	}

	if lastErr == nil {
		_ = os.Remove(filepath.Join(r.instanceDir, "manifest.json"))
	}
	if err := r.lease.Unlock(); err != nil && lastErr == nil {
		lastErr = err
	}
	if lastErr == nil {
		if err := os.RemoveAll(r.instanceDir); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func (r *Runtime) writeManifestLocked() error {
	projects := make([]projectManifest, 0, len(r.projects))
	for _, project := range r.projects {
		projects = append(projects, project.manifest)
	}
	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Name < projects[j].Name
	})
	manifest := runtimeManifest{
		Version:     1,
		Application: r.application,
		InstanceID: r.instanceID,
		PID:        os.Getpid(),
		StartedAt:  r.startedAt,
		Projects:   projects,
	}
	return writeJSONAtomic(filepath.Join(r.instanceDir, "manifest.json"), manifest)
}

func writeJSONAtomic(path string, value interface{}) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal Docker runtime metadata: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".metadata-*")
	if err != nil {
		return fmt.Errorf("create Docker runtime metadata: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace Docker runtime metadata: %w", err)
	}
	return nil
}

func (r *Runtime) reapStale(ctx context.Context) error {
	reaper := flock.New(filepath.Join(r.rootDir, "reaper.lock"))
	lockCtx, cancel := context.WithTimeout(cleanupParent(ctx), 30*time.Second)
	defer cancel()
	locked, err := reaper.TryLockContext(lockCtx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return nil
	}
	defer reaper.Unlock()

	if err := r.reapOwnedInstances(lockCtx); err != nil {
		return err
	}
	return r.reapSharedProjects(lockCtx)
}

func (r *Runtime) reapOwnedInstances(ctx context.Context) error {
	instancesDir := filepath.Join(r.rootDir, "instances")
	entries, err := os.ReadDir(instancesDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == r.instanceID {
			continue
		}
		if _, err := uuid.Parse(entry.Name()); err != nil {
			continue
		}
		instanceDir := filepath.Join(instancesDir, entry.Name())
		lease := flock.New(filepath.Join(instanceDir, "lease.lock"))
		locked, err := lease.TryLock()
		if err != nil || !locked {
			continue
		}
		cleaned := r.reapOwnedInstance(ctx, instanceDir, entry.Name())
		_ = lease.Unlock()
		if cleaned {
			_ = os.RemoveAll(instanceDir)
		}
	}
	return nil
}

func (r *Runtime) reapOwnedInstance(ctx context.Context, instanceDir, instanceID string) bool {
	manifestPath := filepath.Join(instanceDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return false
	}
	var manifest runtimeManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false
	}
	if manifest.Application != r.application || manifest.InstanceID != instanceID {
		return false
	}
	for _, project := range manifest.Projects {
		if project.Lifecycle != LifecycleOwned {
			continue
		}
		if !projectNamePattern.MatchString(project.Name) || !projectKindPattern.MatchString(project.Kind) {
			return false
		}
		expected := map[string]string{
			ManagedLabel:     "true",
			ApplicationLabel: r.application,
			LifecycleLabel:   string(LifecycleOwned),
			ProjectKindLabel: project.Kind,
			InstanceLabel:    instanceID,
		}
		if err := validateProjectOwnership(ctx, project.Name, expected); err != nil {
			Logger.Warn().Err(err).Str("project", project.Name).Msg("Refusing to reap Docker project")
			return false
		}
		if err := r.runProjectCleanup(ctx, project); err != nil {
			Logger.Warn().Err(err).Str("project", project.Name).Msg("Stale container scratch cleanup was incomplete")
			return false
		}
		if err := downComposeProject(ctx, project.Name, nil, SafeDownOptions()); err != nil {
			return false
		}
		if project.ScratchDir != "" {
			scratchDir := filepath.Join(instanceDir, project.ScratchDir)
			if err := validateContainedPath(instanceDir, scratchDir); err != nil {
				return false
			}
			if err := os.RemoveAll(scratchDir); err != nil {
				return false
			}
		}
	}
	return true
}

func (r *Runtime) acquireSharedClientLease(ctx context.Context, projectName string) (*flock.Flock, error) {
	sharedDir := filepath.Join(r.rootDir, "shared", projectName)
	clientsDir := filepath.Join(sharedDir, "clients")
	if err := os.MkdirAll(clientsDir, 0700); err != nil {
		return nil, err
	}
	operation := flock.New(filepath.Join(sharedDir, "operation.lock"))
	lockCtx, cancel := context.WithTimeout(cleanupParent(ctx), 30*time.Second)
	defer cancel()
	locked, err := operation.TryLockContext(lockCtx, 100*time.Millisecond)
	if err != nil || !locked {
		if err == nil {
			err = ErrProjectInUse
		}
		return nil, err
	}
	defer operation.Unlock()
	if _, err := r.removeStaleClientLeases(clientsDir, ""); err != nil {
		return nil, err
	}

	clientLease := flock.New(filepath.Join(clientsDir, r.instanceID+".lock"))
	if err := clientLease.Lock(); err != nil {
		return nil, err
	}
	return clientLease, nil
}

func (r *Runtime) releaseSharedClient(ctx context.Context, manifest projectManifest, clientLease *flock.Flock, legacy bool, down func(context.Context) error) error {
	sharedDir := filepath.Join(r.rootDir, "shared", manifest.Name)
	operation := flock.New(filepath.Join(sharedDir, "operation.lock"))
	lockCtx, cancel := context.WithTimeout(cleanupParent(ctx), 60*time.Second)
	defer cancel()
	locked, err := operation.TryLockContext(lockCtx, 100*time.Millisecond)
	if err != nil || !locked {
		if err == nil {
			err = ErrProjectInUse
		}
		return err
	}
	defer operation.Unlock()

	clientsDir := filepath.Join(sharedDir, "clients")
	ignoredPath := ""
	if clientLease != nil {
		ignoredPath = clientLease.Path()
	}
	active, err := r.removeStaleClientLeases(clientsDir, ignoredPath)
	if err != nil {
		return err
	}
	if !active && !legacy && down != nil {
		if err := down(lockCtx); err != nil {
			return err
		}
		_ = os.Remove(filepath.Join(sharedDir, "project.json"))
	}
	if clientLease != nil {
		_ = clientLease.Unlock()
		_ = os.Remove(clientLease.Path())
	}
	return nil
}

func (r *Runtime) removeStaleClientLeases(clientsDir, ignoredPath string) (bool, error) {
	entries, err := os.ReadDir(clientsDir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	active := false
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lock" {
			continue
		}
		path := filepath.Join(clientsDir, entry.Name())
		if path == ignoredPath {
			continue
		}
		lease := flock.New(path)
		locked, err := lease.TryLock()
		if err != nil {
			return true, err
		}
		if !locked {
			active = true
			continue
		}
		_ = lease.Unlock()
		_ = os.Remove(path)
	}
	return active, nil
}

func (r *Runtime) writeSharedMetadata(manifest projectManifest) error {
	dir := filepath.Join(r.rootDir, "shared", manifest.Name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(dir, "project.json"), manifest)
}

func (r *Runtime) reapSharedProjects(ctx context.Context) error {
	sharedRoot := filepath.Join(r.rootDir, "shared")
	entries, err := os.ReadDir(sharedRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !projectKindPattern.MatchString(entry.Name()) {
			continue
		}
		sharedDir := filepath.Join(sharedRoot, entry.Name())
		operation := flock.New(filepath.Join(sharedDir, "operation.lock"))
		locked, err := operation.TryLock()
		if err != nil || !locked {
			continue
		}
		clientsDir := filepath.Join(sharedDir, "clients")
		active, leaseErr := r.removeStaleClientLeases(clientsDir, "")
		if leaseErr != nil {
			_ = operation.Unlock()
			continue
		}
		if !active {
			data, readErr := os.ReadFile(filepath.Join(sharedDir, "project.json"))
			var project projectManifest
			if readErr == nil && json.Unmarshal(data, &project) == nil &&
				project.Name == entry.Name() && project.Lifecycle == LifecycleShared &&
				projectKindPattern.MatchString(project.Kind) {
				expected := map[string]string{
					ManagedLabel:     "true",
					ApplicationLabel: r.application,
					LifecycleLabel:   string(LifecycleShared),
					ProjectKindLabel: project.Kind,
				}
				if validateProjectOwnership(ctx, project.Name, expected) == nil {
					if downComposeProject(ctx, project.Name, nil, SafeDownOptions()) == nil {
						_ = os.Remove(filepath.Join(sharedDir, "project.json"))
					}
				}
			}
		}
		_ = operation.Unlock()
	}
	return nil
}

func validateProjectOwnership(ctx context.Context, projectName string, expected map[string]string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	filterArgs := filters.NewArgs(filters.Arg("label", "com.docker.compose.project="+projectName))
	containers, err := cli.ContainerList(ctx, containertypes.ListOptions{All: true, Filters: filterArgs})
	if err != nil {
		return err
	}
	networks, err := cli.NetworkList(ctx, networktypes.ListOptions{Filters: filterArgs})
	if err != nil {
		return err
	}
	volumes, err := cli.VolumeList(ctx, volume.ListOptions{Filters: filterArgs})
	if err != nil {
		return err
	}
	for _, container := range containers {
		if err := requireLabels(container.Labels, expected); err != nil {
			return err
		}
	}
	for _, network := range networks {
		if err := requireLabels(network.Labels, expected); err != nil {
			return err
		}
	}
	for _, dockerVolume := range volumes.Volumes {
		if err := requireLabels(dockerVolume.Labels, expected); err != nil {
			return err
		}
	}
	return nil
}

func requireLabels(actual, expected map[string]string) error {
	for key, value := range expected {
		if actual[key] != value {
			return fmt.Errorf("%w: label %s", ErrOwnershipMismatch, key)
		}
	}
	return nil
}

func (r *Runtime) runProjectCleanup(ctx context.Context, manifest projectManifest) error {
	if manifest.CleanupService == "" || len(manifest.CleanupCommand) == 0 {
		return nil
	}
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	defer cli.Close()
	filterArgs := filters.NewArgs(
		filters.Arg("label", "com.docker.compose.project="+manifest.Name),
		filters.Arg("label", "com.docker.compose.service="+manifest.CleanupService),
	)
	containers, err := cli.ContainerList(ctx, containertypes.ListOptions{All: true, Filters: filterArgs})
	if err != nil {
		return err
	}
	if len(containers) == 0 {
		return nil
	}
	if containers[0].State != "running" {
		if err := cli.ContainerStart(ctx, containers[0].ID, containertypes.StartOptions{}); err != nil {
			return fmt.Errorf("start cleanup container: %w", err)
		}
	}
	execResponse, err := cli.ContainerExecCreate(ctx, containers[0].ID, containertypes.ExecOptions{
		Cmd: manifest.CleanupCommand,
	})
	if err != nil {
		return err
	}
	if err := cli.ContainerExecStart(ctx, execResponse.ID, containertypes.ExecStartOptions{}); err != nil {
		return err
	}
	for {
		inspect, err := cli.ContainerExecInspect(ctx, execResponse.ID)
		if err != nil {
			return err
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return fmt.Errorf("scratch cleanup exited with code %d", inspect.ExitCode)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (r *Runtime) removeScratch(manifest projectManifest) error {
	if manifest.ScratchDir == "" {
		return nil
	}
	path := filepath.Join(r.instanceDir, manifest.ScratchDir)
	if err := r.validateScratchPath(path); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func (r *Runtime) validateScratchPath(path string) error {
	return validateContainedPath(r.instanceDir, path)
}

func validateContainedPath(root, path string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil {
		return err
	}
	if rel == "." || rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("refusing cleanup outside Docker runtime instance root")
	}
	return nil
}

func cleanupParent(ctx context.Context) context.Context {
	if ctx == nil || ctx.Err() != nil {
		return context.Background()
	}
	return ctx
}
