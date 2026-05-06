package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Manager handles git worktree operations.
//
// Every git command run by a Manager uses exec.CommandContext with the
// Manager's ctx. NewManager() defaults to context.Background() for
// back-compat; long-lived callers (the daemon) should construct with
// NewManagerWithContext so git calls get canceled on shutdown.
type Manager struct {
	repoPath string
	ctx      context.Context
}

// NewManager creates a new worktree manager with a background context.
// Prefer NewManagerWithContext when a cancellable context is available.
//
//nolint:contextcheck // intentional: compat constructor used when the caller has no ctx.
func NewManager(repoPath string) *Manager {
	return NewManagerWithContext(context.Background(), repoPath)
}

// NewManagerWithContext creates a new worktree manager whose git commands
// are canceled when ctx is canceled. Callers with a cancelable lifetime
// (daemon loops, long-running commands) should use this form. ctx must not
// be nil — NewManager is the right helper when no ctx is available.
func NewManagerWithContext(ctx context.Context, repoPath string) *Manager {
	return &Manager{repoPath: repoPath, ctx: ctx}
}

// runGit runs a git command in the repository directory and returns output.
// If the command fails, the error includes the command output for debugging.
func (m *Manager) runGit(args ...string) ([]byte, error) {
	cmd := exec.CommandContext(m.ctx, "git", args...)
	cmd.Dir = m.repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return output, fmt.Errorf("git %s: %w\nOutput: %s", args[0], err, output)
	}
	return output, nil
}

// resolvePathWithSymlinks resolves a path to its absolute form and evaluates symlinks.
// This is important on macOS where /var is a symlink to /private/var.
// If symlink resolution fails (e.g., path doesn't exist), returns the absolute path.
func resolvePathWithSymlinks(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}

	evalPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		// Path might not exist yet or symlink resolution failed, use absPath
		return absPath, nil //nolint:nilerr // graceful fallback to abs-only path
	}

	return evalPath, nil
}

// Create creates a new git worktree
func (m *Manager) Create(path, branch string) error {
	_, err := m.runGit("worktree", "add", path, branch)
	return err
}

// CreateDetached creates a new worktree in detached HEAD mode at the given commit.
// This avoids branch name conflicts when multiple worktrees need the same base.
func (m *Manager) CreateDetached(path, commit string) error {
	_, err := m.runGit("worktree", "add", "--detach", path, commit)
	return err
}

// CheckoutBranch checks out a branch in the given worktree directory.
// Used to move non-worker worktrees (supervisor, merge-queue, pr-shepherd) from
// detached HEAD to main so that gh CLI commands like `gh pr merge` work correctly.
// If the branch is already checked out in another worktree (e.g., the main clone),
// falls back to creating a local tracking branch (e.g., supervisor-main).
func (m *Manager) CheckoutBranch(worktreePath, branch string) error {
	validBranch := regexp.MustCompile(`^[a-zA-Z0-9_\-\./]+$`)
	if !validBranch.MatchString(branch) {
		return fmt.Errorf("invalid input")
	}
	cmd := exec.CommandContext(m.ctx, "git", "checkout", branch)
	cmd.Dir = worktreePath
	if _, err := cmd.CombinedOutput(); err == nil {
		return nil
	}
	localBranch := filepath.Base(worktreePath) + "-" + branch
	cmd = exec.CommandContext(m.ctx, "git", "checkout", "-b", localBranch, "origin/"+branch)
	cmd.Dir = worktreePath
	_, err := cmd.CombinedOutput()
	return err
}

// CreateNewBranch creates a new worktree with a new branch.
// On failure, attempts to delete any partial branch ref to prevent ghost branches (Bug 4).
func (m *Manager) CreateNewBranch(path, newBranch, startPoint string) error {
	_, err := m.runGit("worktree", "add", "-b", newBranch, path, startPoint)
	if err != nil {
		_ = m.DeleteBranch(newBranch) // Best effort cleanup of partial ref
		return err
	}

	// Copy pytest-testmon database from main repo to new worktree for faster test runs
	// This allows workers to benefit from existing test coverage data without
	// needing to run the full test suite on first execution
	m.copyTestmonDatabase(path)

	return nil
}

