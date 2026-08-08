BIN := .claude/bin/dlog

.PHONY: install test check clean

# The hooks invoke this path. Nothing works until it exists, which is why the
# SessionStart entry in .claude/settings.json says so out loud (prd §12).
install: $(BIN)

$(BIN): $(shell find . -name '*.go' -not -path './.git/*') go.mod
	@mkdir -p $(dir $@)
	go build -o $@ ./cmd/dlog

test:
	go vet ./...
	go test ./...

# The M1 and M2 done-conditions (prd §13) are cross-process and cross-worktree,
# so they live in a script rather than in `go test`.
check: install test
	./scripts/verify-milestones.sh

clean:
	rm -f $(BIN)
