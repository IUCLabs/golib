VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Minimum total statement coverage for the shipped packages. golib is small,
# dependency-light and consumed by every service, so everything it exports is
# expected to have a test.
COVER_MIN := 90

# Packages excluded from the coverage gate: pgtest exists only to be imported
# from other repos' _test.go files, and is itself exercised by the
# container-backed integration tests, which the gate deliberately does not run.
COVER_EXCLUDE := /pg/pgtest/

.PHONY: all build test test-all cover vet fmt lint tidy clean pre-commit patch minor major

all: build

# Mirror the CI pre-commit checks locally.
pre-commit:
	test -z "$$(gofmt -l .)"
	go vet ./...

# golib ships no binaries; building is a compile check over every package.
build:
	go build ./...

# Unit tests. The container-backed integration tests skip themselves under
# -short, which is how CI runs them: the runners have no Docker.
test:
	go test -race -count=1 -short ./...

# Everything, including the integration tests. Needs a container runtime.
test-all:
	TESTCONTAINERS_RYUK_DISABLED=true go test -race -count=1 ./...

# Coverage gate. Unit tests alone must clear COVER_MIN, so the gate runs
# unchanged on a runner with no container runtime.
cover:
	go test -race -count=1 -short -coverprofile=cover.out ./...
	@grep -Ev '$(COVER_EXCLUDE)' cover.out > cover.filtered.out
	@go tool cover -func=cover.filtered.out | awk -v min=$(COVER_MIN) ' \
		/^total:/ { \
			seen = 1; pct = $$3; sub(/%/, "", pct); \
			printf "total coverage: %s%% (minimum %d%%)\n", pct, min; \
			if (pct + 0 < min + 0) { print "coverage below minimum" > "/dev/stderr"; exit 1 } \
		} \
		END { if (!seen) { print "no total in coverage output" > "/dev/stderr"; exit 1 } }'

vet:
	go vet ./...

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

clean:
	rm -f cover.out cover.filtered.out

DEFAULT_BRANCH ?= main

# Publish the release with generated notes, then push the tag; no workflow
# fills the release, so it is not drafted.
patch minor major:
	@git fetch --quiet --tags origin $(DEFAULT_BRANCH)
	@test "$$(git branch --show-current)" = "$(DEFAULT_BRANCH)" || { echo "ERROR: not on $(DEFAULT_BRANCH)"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "ERROR: working tree not clean"; exit 1; }
	@test "$$(git rev-parse HEAD)" = "$$(git rev-parse origin/$(DEFAULT_BRANCH))" || { echo "ERROR: HEAD is not origin/$(DEFAULT_BRANCH)"; exit 1; }
	@LATEST=$$(git tag --list 'v*' --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | head -1); \
	NEW=$$(echo "$${LATEST:-v0.0.0}" | sed 's/^v//' | awk -F. -v b=$@ '{ \
		if (b == "major") print "v" ($$1+1) ".0.0"; \
		else if (b == "minor") print "v" $$1 "." ($$2+1) ".0"; \
		else print "v" $$1 "." $$2 "." ($$3+1) }'); \
	SHA=$$(git rev-parse HEAD); \
	gh release create "$$NEW" --target "$$SHA" --generate-notes --title "$$NEW" || exit 1; \
	git push origin "$$SHA:refs/tags/$$NEW" || { gh release delete "$$NEW" --yes; exit 1; }