// copyTestmonDatabase copies pytest-testmon database files from main repo to worktree
func (m *Manager) copyTestmonDatabase(worktreePath string) {
	// List of testmon database files to copy (SQLite database + WAL files)
	testmonFiles := []string{
		".testmondata",
		".testmondata-shm",
		".testmondata-wal",
	}

	for _, filename := range testmonFiles {
		srcPath := filepath.Join(m.repoPath, filename)
		dstPath := filepath.Join(worktreePath, filename)

		// Check if source file exists
		if _, err := os.Stat(srcPath); err != nil {
			continue // File doesn't exist, skip
		}

		// Read source file
		data, err := os.ReadFile(srcPath)
		if err != nil {
			continue // Can't read, skip silently
		}

		// Write to destination
		_ = os.WriteFile(dstPath, data, 0644)
		// Ignore errors - testmon copy is best-effort, not critical
	}
}

// Remove removes a git worktree
func (m *Manager) Remove(path string, force bool) error {
	args := []string{"worktree", "remove", path}
	if force {
		args = append(args, "--force")
	}
	_, err := m.runGit(args...)
	return err
}

// List returns a list of all worktrees
func (m *Manager) List() ([]WorktreeInfo, error) {
	cmd := exec.CommandContext(m.ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = m.repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	return parseWorktreeList(string(output)), nil
}

// Exists checks if a worktree exists at the given path
func (m *Manager) Exists(path string) (bool, error) {
	worktrees, err := m.List()
	if err != nil {
		return false, err
	}

	evalPath, err := resolvePathWithSymlinks(path)
	if err != nil {
		return false, err
	}

	for _, wt := range worktrees {
		wtEval, err := resolvePathWithSymlinks(wt.Path)
		if err != nil {
			continue
		}
		if wtEval == evalPath {
			return true, nil
		}
	}

	return false, nil
}

// Prune removes worktree information for missing paths
func (m *Manager) Prune() error {
	_, err := m.runGit("worktree", "prune")
	return err
}

// HasUncommittedChanges checks if a worktree has uncommitted changes,
// excluding OAT's own runtime directory (.oat/) which is always safe to ignore.
func HasUncommittedChanges(ctx context.Context, path string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = path
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to check git status: %w", err)
	}

	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line == "" {
			continue
		}
		if len(line) < 4 {
			continue
		}
		filePath := line[3:]
		if idx := strings.Index(filePath, " -> "); idx >= 0 {
			filePath = filePath[idx+4:]
		}
		filePath = strings.Trim(filePath, "\"")
		if !strings.HasPrefix(filePath, ".oat/") {
			return true, nil
		}
	}
	return false, nil
}

// HasUnpushedCommits checks if a worktree has unpushed commits
func HasUnpushedCommits(ctx context.Context, path string) (bool, error) {
	// First verify this is a valid git repository
	verifyCmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	verifyCmd.Dir = path
	if err := verifyCmd.Run(); err != nil {
		return false, fmt.Errorf("not a git repository: %w", err)
	}

	// Check if there's a tracking branch
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	cmd.Dir = path
	if err := cmd.Run(); err != nil {
		// No tracking branch, so no unpushed commits
		// This is a valid state (branch has no upstream configured)
		return false, nil //nolint:nilerr // no tracking branch -> no unpushed commits
	}

	// Check for commits ahead of upstream
	cmd = exec.CommandContext(ctx, "git", "rev-list", "--count", "@{u}..")
	cmd.Dir = path
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to check unpushed commits: %w", err)
	}

	count := strings.TrimSpace(string(output))
	return count != "0", nil
}

// GetCurrentBranch returns the current branch name for a worktree
func GetCurrentBranch(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = path
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// WorktreeInfo contains information about a worktree
type WorktreeInfo struct {
	Path   string
	Commit string
	Branch string
}

// parseWorktreeList parses the output of `git worktree list --porcelain`
func parseWorktreeList(output string) []WorktreeInfo {
	var worktrees []WorktreeInfo
	var current WorktreeInfo

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if current.Path != "" {
				worktrees = append(worktrees, current)
				current = WorktreeInfo{}
			}
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}

		switch parts[0] {
		case "worktree":
			current.Path = parts[1]
		case "HEAD":
			current.Commit = parts[1]
		case "branch":
			current.Branch = strings.TrimPrefix(parts[1], "refs/heads/")
		}
	}

	// Add last worktree if exists
	if current.Path != "" {
		worktrees = append(worktrees, current)
	}

	return worktrees
}

