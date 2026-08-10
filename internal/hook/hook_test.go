package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scratchRepo builds a real repository, because every hook path runs git.
func scratchRepo(t *testing.T, optIn bool) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	os.MkdirAll(filepath.Join(dir, "internal"), 0o755)
	os.WriteFile(filepath.Join(dir, "internal", "a.go"), []byte("package a\n"), 0o644)
	if optIn {
		os.MkdirAll(filepath.Join(dir, ".dlog"), 0o755)
	}
	return dir
}

func run(t *testing.T, event, dir string, payload map[string]any) (int, string, string) {
	t.Helper()
	if payload == nil {
		payload = map[string]any{}
	}
	payload["cwd"] = dir
	data, _ := json.Marshal(payload)
	var out, errb bytes.Buffer
	code := Run(event, bytes.NewReader(data), &out, &errb)
	return code, out.String(), errb.String()
}

// Installed globally, the hooks fire in every repository. One without .dlog/ has
// not asked to be tracked, and must cost it nothing: no journal, no nudge, no
// output on either stream.
func TestUnoptedRepoIsUntouched(t *testing.T) {
	dir := scratchRepo(t, false)
	for _, event := range []string{SessionStart, Stop, SubagentStop, PreCompact, PostToolUse} {
		code, out, errOut := run(t, event, dir, map[string]any{"session_id": "s"})
		if code != 0 {
			t.Errorf("%s: exit %d, want 0", event, code)
		}
		if out != "" || errOut != "" {
			t.Errorf("%s: spoke in an un-opted repo\nstdout: %q\nstderr: %q", event, out, errOut)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".dlog")); !os.IsNotExist(err) {
		t.Error("hooks created .dlog/ in a repo that never opted in")
	}
}

func TestOptedInRepoJournals(t *testing.T) {
	dir := scratchRepo(t, true)
	code, _, errOut := run(t, SubagentStop, dir, map[string]any{
		"session_id": "sess1", "agent_type": "qa", "last_assistant_message": "rejected hard delete",
	})
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, errOut)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".dlog", "journal", "sess1.jsonl"))
	if err != nil {
		t.Fatalf("no journal written: %v", err)
	}
	if !strings.Contains(string(data), "rejected hard delete") {
		t.Errorf("summary not captured: %s", data)
	}
}

// Outside a repository there is nothing to resolve and nothing to say. A global
// hook that complained here would do it in every non-git directory.
func TestNonRepoIsSilent(t *testing.T) {
	dir := t.TempDir()
	code, out, errOut := run(t, Stop, dir, map[string]any{"session_id": "s"})
	if code != 0 || out != "" || errOut != "" {
		t.Errorf("exit %d, stdout %q, stderr %q; want silence", code, out, errOut)
	}
}

// G7: no watched changes means no output at all.
func TestStopIsSilentWithNoWatchedChanges(t *testing.T) {
	dir := scratchRepo(t, true)
	code, out, _ := run(t, Stop, dir, map[string]any{"session_id": "s", "last_assistant_message": "x"})
	if code != 0 || out != "" {
		t.Errorf("exit %d, stdout %q; want silence", code, out)
	}
}

// An unknown event is a wiring mistake, and one that would otherwise lose every
// turn it was meant to record, so it is the one case that does report.
func TestUnknownEventReports(t *testing.T) {
	dir := scratchRepo(t, true)
	code, _, errOut := run(t, "not-an-event", dir, nil)
	if code == 0 || !strings.Contains(errOut, "unknown event") {
		t.Errorf("exit %d, stderr %q", code, errOut)
	}
}

// A payload that is not JSON must not fail the turn it is observing.
func TestGarbagePayloadDoesNotFailTheTurn(t *testing.T) {
	dir := scratchRepo(t, true)
	var out, errb bytes.Buffer
	if code := Run(Stop, strings.NewReader("{not json"), &out, &errb); code != 0 {
		t.Errorf("exit %d, want 0", code)
	}
	_ = dir
}
