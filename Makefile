BIN    := .claude/bin/adrlog
GLOBAL := $(HOME)/.local/bin/adrlog
SRC    := $(shell find . -name '*.go' -not -path './.git/*') go.mod

.PHONY: install install-global test check clean

# Local build, used by the test suite and the verification script.
install: $(BIN)

$(BIN): $(SRC)
	@mkdir -p $(dir $@)
	go build -o $@ ./cmd/adrlog

# The real install. One binary on PATH, wired once in ~/.claude/settings.json,
# and inert in any repository that has not opted in by having a .adrlog/ directory
# (see hook.OptedIn). A repo switches itself on the first time `adrlog new` runs.
install-global: $(GLOBAL)

$(GLOBAL): $(SRC)
	@mkdir -p $(dir $@)
	go build -o $@ ./cmd/adrlog
	@echo "installed $@"

test:
	go vet ./...
	go test ./...

# The M1 and M2 done-conditions (docs/future-work.md) are cross-process and cross-worktree,
# so they live in a script rather than in `go test`.
check: install test
	./scripts/verify-milestones.sh

clean:
	rm -f $(BIN)