// BranchExists checks if a branch exists in the repository
func (m *Manager) BranchExists(branchName string) (bool, error) {
	validBranch := regexp.MustCompile(`^[a-zA-Z0-9_\-\./]+$`)
	if !validBranch.MatchString(branchName) {
		return false, fmt.Errorf("invalid input")
	}
	cmd := exec.CommandContext(m.ctx, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branchName)
	cmd.Dir = m.repoPath
	err := cmd.Run()
	if err != nil {
		// Exit code 1 means branch doesn't exist
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, fmt.Errorf("failed to check branch existence: %w", err)
	}
	return true, nil
}

// RenameBranch renames a branch from oldName to newName
func (m *Manager) RenameBranch(oldName, newName string) error {
	_, err := m.runGit("branch", "-m", oldName, newName)
	return err
}

// DeleteBranch force deletes a branch (git branch -D)
func (m *Manager) DeleteBranch(branchName string) error {
	_, err := m.runGit("branch", "-D", branchName)
	return err
}

// ListBranchesWithPrefix lists all branches that start with the given prefix
func (m *Manager) ListBranchesWithPrefix(prefix string) ([]string, error) {
	validPrefix := regexp.MustCompile(`^[a-zA-Z0-9_\-\./]*$`)
	if !validPrefix.MatchString(prefix) {
		return nil, fmt.Errorf("invalid input")
	}
	cmd := exec.CommandContext(m.ctx, "git", "for-each-ref", "--format=%(refname:short)", "refs/heads/"+prefix)
	cmd.Dir = m.repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list branches: %w", err)
	}

	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if line != "" {
			branches = append(branches, line)
		}
	}
	return branches, nil
}

// FindOrphanedBranches finds branches with the given prefix that don't have corresponding worktrees
func (m *Manager) FindOrphanedBranches(prefix string) ([]string, error) {
	// Get all branches with the prefix
	branches, err := m.ListBranchesWithPrefix(prefix)
	if err != nil {
		return nil, err
	}

	// Get all worktrees
	worktrees, err := m.List()
	if err != nil {
		return nil, err
	}

	// Build a set of branches that have worktrees
	activeBranches := make(map[string]bool)
	for _, wt := range worktrees {
		if wt.Branch != "" {
			activeBranches[wt.Branch] = true
		}
	}

	// Find orphaned branches
	var orphaned []string
	for _, branch := range branches {
		if !activeBranches[branch] {
			orphaned = append(orphaned, branch)
		}
	}

	return orphaned, nil
}

// CanCreateBranchWithPrefix checks if a branch can be created with a given prefix.
// Returns false if there's a conflicting branch (e.g., "workspace" exists and
// we're trying to create "workspace/foo").
func (m *Manager) CanCreateBranchWithPrefix(prefix string) (bool, string, error) {
	// Check if a branch with the exact prefix name exists (e.g., "workspace")
	// This would prevent creating "workspace/foo" due to git ref limitations
	exists, err := m.BranchExists(prefix)
	if err != nil {
		return false, "", err
	}
	if exists {
		return false, prefix, nil
	}
	return true, "", nil
}

// MigrateLegacyWorkspaceBranch checks for a legacy "workspace" branch and renames it
// to "workspace/default" to allow the new workspace/<name> naming convention.
// Returns:
//   - migrated: true if migration was performed
//   - error: any error that occurred
func (m *Manager) MigrateLegacyWorkspaceBranch() (bool, error) {
	// Check if legacy "workspace" branch exists
	legacyExists, err := m.BranchExists("workspace")
	if err != nil {
		return false, fmt.Errorf("failed to check for legacy workspace branch: %w", err)
	}

	if !legacyExists {
		// No legacy branch, nothing to migrate
		return false, nil
	}

	// Check if the new naming convention is already in use (workspace/default exists)
	newExists, err := m.BranchExists("workspace/default")
	if err != nil {
		return false, fmt.Errorf("failed to check for workspace/default branch: %w", err)
	}

	if newExists {
		// Both exist - this is a conflict state that shouldn't happen in normal usage
		return false, fmt.Errorf("both 'workspace' and 'workspace/default' branches exist; manual resolution required")
	}

	// Rename workspace -> workspace/default
	if err := m.RenameBranch("workspace", "workspace/default"); err != nil {
		return false, fmt.Errorf("failed to migrate workspace branch: %w", err)
	}

	return true, nil
}

