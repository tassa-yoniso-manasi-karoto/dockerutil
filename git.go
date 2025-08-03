
package dockerutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
)

// GitManager handles Git repository operations
type GitManager struct {
	repoURL     string
	localPath   string
	repo        *git.Repository
}

// gitProgressWriter implements io.Writer to capture git progress messages
// and log them using DockerLogger instead of writing to stdout
type gitProgressWriter struct {
	repoName string
}

// newGitProgressWriter creates a new progress writer for git operations
func newGitProgressWriter(repoURL string) *gitProgressWriter {
	return &gitProgressWriter{repoName: extractRepoName(repoURL)}
}

// Write implements io.Writer interface
func (w *gitProgressWriter) Write(p []byte) (n int, err error) {
	message := strings.TrimSpace(string(p))
	if message != "" {
		// Use DockerLogger which has ConsoleWriter configured
		DockerLogger.Debug().
			Str("repo", w.repoName).
			Msg(message)
	}
	return len(p), nil
}

// extractRepoName extracts a human-readable repository name from a git URL
// e.g., "https://github.com/tshatrov/ichiran.git" -> "github.com/tshatrov/ichiran"
func extractRepoName(repoURL string) string {
	// Remove .git suffix if present
	name := strings.TrimSuffix(repoURL, ".git")
	
	// Handle different URL formats
	if strings.HasPrefix(name, "https://") {
		name = strings.TrimPrefix(name, "https://")
	} else if strings.HasPrefix(name, "http://") {
		name = strings.TrimPrefix(name, "http://")
	} else if strings.HasPrefix(name, "git@") {
		// Handle SSH URLs like git@github.com:user/repo
		name = strings.TrimPrefix(name, "git@")
		name = strings.Replace(name, ":", "/", 1)
	}
	
	return name
}

// NewGitManager creates a new Git manager instance
func NewGitManager(repoURL, localPath string) *GitManager {
	return &GitManager{
		repoURL:   repoURL,
		localPath: localPath,
	}
}

// EnsureRepo ensures the repository exists
func (gm *GitManager) EnsureRepoExists() error {
	gitDir := filepath.Join(gm.localPath, ".git")
	exists, err := dirExists(gitDir)
	if err != nil {
		return fmt.Errorf("failed to check git directory: %w", err)
	}

	if !exists {
		DockerLogger.Info().
			Str("repo", extractRepoName(gm.repoURL)).
			Msg("Local repository does not exist. Cloning...")
		if err := gm.clone(); err != nil {
			return fmt.Errorf("failed to clone repository: %w", err)
		}
	}
	return nil
}

// CheckIfUpdateNeeded checks if the local repository needs updating
func (gm *GitManager) CheckIfUpdateNeeded() (bool, error) {
	if gm.repo == nil {
		var err error
		gm.repo, err = git.PlainOpen(gm.localPath)
		if err != nil {
			return true, fmt.Errorf("failed to open repository: %w", err)
		}
	}

	// Get the current HEAD
	head, err := gm.repo.Head()
	if err != nil {
		return true, fmt.Errorf("failed to get HEAD: %w", err)
	}

	// Get the remote reference
	remote, err := gm.repo.Remote("origin")
	if err != nil {
		return true, fmt.Errorf("failed to get remote: %w", err)
	}

	// Fetch the latest changes
	err = remote.Fetch(&git.FetchOptions{
		Force: true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return true, fmt.Errorf("failed to fetch from remote: %w", err)
	}

	// Get the remote HEAD
	refs, err := remote.List(&git.ListOptions{})
	if err != nil {
		return true, fmt.Errorf("failed to list refs: %w", err)
	}

	// Find the symbolic HEAD reference to determine default branch
	var defaultBranch string
	
	for _, ref := range refs {
		if ref.Name().String() == "HEAD" {
			// HEAD is a symbolic reference, get its target
			target := ref.Target()
			if target != "" {
				defaultBranch = target.String()
				DockerLogger.Debug().
					Str("repo", extractRepoName(gm.repoURL)).
					Str("defaultBranch", defaultBranch).
					Msg("Found default branch from symbolic HEAD")
			}
			break
		}
	}
	
	// If we couldn't resolve symbolic HEAD, try to use the current branch
	if defaultBranch == "" {
		// Get current branch name
		currentRef, err := gm.repo.Head()
		if err == nil && currentRef.Name().IsBranch() {
			defaultBranch = currentRef.Name().String()
			DockerLogger.Debug().
				Str("repo", extractRepoName(gm.repoURL)).
				Str("currentBranch", defaultBranch).
				Msg("Using current branch as default")
		}
	}
	
	// If we still don't have a branch, try common default branch names
	if defaultBranch == "" {
		commonDefaults := []string{"refs/heads/dev", "refs/heads/main", "refs/heads/master"}
		for _, branchName := range commonDefaults {
			for _, ref := range refs {
				if ref.Name().String() == branchName {
					defaultBranch = branchName
					DockerLogger.Debug().
						Str("repo", extractRepoName(gm.repoURL)).
						Str("defaultBranch", defaultBranch).
						Msg("Using common default branch name")
					break
				}
			}
			if defaultBranch != "" {
				break
			}
		}
	}
	
	// Check if update is needed on the default branch
	if defaultBranch != "" {
		for _, ref := range refs {
			if ref.Name().String() == defaultBranch {
				if head.Hash() != ref.Hash() {
					DockerLogger.Info().
						Str("repo", extractRepoName(gm.repoURL)).
						Str("branch", defaultBranch).
						Msg("Local and remote HEADs differ, update needed")
					return true, nil
				}
				DockerLogger.Debug().
					Str("repo", extractRepoName(gm.repoURL)).
					Str("branch", defaultBranch).
					Msg("Local repository is up to date")
				break
			}
		}
	} else {
		DockerLogger.Warn().
			Str("repo", extractRepoName(gm.repoURL)).
			Msg("Could not determine default branch, assuming update needed")
		return true, nil
	}

	return false, nil
}

func (gm *GitManager) clone() error {
	repoName := extractRepoName(gm.repoURL)
	DockerLogger.Info().
		Str("repo", repoName).
		Msg("Cloning repository...")
	repo, err := git.PlainClone(gm.localPath, false, &git.CloneOptions{
		URL:      gm.repoURL,
		Progress: newGitProgressWriter(gm.repoURL),
	})
	if err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}
	gm.repo = repo
	DockerLogger.Info().
		Str("repo", repoName).
		Msg("Repository cloned successfully")
	return nil
}

func (gm *GitManager) pull() error {
	var err error
	gm.repo, err = git.PlainOpen(gm.localPath)
	if err != nil {
		return fmt.Errorf("failed to open repository: %w", err)
	}

	worktree, err := gm.repo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to get worktree: %w", err)
	}

	err = worktree.Pull(&git.PullOptions{
		RemoteName: "origin",
		Progress:   newGitProgressWriter(gm.repoURL),
	})
	if err != nil {
		if err == git.NoErrAlreadyUpToDate {
			DockerLogger.Info().
				Str("repo", extractRepoName(gm.repoURL)).
				Msg("Repository is already up-to-date")
			return nil
		}
		return fmt.Errorf("failed to pull repository: %w", err)
	}
	DockerLogger.Info().
		Str("repo", extractRepoName(gm.repoURL)).
		Msg("Repository updated successfully")
	return nil
}

func dirExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}