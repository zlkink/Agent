package host

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// setupWorktree gives a session its own git worktree, so agent edits never
// touch the project folder until the user merges.
func (h *Host) setupWorktree(s *Session) error {
	s.mu.Lock()
	cwd := s.Cwd
	s.mu.Unlock()

	h.mu.Lock()
	autoPull := bool_(h.settings["worktreeAutoPull"])
	h.mu.Unlock()
	if autoPull {
		pullLatest(cwd)
	}

	top, err := gitOut(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("当前目录不是 Git 仓库")
	}
	top = normalizeDir(strings.TrimSpace(top))
	commit, err := gitOut(cwd, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("无法读取 Git HEAD: %v", err)
	}
	commit = strings.TrimSpace(commit)
	baseBranch := strings.TrimSpace(mustGit(cwd, "rev-parse", "--abbrev-ref", "HEAD"))
	if baseBranch == "HEAD" {
		baseBranch = ""
	}
	branch := "prometheus/" + strings.ReplaceAll(s.ID[:8], "-", "")
	wtPath := filepath.Join(h.cfg.DataDir, "worktrees", s.ID)
	_ = os.MkdirAll(filepath.Dir(wtPath), 0o755)
	if out, err := runGit(top, "worktree", "add", "-b", branch, wtPath, commit); err != nil {
		return fmt.Errorf("git worktree add: %s", lastLine(out))
	}
	rel, err := filepath.Rel(top, cwd)
	if err != nil || rel == "." {
		rel = ""
	}
	wtCwd := wtPath
	if rel != "" {
		wtCwd = filepath.Join(wtPath, rel)
	}
	wt := map[string]any{
		"root": top, "path": wtPath, "cwd": wtCwd, "repo": top,
		"branch": branch, "baseBranch": baseBranch, "baseCommit": commit,
		"state": "active", "createdAt": time.Now().UTC().Format(time.RFC3339),
	}
	s.mu.Lock()
	s.worktree = wt
	s.Cwd = wtCwd
	s.mu.Unlock()
	return nil
}

func (h *Host) worktreeStatus(s *Session) (any, error) {
	return h.worktreeStatusFresh(s, false)
}

func (h *Host) worktreeStatusFresh(s *Session, fresh bool) (any, error) {
	s.mu.Lock()
	wt := s.worktree
	s.mu.Unlock()
	if wt == nil {
		return nil, fmt.Errorf("worktreeGone")
	}
	wtPath := str(wt["path"])
	root := str(wt["root"])
	base := str(wt["baseBranch"])
	state := str(wt["state"])
	if state != "active" {
		return map[string]any{"worktree": wt, "files": []any{}, "ahead": 0, "behind": 0, "conflicts": []any{}, "mainDirty": []any{}, "overlaps": []any{}, "blocked": "worktreeGone"}, nil
	}

	files := changedFiles(wt)
	conflicts := []any{}
	for _, p := range strings.Split(mustGit(wtPath, "diff", "--name-only", "--diff-filter=U"), "\n") {
		if p = strings.TrimSpace(p); p != "" {
			conflicts = append(conflicts, p)
		}
	}
	changed := map[string]bool{}
	for _, f := range files {
		changed[str(asMap(f)["path"])] = true
	}
	mergeBase := str(wt["baseCommit"])
	if out, err := gitOut(wtPath, "merge-base", base, "HEAD"); err == nil && strings.TrimSpace(out) != "" {
		mergeBase = strings.TrimSpace(out)
	}
	ahead := 0
	if mergeBase != "" {
		ahead = atoi(strings.TrimSpace(mustGit(wtPath, "rev-list", "--count", mergeBase+"..HEAD")))
	}
	behind := 0
	if base != "" {
		behind = atoi(strings.TrimSpace(mustGit(wtPath, "rev-list", "--count", "HEAD.."+base)))
	}
	mainDirty := []any{}
	for _, line := range strings.Split(strings.TrimSpace(mustGit(root, "status", "--porcelain=v1")), "\n") {
		if len(line) < 4 {
			continue
		}
		name := strings.TrimSpace(line[3:])
		if changed[name] {
			mainDirty = append(mainDirty, name)
		}
	}

	out := map[string]any{
		"worktree": wt, "files": files, "ahead": ahead, "behind": behind,
		"conflicts": conflicts, "mainDirty": mainDirty, "overlaps": []any{},
	}
	remote := worktreeRemoteOf(wt, files, fresh)
	if remote != nil {
		out["remote"] = remote
	}
	current := strings.TrimSpace(mustGit(root, "rev-parse", "--abbrev-ref", "HEAD"))
	switch {
	case len(conflicts) > 0:
		out["blocked"] = "worktreeConflict"
		out["blockedDetail"] = joinLimited(conflicts, 8)
	case base != "" && current != base:
		out["blocked"] = "worktreeBaseMoved"
		out["blockedDetail"] = current
	case remote != nil && bool_(remote["landed"]):
		out["blocked"] = "worktreeLanded"
		out["blockedDetail"] = str(remote["ref"])
	case remote != nil && len(asList(remote["overlap"])) > 0:
		out["blocked"] = "worktreeRemoteMoved"
		out["blockedDetail"] = str(remote["ref"]) + ": " + joinLimited(asList(remote["overlap"]), 8)
	case len(mainDirty) > 0:
		out["blocked"] = "worktreeMainDirty"
		out["blockedDetail"] = joinLimited(mainDirty, 8)
	case len(files) == 0:
		out["blocked"] = "worktreeEmpty"
	}
	return out, nil
}