// CheckWorkspaceBranchConflict checks if there's a potential conflict between
// legacy "workspace" branch and the new workspace/<name> naming convention.
// Returns:
//   - hasConflict: true if there's a blocking "workspace" branch
//   - suggestion: a suggested fix for the user
func (m *Manager) CheckWorkspaceBranchConflict() (bool, string, error) {
	exists, err := m.BranchExists("workspace")
	if err != nil {
		return false, "", err
	}

	if exists {
		suggestion := `A legacy 'workspace' branch exists which conflicts with the new workspace/<name> naming convention.

To fix this, you can either:
1. Let oat migrate the branch automatically by running:
   cd ` + m.repoPath + ` && git branch -m workspace workspace/default

2. Or manually rename/delete the legacy branch:
   cd ` + m.repoPath + ` && git branch -m workspace <new-name>
   cd ` + m.repoPath + ` && git branch -d workspace`
		return true, suggestion, nil
	}

	return false, "", nil
}

// GetUpstreamRemote returns the name of the upstream remote, typically "upstream" or "origin"
// It prefers "upstream" if it exists, otherwise falls back to "origin"
func (m *Manager) GetUpstreamRemote() (string, error) {
	// Check if "upstream" remote exists
	cmd := exec.CommandContext(m.ctx, "git", "remote", "get-url", "upstream")
	cmd.Dir = m.repoPath
	if err := cmd.Run(); err == nil {
		return "upstream", nil
	}

	// Fall back to "origin"
	cmd = exec.CommandContext(m.ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = m.repoPath
	if err := cmd.Run(); err == nil {
		return "origin", nil
	}

	return "", fmt.Errorf("no upstream or origin remote found")
}

// GetDefaultBranch returns the default branch name for a remote (e.g., "main" or "master")
func (m *Manager) GetDefaultBranch(remote string) (string, error) {
	validRemote := regexp.MustCompile(`^[a-zA-Z0-9_\-\.]+$`)
	if !validRemote.MatchString(remote) {
		return "", fmt.Errorf("invalid input")
	}
	// Try to get the default branch from the remote's HEAD
	cmd := exec.CommandContext(m.ctx, "git", "symbolic-ref", fmt.Sprintf("refs/remotes/%s/HEAD", remote))
	cmd.Dir = m.repoPath
	output, err := cmd.Output()
	if err == nil {
		// Output is like "refs/remotes/origin/main" - extract the branch name
		refPath := strings.TrimSpace(string(output))
		parts := strings.Split(refPath, "/")
		if len(parts) > 0 {
			return parts[len(parts)-1], nil
		}
	}

	// Fallback: check for common branch names
	for _, branch := range []string{"main", "master"} {
		cmd := exec.CommandContext(m.ctx, "git", "rev-parse", "--verify", fmt.Sprintf("refs/remotes/%s/%s", remote, branch))
		cmd.Dir = m.repoPath
		if err := cmd.Run(); err == nil {
			return branch, nil
		}
	}

	return "", fmt.Errorf("could not determine default branch for remote %s", remote)
}

// FetchRemote fetches updates from a remote
func (m *Manager) FetchRemote(remote string) error {
	_, err := m.runGit("fetch", remote)
	return err
}

// FindMergedUpstreamBranches finds local branches that have been merged into the upstream default branch.
// It fetches from the upstream remote first to ensure we have the latest state.
// The branchPrefix filters which branches to check (e.g., "oat/" or "work/").
// Returns a list of branch names that can be safely deleted.
func (m *Manager) FindMergedUpstreamBranches(branchPrefix string) ([]string, error) {
	// Get the upstream remote name
	remote, err := m.GetUpstreamRemote()
	if err != nil {
		return nil, fmt.Errorf("failed to get upstream remote: %w", err)
	}

	// Fetch from upstream to get the latest state
	if err := m.FetchRemote(remote); err != nil {
		return nil, fmt.Errorf("failed to fetch from upstream: %w", err)
	}

	// Get the default branch name
	defaultBranch, err := m.GetDefaultBranch(remote)
	if err != nil {
		return nil, fmt.Errorf("failed to get default branch: %w", err)
	}

	// Get branches merged into upstream's default branch
	upstreamRef := fmt.Sprintf("%s/%s", remote, defaultBranch)
	cmd := exec.CommandContext(m.ctx, "git", "branch", "--merged", upstreamRef, "--format=%(refname:short)")
	cmd.Dir = m.repoPath
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list merged branches: %w", err)
	}

	// Filter branches by prefix
	var mergedBranches []string
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		branch := strings.TrimSpace(line)
		if branch == "" {
			continue
		}
		// Skip the default branches themselves
		if branch == "main" || branch == "master" {
			continue
		}
		// Only include branches matching the prefix
		if branchPrefix != "" && !strings.HasPrefix(branch, branchPrefix) {
			continue
		}
		mergedBranches = append(mergedBranches, branch)
	}

	return mergedBranches, nil
}

