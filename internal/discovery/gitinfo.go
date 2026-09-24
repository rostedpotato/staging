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

// gitInfo reads branch/commit/time from a repo dir. Falls back to the slot's
// .env (VITE_GIT_*) if git is unavailable or the dir is not a checkout.
func gitInfo(ctx context.Context, repoDir string) (branch, commit, commitTime, version string) {
	if st, err := os.Stat(filepath.Join(repoDir, ".git")); err == nil && st.IsDir() {
		branch = runGit(ctx, repoDir, "rev-parse", "--abbrev-ref", "HEAD")
		commit = runGit(ctx, repoDir, "rev-parse", "--short", "HEAD")
		commitTime = runGit(ctx, repoDir, "log", "-1", "--format=%ci")
	}
	// .env can carry an explicit version and also serves as a fallback.
	if b, c, v := envGitInfo(repoDir); v != "" || (branch == "" && b != "") {
		if branch == "" {
			branch = b
		}
		if commit == "" {
			commit = c
		}
		version = v
	}
	return
}

func runGit(ctx context.Context, dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
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
