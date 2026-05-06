//go:generate go run ../../cmd/generate-docs

package config

import (
	"os"
	"path/filepath"
)

// Paths holds all the directory and file paths used by oat
type Paths struct {
	Root             string // $HOME/.oat/
	BinDir           string // bin/ (neutral dir for PATH, symlinks to real binaries)
	DaemonPID        string // daemon.pid
	DaemonSock       string // daemon.sock
	DaemonLog        string // daemon.log
	StateFile        string // state.json
	ReposDir         string // repos/
	WorktreesDir     string // wts/
	MessagesDir      string // messages/
	OutputDir        string // output/
	ArchiveDir       string // archive/ (for paused work)
	ModelProfilesDir string // model-profiles/ (onboarded model capability profiles)
}

// DefaultPaths returns the default paths for oat
func DefaultPaths() (*Paths, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	root := filepath.Join(home, ".oat")

	return &Paths{
		Root:             root,
		BinDir:           filepath.Join(root, "bin"),
		DaemonPID:        filepath.Join(root, "daemon.pid"),
		DaemonSock:       filepath.Join(root, "daemon.sock"),
		DaemonLog:        filepath.Join(root, "daemon.log"),
		StateFile:        filepath.Join(root, "state.json"),
		ReposDir:         filepath.Join(root, "repos"),
		WorktreesDir:     filepath.Join(root, "wts"),
		MessagesDir:      filepath.Join(root, "messages"),
		OutputDir:        filepath.Join(root, "output"),
		ArchiveDir:       filepath.Join(root, "archive"),
		ModelProfilesDir: filepath.Join(root, "model-profiles"),
	}, nil
}

// EnsureDirectories creates all necessary directories if they don't exist
func (p *Paths) EnsureDirectories() error {
	dirs := []string{
		p.Root,
		p.BinDir,
		p.ReposDir,
		p.WorktreesDir,
		p.MessagesDir,
		p.OutputDir,
		p.ArchiveDir,
		p.ModelProfilesDir,
	}

	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	return nil
}

// RepoDir returns the path for a specific repository
func (p *Paths) RepoDir(repoName string) string {
	return filepath.Join(p.ReposDir, repoName)
}

// RepoAgentsDir returns the path for a repository's agent definitions
// These are the per-repo agent templates that define configurable agents
func (p *Paths) RepoAgentsDir(repoName string) string {
	return filepath.Join(p.ReposDir, repoName, "agents")
}

// WorktreeDir returns the path for a repository's worktrees
func (p *Paths) WorktreeDir(repoName string) string {
	return filepath.Join(p.WorktreesDir, repoName)
}

// AgentWorktree returns the path for a specific agent's worktree
func (p *Paths) AgentWorktree(repoName, agentName string) string {
	return filepath.Join(p.WorktreeDir(repoName), agentName)
}

// MessagesDir returns the path for a repository's messages
func (p *Paths) RepoMessagesDir(repoName string) string {
	return filepath.Join(p.MessagesDir, repoName)
}

// AgentMessagesDir returns the path for a specific agent's messages
func (p *Paths) AgentMessagesDir(repoName, agentName string) string {
	return filepath.Join(p.RepoMessagesDir(repoName), agentName)
}

// RepoOutputDir returns the path for a repository's output logs
func (p *Paths) RepoOutputDir(repoName string) string {
	return filepath.Join(p.OutputDir, repoName)
}

// WorkersOutputDir returns the path for worker agent output logs
func (p *Paths) WorkersOutputDir(repoName string) string {
	return filepath.Join(p.RepoOutputDir(repoName), "workers")
}

// AgentLogFile returns the path to an agent's log file
func (p *Paths) AgentLogFile(repoName, agentName string, isWorker bool) string {
	if isWorker {
		return filepath.Join(p.WorkersOutputDir(repoName), agentName+".log")
	}
	return filepath.Join(p.RepoOutputDir(repoName), agentName+".log")
}

// NewTestPaths creates a Paths instance for testing with all paths under tmpDir.
// This eliminates duplicate test setup code and ensures consistent path configuration.
func NewTestPaths(tmpDir string) *Paths {
	return &Paths{
		Root:             tmpDir,
		BinDir:           filepath.Join(tmpDir, "bin"),
		DaemonPID:        filepath.Join(tmpDir, "daemon.pid"),
		DaemonSock:       filepath.Join(tmpDir, "daemon.sock"),
		DaemonLog:        filepath.Join(tmpDir, "daemon.log"),
		StateFile:        filepath.Join(tmpDir, "state.json"),
		ReposDir:         filepath.Join(tmpDir, "repos"),
		WorktreesDir:     filepath.Join(tmpDir, "wts"),
		MessagesDir:      filepath.Join(tmpDir, "messages"),
		OutputDir:        filepath.Join(tmpDir, "output"),
		ArchiveDir:       filepath.Join(tmpDir, "archive"),
		ModelProfilesDir: filepath.Join(tmpDir, "model-profiles"),
	}
}

// RepoArchiveDir returns the path for a repository's archived work
func (p *Paths) RepoArchiveDir(repoName string) string {
	return filepath.Join(p.ArchiveDir, repoName)
}

// ScratchpadDir returns the path for a repository's shared scratchpad.
// Workers can read/write here to share discovered facts across agents.
func (p *Paths) ScratchpadDir(repoName string) string {
	return filepath.Join(p.RepoDir(repoName), "scratchpad")
}

// EnsureBinSymlinks creates symlinks in ~/.oat/bin/ pointing to the given
// binary paths. Existing symlinks are updated if their target has changed.
// This keeps PATH free of source-tree directories that could confuse agents.
func (p *Paths) EnsureBinSymlinks(binaries map[string]string) error {
	if err := os.MkdirAll(p.BinDir, 0755); err != nil {
		return err
	}
	for name, target := range binaries {
		link := filepath.Join(p.BinDir, name)
		existing, err := os.Readlink(link)
		if err == nil && existing == target {
			continue
		}
		os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			return err
		}
	}
	return nil
}
