// Package gitx resolves repository state through the git CLI.
//
// Everything adrlog knows about the world comes from git and the filesystem — no
// network, no service, no LLM. That is also what keeps the rest of the tool
// testable against a scratch repo.
package gitx

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
)

// Repo is a handle on one worktree of a repository.
//
// A Repo is a short-lived, single-shot view: a hook process opens one, reads what
// it needs and exits. Every field is resolved up front and the changed-file list
// is cached, because each git subprocess costs about 9ms and the hook budget is
// 50ms at p99.
type Repo struct {
	Dir  string // the worktree we were invoked from
	root string // shared repository root, resolved once
	top  string // this worktree's toplevel
	head string
	ref  string

	changed    []string
	changedErr error
	haveChange bool
}

// Open resolves the repository containing dir. dir may be any directory inside a
// worktree; "" means the process working directory.
func Open(dir string) (*Repo, error) {
	r := &Repo{Dir: dir}

	// One subprocess for four facts. The argument order matters: --abbrev-ref is
	// sticky, so the bare HEAD before it yields the sha and the HEAD after it
	// yields the branch name. Putting --short in this list breaks the whole call.
	out, err := r.git("rev-parse", "--git-common-dir", "--show-toplevel", "HEAD", "--abbrev-ref", "HEAD")
	lines := strings.Split(out, "\n")
	if err != nil || len(lines) < 4 {
		// A repository with no commits yet cannot resolve HEAD, and that is not a
		// reason to refuse to journal. Fall back to the two facts that always exist.
		out, err = r.git("rev-parse", "--git-common-dir", "--show-toplevel")
		if err != nil {
			return nil, err
		}
		lines = strings.Split(out, "\n")
		if len(lines) < 2 {
			return nil, errors.New("git rev-parse returned no usable output")
		}
		lines = append(lines, "", "")
	}

	// The shared root, not the worktree. --show-toplevel alone would
	// scatter .adrlog/ across five parallel sessions and lose journals when an
	// unchanged worktree is auto-removed at session end.
	common := lines[0]
	if !filepath.IsAbs(common) {
		// git reports this one relative to the working directory, not the toplevel.
		base := dir
		if base == "" {
			base = "."
		}
		common = filepath.Join(base, common)
	}
	if common, err = filepath.Abs(common); err != nil {
		return nil, err
	}
	r.root = filepath.Dir(common)
	r.top = lines[1]
	r.head = lines[2]
	r.ref = lines[3]
	return r, nil
}

// Root is the shared repository root. All adrlog state — docs/adr/ and .adrlog/ —
// resolves against this, so five worktrees write to one place.
func (r *Repo) Root() string { return r.root }

// Toplevel is this worktree's own checkout path.
func (r *Repo) Toplevel() string { return r.top }

// WorktreeName keys per-worktree state. A nudge fingerprint from one
// worktree must not suppress a nudge in another, because their changed-file sets
// differ — sharing that state would be wrong, not merely racy.
func (r *Repo) WorktreeName() string {
	if r.top == "" {
		return "unknown"
	}
	return filepath.Base(r.top)
}

// Branch returns the current branch name, or "" when detached or unreadable.
func (r *Repo) Branch() string {
	if r.ref == "HEAD" {
		return "" // detached
	}
	return r.ref
}

// headLen is how much of the sha we keep. git's own short form is variable and
// would cost another subprocess to compute; 12 is unambiguous well past the size
// of any repo this runs in, at the cost of a few characters against git's own
// 7-character short form.
const headLen = 12

// Head returns the abbreviated HEAD sha, or "" on an unborn branch.
func (r *Repo) Head() string {
	if len(r.head) > headLen {
		return r.head[:headLen]
	}
	return r.head
}

// ChangedFiles lists uncommitted paths in this worktree, relative to its
// toplevel. Includes untracked files: a decision landing in a brand-new file is
// exactly the case the nudge exists to catch.
//
// Cached: the Stop hook needs this twice, once to journal and once to decide
// whether to nudge, and a second `git status` would spend a fifth of the hook
// budget re-deriving an answer that cannot have changed mid-process.
func (r *Repo) ChangedFiles() ([]string, error) {
	if r.haveChange {
		return r.changed, r.changedErr
	}
	r.haveChange = true
	r.changed, r.changedErr = r.readChangedFiles()
	return r.changed, r.changedErr
}

func (r *Repo) readChangedFiles() ([]string, error) {
	out, err := r.git("status", "--porcelain", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(out, "\x00")
	var files []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		status, path := f[:2], f[3:]
		if status[0] == 'R' || status[0] == 'C' {
			// Rename and copy entries carry the source path in the next NUL field.
			// Report the destination and skip the source.
			i++
		}
		files = append(files, path)
	}
	return files, nil
}

// IsDirty reports whether a repo-relative path has uncommitted modifications in
// this worktree. Used to warn before editing a supersede target another session
// may be holding dirty.
func (r *Repo) IsDirty(rel string) bool {
	out, err := r.git("status", "--porcelain", "--", rel)
	return err == nil && strings.TrimSpace(out) != ""
}

// TrackedFiles lists tracked paths, for the affects rot check.
func (r *Repo) TrackedFiles() ([]string, error) {
	out, err := r.git("ls-files")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

func (r *Repo) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}