// DeleteRemoteBranch deletes a branch from a remote
func (m *Manager) DeleteRemoteBranch(remote, branchName string) error {
	_, err := m.runGit("push", remote, "--delete", branchName)
	return err
}

// CleanupMergedBranches finds and deletes local branches that have been merged upstream.
// If deleteRemote is true, it also deletes the corresponding remote branches from origin.
// Returns the list of deleted branch names.
func (m *Manager) CleanupMergedBranches(branchPrefix string, deleteRemote bool) ([]string, error) {
	// Find merged branches
	mergedBranches, err := m.FindMergedUpstreamBranches(branchPrefix)
	if err != nil {
		return nil, err
	}

	if len(mergedBranches) == 0 {
		return nil, nil
	}

	// Get worktrees to avoid deleting branches that are still checked out
	worktrees, err := m.List()
	if err != nil {
		return nil, fmt.Errorf("failed to list worktrees: %w", err)
	}

	activeBranches := make(map[string]bool)
	for _, wt := range worktrees {
		if wt.Branch != "" {
			activeBranches[wt.Branch] = true
		}
	}

	var deleted []string
	for _, branch := range mergedBranches {
		// Skip branches that are currently checked out in worktrees
		if activeBranches[branch] {
			continue
		}

		// Delete local branch
		if err := m.DeleteBranch(branch); err != nil {
			// Log but continue with other branches
			continue
		}
		deleted = append(deleted, branch)

		// Delete remote branch if requested
		if deleteRemote {
			// Try to delete from origin (the fork)
			_ = m.DeleteRemoteBranch("origin", branch)
		}
	}

	return deleted, nil
}

// CleanupOrphanedResult contains the result of a cleanup operation
type CleanupOrphanedResult struct {
	Removed []string          // Successfully removed directories
	Errors  map[string]string // Directories that failed to remove with error messages
}

// CleanupOrphaned removes worktree directories that exist on disk but not in git.
// Returns a result containing both successfully removed paths and any errors encountered.
func CleanupOrphaned(wtRootDir string, manager *Manager) ([]string, error) {
	result, err := CleanupOrphanedWithDetails(wtRootDir, manager, nil)
	if err != nil {
		return nil, err
	}
	return result.Removed, nil
}

// CleanupOrphanedWithDetails removes worktree directories that exist on disk but not in git.
// Unlike CleanupOrphaned, this returns detailed results including any removal errors.
// protectedPaths (optional) lists directories that must never be deleted even if they
// appear orphaned -- used by the daemon to shield active agent worktrees from removal
// when concurrent git operations cause git worktree list to return incomplete results.
func CleanupOrphanedWithDetails(wtRootDir string, manager *Manager, protectedPaths map[string]bool) (*CleanupOrphanedResult, error) {
	result := &CleanupOrphanedResult{
		Errors: make(map[string]string),
	}

	// Get all worktrees from git
	gitWorktrees, err := manager.List()
	if err != nil {
		return nil, err
	}

	gitPaths := make(map[string]bool)
	for _, wt := range gitWorktrees {
		evalPath, err := resolvePathWithSymlinks(wt.Path)
		if err != nil {
			continue
		}
		gitPaths[evalPath] = true
	}

	// Find directories in wtRootDir that aren't in git worktrees
	entries, err := os.ReadDir(wtRootDir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		path := filepath.Join(wtRootDir, entry.Name())
		evalPath, err := resolvePathWithSymlinks(path)
		if err != nil {
			continue
		}

		if !gitPaths[evalPath] {
			if protectedPaths != nil && (protectedPaths[path] || protectedPaths[evalPath]) {
				result.Errors[path] = "skipped: directory belongs to active agent (git tracking out of sync)"
				continue
			}

			// Safety checks before removing: don't destroy work that hasn't
			// been committed or pushed. These catch cases where git tracking
			// gets corrupted by concurrent agent operations but the worktree
			// still contains valuable in-progress work.
			if hasChanges, err := HasUncommittedChanges(manager.ctx, path); err == nil && hasChanges {
				result.Errors[path] = "skipped: has uncommitted changes"
				continue
			}
			if hasUnpushed, err := HasUnpushedCommits(manager.ctx, path); err == nil && hasUnpushed {
				result.Errors[path] = "skipped: has unpushed commits"
				continue
			}

			if err := os.RemoveAll(path); err != nil {
				result.Errors[path] = err.Error()
			} else {
				result.Removed = append(result.Removed, path)
			}
		}
	}

	return result, nil
}

