package discovery

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitInfo reads branch/commit/time from a repo dir. The live git checkout is
// the source of truth (it reflects the code actually on disk); the slot's .env
// (VITE_GIT_*) is only a fallback for when git can't be read, since that file
// records the version baked at the last frontend build and is often stale.
func gitInfo(ctx context.Context, repoDir string) (branch, commit, commitTime, version string) {
	if st, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil && st.IsDir() {
		branch = runGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
		commit = runGit(ctx, repoDir, "rev-parse", "--short", "HEAD")
		commitTime = runGit(ctx, repoDir, "log", "-1", "--format=%ci")
	}
	// Fall back to .env only for fields git didn't give us.
	b, c, v := envGitInfo(repoDir)
	if branch == "" {
		branch = b
	}
	if commit == "" {
		commit = c
	}
	// Version label: build it from the live branch+commit so it always matches
	// what's on disk. Use .env's version string only if git gave us nothing.
	switch {
	case branch != "" && commit != "":
		version = branch + "-" + commit
	case commit != "":
		version = commit
	default:
		version = v
	}
	return
}

func runGit(ctx context.Context, dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// -c safe.directory=* so git reads repos owned by a different user than
	// the container process (host repos are owned by root, mounted read-only).
	full := append([]string{"-c", "safe.directory=*", "-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// envGitInfo reads VITE_GIT_* keys from <repoDir>/.env (secret values ignored).
func envGitInfo(repoDir string) (branch, commit, version string) {
	f, err := os.Open(filepath.Join(repoDir, ".env"))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "VITE_GIT_BRANCH":
			branch = v
		case "VITE_GIT_COMMIT_HASH":
			commit = v
		case "VITE_GIT_FULL_VERSION":
			version = v
		}
	}
	return
}
