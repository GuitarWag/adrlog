// Package config loads .dlog/config.json (prd §6.4). Every field is optional;
// a missing file is not an error, it is the defaults.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Config struct {
	Watch                []string `json:"watch"`
	Ignore               []string `json:"ignore"`
	JournalCommitted     bool     `json:"journal_committed"`
	MinFiles             int      `json:"min_files"`
	CooldownSeconds      int      `json:"cooldown_seconds"`
	Enforce              bool     `json:"enforce"`
	CtxLimit             int      `json:"ctx_limit"`
	DriftCommitThreshold int      `json:"drift_commit_threshold"`
	DriftWindowDays      int      `json:"drift_window_days"`
	ProposedStaleDays    int      `json:"proposed_stale_days"`
	JournalRetentionDays int      `json:"journal_retention_days"`
}

func Default() Config {
	return Config{
		Watch:                []string{"internal/**", "cmd/**", "migrations/**", "api/**", "**/*.tf"},
		Ignore:               []string{"**/*_test.go", "**/testdata/**", "docs/**"},
		JournalCommitted:     false,
		MinFiles:             2,
		CooldownSeconds:      900,
		Enforce:              false,
		CtxLimit:             5,
		DriftCommitThreshold: 20,
		DriftWindowDays:      90,
		ProposedStaleDays:    14,
		JournalRetentionDays: 90,
	}
}

// Path is where config lives, under the shared root so all worktrees agree.
func Path(root string) string { return filepath.Join(root, ".dlog", "config.json") }

// Load reads the config, falling back to defaults for a missing file. A file that
// exists but does not parse is an error, never a silent reset to defaults — a
// typo'd watch list that quietly reverts is the kind of failure nobody notices.
func Load(root string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(Path(root))
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Default(), fmt.Errorf("%s: %w", Path(root), err)
	}
	return cfg, nil
}