func changedFiles(wt map[string]any) []any {
	wtPath := str(wt["path"])
	baseBranch := str(wt["baseBranch"])
	base := str(wt["baseCommit"])
	if out, err := gitOut(wtPath, "merge-base", baseBranch, "HEAD"); err == nil && strings.TrimSpace(out) != "" {
		base = strings.TrimSpace(out)
	}
	out := map[string]string{}
	if raw, err := gitRaw(wtPath, "diff", "--name-status", "-z", base); err == nil {
		parts := strings.Split(raw, "\x00")
		for i := 0; i+1 < len(parts); {
			st := parts[i]
			if st == "" {
				break
			}
			letter := st[:1]
			if letter == "R" || letter == "C" {
				if i+2 >= len(parts) {
					break
				}
				out[parts[i+2]] = "R"
				i += 3
				continue
			}
			out[parts[i+1]] = letter
			i += 2
		}
	}
	// Content that already matches the base branch tip is no longer a change.
	if len(out) > 0 {
		if raw, err := gitRaw(wtPath, "diff", "--name-only", "--no-renames", "-z", baseBranch); err == nil {
			still := nulSet(raw)
			for p := range out {
				if !still[p] {
					delete(out, p)
				}
			}
		}
	}
	if raw, err := gitRaw(wtPath, "ls-files", "--others", "--exclude-standard", "-z", "--", "."); err == nil {
		for p := range nulSet(raw) {
			out[p] = "?"
		}
	}
	delete(out, "")
	paths := make([]string, 0, len(out))
	for p := range out {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	files := make([]any, 0, len(paths))
	for _, p := range paths {
		files = append(files, map[string]any{"path": p, "status": out[p]})
	}
	return files
}

var (
	worktreeFetchMu sync.Mutex
	worktreeFetched = map[string]time.Time{}
)

const worktreeFetchEvery = 60 * time.Second

// worktreeRemoteOf reports what the base branch's upstream knows that the
// project folder does not. Best effort: no upstream or no network returns nil.
func worktreeRemoteOf(wt map[string]any, files []any, fresh bool) map[string]any {
	repo := str(wt["repo"])
	base := str(wt["baseBranch"])
	wtPath := str(wt["path"])
	if repo == "" || base == "" {
		return nil
	}
	up, err := gitOut(repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", base+"@{upstream}")
	if err != nil {
		return nil
	}
	ref := strings.TrimSpace(up)
	if ref == "" {
		return nil
	}
	worktreeFetchMu.Lock()
	last := worktreeFetched[repo]
	if fresh || time.Since(last) > worktreeFetchEvery {
		worktreeFetched[repo] = time.Now()
		worktreeFetchMu.Unlock()
		if remote := strings.TrimSpace(mustGit(repo, "config", "branch."+base+".remote")); remote != "" && remote != "." {
			_, _ = runGitTimeout(10*time.Second, repo, "fetch", "--quiet", remote)
		}
	} else {
		worktreeFetchMu.Unlock()
	}
	behind := atoi(strings.TrimSpace(mustGit(repo, "rev-list", "--count", base+".."+ref)))
	out := map[string]any{"ref": ref, "behind": behind, "landed": false, "overlap": []any{}}
	if behind == 0 || len(files) == 0 {
		return out
	}
	tracked := []string{}
	all := []string{}
	for _, f := range files {
		m := asMap(f)
		p := str(m["path"])
		if p == "" {
			continue
		}
		all = append(all, p)
		if str(m["status"]) != "?" {
			tracked = append(tracked, p)
		}
	}
	if raw, err := gitRaw(wtPath, "diff", "--name-only", "--no-renames", "-z", ref); err == nil {
		still := nulSet(raw)
		landed := len(tracked) == len(files)
		for _, p := range tracked {
			if still[p] {
				landed = false
			}
		}
		out["landed"] = landed
	}
	if !bool_(out["landed"]) {
		moved := map[string]bool{}
		if raw, err := gitRaw(repo, "diff", "--name-only", "--no-renames", "-z", base, ref); err == nil {
			moved = nulSet(raw)
		}
		overlap := []any{}
		for _, p := range all {
			if moved[p] {
				overlap = append(overlap, p)
			}
		}
		out["overlap"] = overlap
	}
	return out
}

func nulSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(raw, "\x00") {
		if p != "" {
			out[p] = true
		}
	}
	return out
}

func joinLimited(list []any, n int) string {
	parts := make([]string, 0, len(list))
	for _, v := range list {
		if len(parts) == n {
			break
		}
		parts = append(parts, str(v))
	}
	return strings.Join(parts, ", ")
}

// pullLatest fast-forwards the current branch before a chat gets its worktree.
// Local edits, no upstream or network failure just leave it where it is.
func pullLatest(dir string) {
	if _, err := gitOut(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err != nil {
		return
	}
	if strings.TrimSpace(mustGit(dir, "status", "--porcelain", "--untracked-files=no")) != "" {
		return
	}
	_, _ = runGitTimeout(20*time.Second, dir, "pull", "--ff-only", "--quiet")
}

// pullBase fast-forwards the project folder's base branch to its upstream once
// the chat's changes already landed there. Anything but a clean fast-forward is
// refused.
func (h *Host) pullBase(s *Session) (any, error) {
	s.mu.Lock()
	wt := s.worktree
	s.mu.Unlock()
	if wt == nil || str(wt["state"]) != "active" {
		return nil, fmt.Errorf("worktreeGone")
	}
	repo := str(wt["root"])
	base := str(wt["baseBranch"])
	if base == "" {
		return nil, fmt.Errorf("worktree has no base branch")
	}
	up, err := gitOut(repo, "rev-parse", "--abbrev-ref", "--symbolic-full-name", base+"@{upstream}")
	if err != nil || strings.TrimSpace(up) == "" {
		return nil, fmt.Errorf("worktreeFailed: no upstream")
	}
	ref := strings.TrimSpace(up)
	if remote := strings.TrimSpace(mustGit(repo, "config", "branch."+base+".remote")); remote != "" && remote != "." {
		if out, err := runGitTimeout(20*time.Second, repo, "fetch", "--quiet", remote); err != nil {
			return nil, fmt.Errorf("拉取失败：%s", lastLine(out))
		}
	}
	current := strings.TrimSpace(mustGit(repo, "rev-parse", "--abbrev-ref", "HEAD"))
	if current == base {
		if out, err := runGit(repo, "merge", "--ff-only", ref); err != nil {
			return nil, fmt.Errorf("无法快进 %s：%s", base, lastLine(out))
		}
	} else {
		if out, err := runGit(repo, "branch", "-f", base, ref); err != nil {
			return nil, fmt.Errorf("更新 %s 失败：%s", base, lastLine(out))
		}
	}
	return h.worktreeStatusFresh(s, true)
}

func (h *Host) worktreeDiff(s *Session, path string) (any, error) {
	s.mu.Lock()
	wt := s.worktree
	s.mu.Unlock()
	if wt == nil {
		return nil, fmt.Errorf("worktreeGone")
	}
	args := []string{"diff", "--no-color", "--unified=3"}
	if path != "" {
		args = append(args, "--", path)
	}
	out := mustGit(str(wt["path"]), args...)
	truncated := false
	if len(out) > 2<<20 {
		out = out[:2<<20]
		truncated = true
	}
	return map[string]any{"text": out, "truncated": truncated}, nil
}

func (h *Host) discardWorktree(s *Session) error {
	s.mu.Lock()
	wt := s.worktree
	s.mu.Unlock()
	if wt == nil {
		return fmt.Errorf("worktreeGone")
	}
	root := str(wt["root"])
	path := str(wt["path"])
	branch := str(wt["branch"])
	if str(wt["state"]) == "active" {
		_, _ = runGit(root, "worktree", "remove", "--force", path)
		_, _ = runGit(root, "branch", "-D", branch)
	}
	s.mu.Lock()
	wt["state"] = "discarded"
	s.Cwd = root
	s.mu.Unlock()
	s.closeClient()
	h.persist()
	return nil
}

func (h *Host) syncWorktree(s *Session) (any, error) {
	s.mu.Lock()
	wt := s.worktree
	s.mu.Unlock()
	if wt == nil {
		return nil, fmt.Errorf("worktreeGone")
	}
	if str(wt["state"]) != "active" {
		return nil, fmt.Errorf("worktreeGone")
	}
	base := str(wt["baseBranch"])
	if base == "" {
		return nil, fmt.Errorf("worktree has no base branch")
	}
	if out, err := runGit(str(wt["path"]), "merge", "--no-edit", base); err != nil {
		// Conflicts stay inside the worktree; the status reports them.
		_ = out
	}
	return h.worktreeStatus(s)
}

func (h *Host) pendingWorktrees() []any {
	h.mu.Lock()
	ids := append([]string(nil), h.order...)
	h.mu.Unlock()
	out := []any{}
	for _, id := range ids {
		s := h.session(id)
		if s == nil {
			continue
		}
		s.mu.Lock()
		wt := s.worktree
		s.mu.Unlock()
		if wt == nil || str(wt["state"]) != "active" {
			continue
		}
		files := 0
		for _, line := range strings.Split(strings.TrimSpace(mustGit(str(wt["path"]), "status", "--porcelain=v1")), "\n") {
			if strings.TrimSpace(line) != "" {
				files++
			}
		}
		out = append(out, map[string]any{"sessionId": id, "branch": str(wt["branch"]), "files": files})
	}
	return out
}

func (h *Host) mergeWorktree(s *Session, strategy, message, resolve string) (any, error) {
	s.mu.Lock()
	wt := s.worktree
	s.mu.Unlock()
	if wt == nil || str(wt["state"]) != "active" {
		return nil, fmt.Errorf("worktreeGone")
	}
	root := str(wt["root"])
	branch := str(wt["branch"])
	base := str(wt["baseBranch"])
	if base == "" {
		return nil, fmt.Errorf("worktree has no base branch")
	}
	if strategy == "" {
		strategy = "squash"
	}
	if message == "" {
		message = "Merge " + branch
	}

	current := strings.TrimSpace(mustGit(root, "rev-parse", "--abbrev-ref", "HEAD"))
	if current != base {
		if resolve != "switchBase" {
			return nil, fmt.Errorf("worktreeBaseMoved: 项目当前在 %s，请先切回 %s", current, base)
		}
		if out, err := runGit(root, "checkout", base); err != nil {
			return nil, fmt.Errorf("切换 %s 失败：%s", base, lastLine(out))
		}
	}

	dirty := strings.TrimSpace(mustGit(root, "status", "--porcelain"))
	stashed := false
	switch {
	case dirty != "" && resolve == "overwrite":
		backupDir := filepath.Join(h.cfg.DataDir, "worktrees", "backups", s.ID, time.Now().UTC().Format("20060102-150405"))
		files, err := backupDirty(root, backupDir)
		if err != nil {
			return nil, err
		}
		_, _ = runGit(root, "checkout", "--", ".")
		_, _ = runGit(root, "clean", "-fd")
		s.mu.Lock()
		wt["backup"] = map[string]any{"dir": backupDir, "files": files}
		s.mu.Unlock()
	case dirty != "" && resolve == "keepMain":
		if out, err := runGit(root, "stash", "push", "--include-untracked", "-m", "prometheus-merge"); err != nil {
			return nil, fmt.Errorf("暂存改动失败：%s", lastLine(out))
		}
		stashed = true
	case dirty != "":
		return nil, fmt.Errorf("worktreeMainDirty: 项目目录有未提交改动，请选择覆盖、保留或先提交")
	}

	var mergeErr error
	if strategy == "apply" {
		diff := mustGit(root, "diff", base+".."+branch)
		if strings.TrimSpace(diff) == "" {
			return h.worktreeStatus(s)
		}
		cmd := exec.Command("git", "-C", root, "apply", "--3way", "-")
		cmd.Stdin = strings.NewReader(diff)
		if out, err := cmd.CombinedOutput(); err != nil {
			mergeErr = fmt.Errorf("worktreeConflict: %s", lastLine(string(out)))
		}
	} else if strategy == "squash" {
		if out, err := runGit(root, "merge", "--squash", branch); err != nil {
			_, _ = runGit(root, "reset", "--hard", "HEAD")
			mergeErr = fmt.Errorf("worktreeConflict: %s", lastLine(out))
		} else if out, err := runGit(root, "commit", "-m", message); err != nil {
			mergeErr = fmt.Errorf("%s", lastLine(out))
		}
	} else {
		if out, err := runGit(root, "merge", "--no-ff", "-m", message, branch); err != nil {
			_, _ = runGit(root, "merge", "--abort")
			mergeErr = fmt.Errorf("worktreeConflict: %s", lastLine(out))
		}
	}
	if stashed {
		_, _ = runGit(root, "stash", "pop")
	}
	if mergeErr != nil {
		return nil, mergeErr
	}

	commit := strings.TrimSpace(mustGit(root, "rev-parse", "HEAD"))
	s.mu.Lock()
	wt["baseCommit"] = commit
	wt["state"] = "active"
	s.mu.Unlock()
	h.persist()
	return h.worktreeStatus(s)
}

func (h *Host) restoreBackup(s *Session, drop bool) (any, error) {
	s.mu.Lock()
	wt := s.worktree
	s.mu.Unlock()
	if wt == nil {
		return nil, fmt.Errorf("worktreeGone")
	}
	raw, _ := wt["backup"].(map[string]any)
	dir := str(raw["dir"])
	if dir == "" {
		return h.worktreeStatus(s)
	}
	if !drop {
		if err := restoreDirty(str(wt["root"]), dir); err != nil {
			return nil, err
		}
	}
	_ = os.RemoveAll(dir)
	s.mu.Lock()
	delete(wt, "backup")
	s.mu.Unlock()
	h.persist()
	return h.worktreeStatus(s)
}

func backupDirty(root, backupDir string) ([]string, error) {
	out := mustGit(root, "status", "--porcelain=v1")
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if len(line) < 4 {
			continue
		}
		name := strings.TrimSpace(line[3:])
		if i := strings.LastIndex(name, " -> "); i >= 0 {
			name = name[i+4:]
		}
		src := filepath.Join(root, filepath.FromSlash(name))
		fi, err := os.Stat(src)
		if err != nil || fi.IsDir() {
			continue
		}
		dst := filepath.Join(backupDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := copyFile(src, dst); err != nil {
			return nil, err
		}
		files = append(files, name)
	}
	return files, nil
}

func restoreDirty(root, backupDir string) error {
	return filepath.Walk(backupDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(backupDir, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(path, dst)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runGitTimeout(d time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func gitRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	return string(out), err
}

func gitOut(dir string, args ...string) (string, error) {
	return runGit(dir, args...)
}

func mustGit(dir string, args ...string) string {
	out, _ := runGit(dir, args...)
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