// WorktreeState represents the current state of a worktree
type WorktreeState struct {
	Path           string
	Branch         string
	IsDetachedHEAD bool
	IsMidRebase    bool
	IsMidMerge     bool
	HasUncommitted bool
	CommitsBehind  int  // Number of commits behind remote main
	CommitsAhead   int  // Number of commits ahead of remote main
	CanRefresh     bool // True if worktree is in a state that can be safely refreshed
	RefreshReason  string
}

// GetWorktreeState checks the current state of a worktree and whether it can be safely refreshed
func GetWorktreeState(ctx context.Context, worktreePath string, remote string, mainBranch string) (WorktreeState, error) {
	validRemote := regexp.MustCompile(`^[a-zA-Z0-9_\-\.]+$`)
	validBranch := regexp.MustCompile(`^[a-zA-Z0-9_\-\./]+$`)
	if !validRemote.MatchString(remote) {
		return WorktreeState{}, fmt.Errorf("invalid input")
	}
	if !validBranch.MatchString(mainBranch) {
		return WorktreeState{}, fmt.Errorf("invalid input")
	}
	state := WorktreeState{
		Path:       worktreePath,
		CanRefresh: true,
	}

	// Get current branch (or detect detached HEAD)
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		// Check if it's detached HEAD (different error than not being a git repo)
		cmd2 := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD")
		cmd2.Dir = worktreePath
		if err2 := cmd2.Run(); err2 != nil {
			return state, fmt.Errorf("not a git repository or invalid state: %w", err)
		}
		state.IsDetachedHEAD = true
		state.CanRefresh = false
		state.RefreshReason = "detached HEAD"
	} else {
		state.Branch = strings.TrimSpace(string(output))
	}

	// Check for mid-rebase state
	gitDir := filepath.Join(worktreePath, ".git")
	// For worktrees, .git is a file pointing to the real git dir
	if content, err := os.ReadFile(gitDir); err == nil && strings.HasPrefix(string(content), "gitdir:") {
		gitDir = strings.TrimSpace(strings.TrimPrefix(string(content), "gitdir:"))
	}
	rebaseDir := filepath.Join(gitDir, "rebase-merge")
	rebaseApplyDir := filepath.Join(gitDir, "rebase-apply")
	if _, err := os.Stat(rebaseDir); err == nil {
		state.IsMidRebase = true
		state.CanRefresh = false
		state.RefreshReason = "mid-rebase"
	} else if _, err := os.Stat(rebaseApplyDir); err == nil {
		state.IsMidRebase = true
		state.CanRefresh = false
		state.RefreshReason = "mid-rebase"
	}

	// Check for mid-merge state
	mergeHead := filepath.Join(gitDir, "MERGE_HEAD")
	if _, err := os.Stat(mergeHead); err == nil {
		state.IsMidMerge = true
		state.CanRefresh = false
		state.RefreshReason = "mid-merge"
	}

	// Check for uncommitted changes
	hasChanges, err := HasUncommittedChanges(ctx, worktreePath)
	if err == nil {
		state.HasUncommitted = hasChanges
	}

	// Skip commit count checks if we can't refresh anyway
	if !state.CanRefresh {
		return state, nil
	}

	// Check if on main branch (shouldn't refresh main)
	if state.Branch == mainBranch || state.Branch == "main" || state.Branch == "master" {
		state.CanRefresh = false
		state.RefreshReason = "on main branch"
		return state, nil
	}

	// Check commits behind/ahead of remote main
	cmd = exec.CommandContext(ctx, "git", "rev-list", "--left-right", "--count", fmt.Sprintf("%s/%s...HEAD", remote, mainBranch))
	cmd.Dir = worktreePath
	output, err = cmd.Output()
	if err != nil {
		// If we can't check, assume we can't safely auto-refresh
		state.CanRefresh = false
		state.RefreshReason = fmt.Sprintf("could not check remote status: %v", err)
		return state, nil
	}

	// Parse output like "3\t5" (behind\tahead)
	parts := strings.Fields(strings.TrimSpace(string(output)))
	if len(parts) == 2 {
		_, _ = fmt.Sscanf(parts[0], "%d", &state.CommitsBehind) // parse error leaves zero
		_, _ = fmt.Sscanf(parts[1], "%d", &state.CommitsAhead)
	}

	// If not behind, no need to refresh
	if state.CommitsBehind == 0 {
		state.CanRefresh = false
		state.RefreshReason = "already up to date"
	}

	return state, nil
}

