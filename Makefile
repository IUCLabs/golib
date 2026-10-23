VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# Minimum total statement coverage for the shipped packages. golib is small,
# dependency-light and consumed by every service, so everything it exports is
# expected to have a test.
COVER_MIN := 90

# Packages excluded from the coverage gate: pgtest exists only to be imported
# from other repos' _test.go files, and is itself exercised by the
# container-backed integration tests, which the gate deliberately does not run.
COVER_EXCLUDE := /pg/pgtest/

.PHONY: all build test test-all cover vet fmt lint tidy clean pre-commit patch minor major _tag

all: build

# Mirror the .woodpecker/pre-commit.yaml checks locally.
pre-commit:
	test -z "$$(gofmt -l .)"
	go vet ./...

# golib ships no binaries; building is a compile check over every package.
build:
	go build ./...

# Unit tests. The container-backed integration tests skip themselves under
# -short, which is how CI runs them: the Kubernetes runners have no Docker.
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

# Bump helpers — read the latest semver tag and create the next one. Consumers
# pin the resulting v* tag with `go get git.unkin.net/unkin/golib@vX.Y.Z`.
_LATEST := $(shell git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | head -1)
_BASE   := $(if $(_LATEST),$(_LATEST),v0.0.0)
_MAJ    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f1)
_MIN    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f2)
_PAT    := $(shell echo $(_BASE) | sed 's/^v//' | cut -d. -f3)

patch:
	@NEW=v$(_MAJ).$(_MIN).$(shell expr $(_PAT) + 1); \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

minor:
	@NEW=v$(_MAJ).$(shell expr $(_MIN) + 1).0; \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

major:
	@NEW=v$(shell expr $(_MAJ) + 1).0.0; \
	git tag $$NEW && echo "Tagged $$NEW" && $(MAKE) _tag TAG=$$NEW

_tag:
	git push origin $(TAG)
