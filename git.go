
package dockerutil

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/rs/zerolog/log"
)

// GitManager handles Git repository operations
type GitManager struct {
	repoURL     string
	localPath   string
	repo        *git.Repository
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
		log.Info().Msg("Local repository does not exist. Cloning...")
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

	for _, ref := range refs {
		if ref.Name().String() == "refs/heads/master" {
			if head.Hash() != ref.Hash() {
				log.Info().Msg("Local and remote HEADs differ, update needed")
				return true, nil
			}
			break
		}
	}

	return false, nil
}

func (gm *GitManager) clone() error {
	log.Info().Msg("Cloning repository...")
	repo, err := git.PlainClone(gm.localPath, false, &git.CloneOptions{
		URL:      gm.repoURL,
		Progress: os.Stdout,
	})
	if err != nil {
		return fmt.Errorf("failed to clone repository: %w", err)
	}
	gm.repo = repo
	log.Info().Msg("Repository cloned successfully")
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
		Progress:   os.Stdout,
	})
	if err != nil {
		if err == git.NoErrAlreadyUpToDate {
			log.Info().Msg("Repository is already up-to-date")
			return nil
		}
		return fmt.Errorf("failed to pull repository: %w", err)
	}
	log.Info().Msg("Repository updated successfully")
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