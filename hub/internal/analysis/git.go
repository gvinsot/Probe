package analysis

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gvinsot/Probe/hub/internal/store"
)

// gitRunner drives git in a disposable directory with a minimal environment.
type gitRunner struct {
	dir string
	env []string
}

// gitEnv builds the environment of every git invocation.
//
// The credential is injected through GIT_CONFIG_* variables scoped to the
// remote URL: it never appears on the command line, where it would be visible
// in the process table, and it is never sent to another host.
func gitEnv(home, cloneURL, authHeader string) []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_ASKPASS=/bin/true",
		"LC_ALL=C",
	}
	if authHeader == "" || cloneURL == "" {
		return env
	}
	return append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http."+cloneURL+".extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: "+authHeader,
	)
}

// run executes one git command and returns its trimmed standard output.
func (g *gitRunner) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = g.env
	var stdout bytes.Buffer
	stderr := &diagnosticBuffer{}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, g.safeDiagnostic(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// blob returns the exact bytes of a file at a commit, without trimming, so
// its digest matches the committed file. It is bounded like a report.
func (g *gitRunner) blob(ctx context.Context, commit, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "cat-file", "blob", commit+":"+path)
	cmd.Dir = g.dir
	cmd.Env = g.env
	var stdout bytes.Buffer
	stderr := &diagnosticBuffer{}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file: %w: %s", err, g.safeDiagnostic(stderr.String()))
	}
	if stdout.Len() > maxPolicyBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, maxPolicyBytes)
	}
	return stdout.Bytes(), nil
}

// maxPolicyBytes bounds a policy read from a commit.
const maxPolicyBytes = 1 << 20

// prepare initializes an empty repository pointed at the remote.
func (g *gitRunner) prepare(ctx context.Context, cloneURL string) error {
	if cloneURL == "" {
		return fmt.Errorf("repository has no HTTPS clone URL")
	}
	if !strings.HasPrefix(cloneURL, "https://") && !strings.HasPrefix(cloneURL, "http://") {
		return fmt.Errorf("unsupported clone URL scheme")
	}
	if _, err := g.run(ctx, "init", "--quiet", "--initial-branch=probe"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(g.dir, "tmp"), 0o700); err != nil {
		return err
	}
	_, err := g.run(ctx, "remote", "add", "origin", cloneURL)
	return err
}

// fetch brings in the analyzed commit, preferring the branch ref because every
// server allows it, and falling back to fetching the commit directly when the
// branch has already moved on.
func (g *gitRunner) fetch(ctx context.Context, branch, commit string, depth int) error {
	var branchErr error
	if branch != "" {
		_, branchErr = g.run(ctx, "fetch", "--quiet", "--no-tags",
			fmt.Sprintf("--depth=%d", depth), "origin",
			"+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	}
	if g.has(ctx, commit) {
		return nil
	}
	if _, err := g.run(ctx, "fetch", "--quiet", "--no-tags", fmt.Sprintf("--depth=%d", depth), "origin", commit); err != nil {
		if branchErr != nil {
			return branchErr
		}
		return err
	}
	if !g.has(ctx, commit) {
		return fmt.Errorf("commit %s is not reachable on the remote", short(commit))
	}
	return nil
}

// has reports whether a commit object is present locally.
func (g *gitRunner) has(ctx context.Context, rev string) bool {
	if rev == "" {
		return false
	}
	_, err := g.run(ctx, "cat-file", "-e", rev+"^{commit}")
	return err == nil
}

// resolveBase picks the commit the candidate is compared against: the commit
// the branch pointed at before the push when it is still reachable, the first
// parent otherwise, and the candidate itself for an initial commit, which
// yields an empty, honest range rather than a wrong one.
func (g *gitRunner) resolveBase(ctx context.Context, before, head string, depth int) string {
	if commitPattern.MatchString(before) && strings.Trim(before, "0") != "" {
		if g.has(ctx, before) {
			return before
		}
		if _, err := g.run(ctx, "fetch", "--quiet", "--no-tags", fmt.Sprintf("--depth=%d", depth), "origin", before); err == nil && g.has(ctx, before) {
			return before
		}
	}
	if parent, err := g.run(ctx, "rev-parse", "--verify", "--quiet", head+"^"); err == nil && parent != "" {
		return strings.TrimSpace(parent)
	}
	return head
}

// Bound capture independently of the public diagnostic limit. If capture is
// truncated, omit it entirely so a partial known secret cannot evade redaction.
type diagnosticBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (64 << 10) - b.buffer.Len()
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}
func (b *diagnosticBuffer) String() string {
	if b.truncated {
		return "diagnostics exceeded capture limit"
	}
	return b.buffer.String()
}
func (g *gitRunner) safeDiagnostic(message string) string {
	var secrets []string
	for _, value := range g.env {
		if !strings.HasPrefix(value, "GIT_CONFIG_VALUE_") {
			continue
		}
		_, header, _ := strings.Cut(value, "=")
		secrets = append(secrets, header)
		_, auth, ok := strings.Cut(header, ": ")
		if !ok {
			continue
		}
		kind, credential, _ := strings.Cut(auth, " ")
		secrets = append(secrets, auth, credential)
		if strings.EqualFold(kind, "Basic") {
			if decoded, err := base64.StdEncoding.DecodeString(credential); err == nil {
				_, token, _ := strings.Cut(string(decoded), ":")
				secrets = append(secrets, string(decoded), token)
			}
		}
	}
	return store.SafeError(message, secrets...)
}