// IsBehindMain checks if a worktree is behind the remote main branch
func IsBehindMain(ctx context.Context, worktreePath string, remote string, mainBranch string) (bool, int, error) {
	state, err := GetWorktreeState(ctx, worktreePath, remote, mainBranch)
	if err != nil {
		return false, 0, err
	}
	return state.CommitsBehind > 0, state.CommitsBehind, nil
}

// RefreshResult contains the result of a worktree refresh operation
type RefreshResult struct {
	WorktreePath   string
	Branch         string
	CommitsRebased int
	WasStashed     bool
	StashRestored  bool
	HasConflicts   bool
	ConflictFiles  []string
	Error          error
	Skipped        bool
	SkipReason     string
}

// RefreshWorktree syncs a worktree with the latest changes from the main branch.
// It fetches from the remote, stashes any uncommitted changes, rebases onto main,
// and restores the stash. Returns detailed results about what happened.
func RefreshWorktree(ctx context.Context, worktreePath string, remote string, mainBranch string) RefreshResult {
	validRemote := regexp.MustCompile(`^[a-zA-Z0-9_\-\.]+$`)
	validBranch := regexp.MustCompile(`^[a-zA-Z0-9_\-\./]+$`)
	if !validRemote.MatchString(remote) {
		return RefreshResult{
			WorktreePath: worktreePath,
			Error:        fmt.Errorf("invalid input"),
		}
	}
	if !validBranch.MatchString(mainBranch) {
		return RefreshResult{
			WorktreePath: worktreePath,
			Error:        fmt.Errorf("invalid input"),
		}
	}
	result := RefreshResult{
		WorktreePath: worktreePath,
	}

	// Check for detached HEAD, mid-rebase, or mid-merge states
	// These must be resolved before we can safely refresh
	gitDir := filepath.Join(worktreePath, ".git")
	// For worktrees, .git is a file pointing to the real git dir
	if content, err := os.ReadFile(gitDir); err == nil && strings.HasPrefix(string(content), "gitdir:") {
		gitDir = strings.TrimSpace(strings.TrimPrefix(string(content), "gitdir:"))
	}

	// Check for mid-rebase state
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-merge")); err == nil {
		result.Skipped = true
		result.SkipReason = "mid-rebase (run 'git rebase --continue' or 'git rebase --abort')"
		return result
	}
	if _, err := os.Stat(filepath.Join(gitDir, "rebase-apply")); err == nil {
		result.Skipped = true
		result.SkipReason = "mid-rebase (run 'git rebase --continue' or 'git rebase --abort')"
		return result
	}

	// Check for mid-merge state
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		result.Skipped = true
		result.SkipReason = "mid-merge (run 'git merge --continue' or 'git merge --abort')"
		return result
	}

	// Get current branch (also detects detached HEAD)
	cmd := exec.CommandContext(ctx, "git", "symbolic-ref", "--short", "HEAD")
	cmd.Dir = worktreePath
	output, err := cmd.Output()
	if err != nil {
		// Check if it's detached HEAD vs not a git repo
		cmd2 := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD")
		cmd2.Dir = worktreePath
		if cmd2.Run() == nil {
			result.Skipped = true
			result.SkipReason = "detached HEAD (checkout a branch first)"
			return result
		}
		result.Error = fmt.Errorf("failed to get current branch: %w", err)
		return result
	}
	branch := strings.TrimSpace(string(output))
	result.Branch = branch

	// Don't refresh if on main branch directly
	if branch == mainBranch || branch == "main" || branch == "master" {
		result.Skipped = true
		result.SkipReason = "on main branch"
		return result
	}

	// Fetch latest from remote
	cmd = exec.CommandContext(ctx, "git", "fetch", remote, mainBranch)
	cmd.Dir = worktreePath
	if output, err := cmd.CombinedOutput(); err != nil {
		result.Error = fmt.Errorf("failed to fetch from %s: %w\nOutput: %s", remote, err, output)
		return result
	}

	// Check for uncommitted changes
	hasChanges, err := HasUncommittedChanges(ctx, worktreePath)
	if err != nil {
		result.Error = fmt.Errorf("failed to check for uncommitted changes: %w", err)
		return result
	}

	// Stash if there are uncommitted changes (including untracked files)
	stashName := ""
	if hasChanges {
		stashName = fmt.Sprintf("refresh-stash-%d", os.Getpid())
		cmd = exec.CommandContext(ctx, "git", "stash", "push", "--include-untracked", "-m", stashName)
		cmd.Dir = worktreePath
		if output, err := cmd.CombinedOutput(); err != nil {
			result.Error = fmt.Errorf("failed to stash changes: %w\nOutput: %s", err, output)
			return result
		}
		result.WasStashed = true
	}

	// Get current commit count before rebase
	cmd = exec.CommandContext(ctx, "git", "rev-list", "--count", fmt.Sprintf("%s/%s..HEAD", remote, mainBranch))
	cmd.Dir = worktreePath
	countOutput, _ := cmd.Output()
	commitsBefore := strings.TrimSpace(string(countOutput))

	// Rebase onto main
	cmd = exec.CommandContext(ctx, "git", "rebase", fmt.Sprintf("%s/%s", remote, mainBranch))
	cmd.Dir = worktreePath
	rebaseOutput, rebaseErr := cmd.CombinedOutput()

	if rebaseErr != nil {
		// Check if there are conflicts
		cmd = exec.CommandContext(ctx, "git", "diff", "--name-only", "--diff-filter=U")
		cmd.Dir = worktreePath
		conflictOutput, _ := cmd.Output()
		conflictFiles := strings.Split(strings.TrimSpace(string(conflictOutput)), "\n")
		if len(conflictFiles) > 0 && conflictFiles[0] != "" {
			result.HasConflicts = true
			result.ConflictFiles = conflictFiles
			// Abort the rebase to leave the worktree in a clean state; best-effort cleanup.
			abortCmd := exec.CommandContext(ctx, "git", "rebase", "--abort")
			abortCmd.Dir = worktreePath
			_ = abortCmd.Run()
		}
		result.Error = fmt.Errorf("rebase failed: %w\nOutput: %s", rebaseErr, rebaseOutput)

		// Restore stash if we stashed
		if result.WasStashed {
			popCmd := exec.CommandContext(ctx, "git", "stash", "pop")
			popCmd.Dir = worktreePath
			if popCmd.Run() == nil {
				result.StashRestored = true
			}
		}
		return result
	}

	// Calculate commits rebased (commits that were ahead of main)
	// This is an approximation based on the output
	if commitsBefore != "" && commitsBefore != "0" {
		_, _ = fmt.Sscanf(commitsBefore, "%d", &result.CommitsRebased) // parse error leaves zero
	}

	// Restore stash if we stashed
	if result.WasStashed {
		cmd = exec.CommandContext(ctx, "git", "stash", "pop")
		cmd.Dir = worktreePath
		if err := cmd.Run(); err != nil {
			// Stash pop might fail if there are conflicts
			result.Error = fmt.Errorf("stash pop failed (manual resolution may be needed): %w", err)
		} else {
			result.StashRestored = true
		}
	}

	return result
}

// RefreshWorktreeWithDefaults refreshes a worktree using the repository's default remote and branch
func (m *Manager) RefreshWorktreeWithDefaults(worktreePath string) RefreshResult {
	// Get the upstream remote
	remote, err := m.GetUpstreamRemote()
	if err != nil {
		return RefreshResult{
			WorktreePath: worktreePath,
			Error:        fmt.Errorf("failed to get remote: %w", err),
		}
	}

	// Get the default branch
	mainBranch, err := m.GetDefaultBranch(remote)
	if err != nil {
		return RefreshResult{
			WorktreePath: worktreePath,
			Error:        fmt.Errorf("failed to get default branch: %w", err),
		}
	}

	return RefreshWorktree(m.ctx, worktreePath, remote, mainBranch)
}
