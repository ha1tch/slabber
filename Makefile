# Makefile for github.com/ha1tch/slabber
#
# Targets:
#   make               — default: vet + test
#   make build         — compile package (no output binary)
#   make test          — run all tests
#   make test-race     — run all tests with race detector
#   make test-verbose  — run all tests with verbose output
#   make bench         — run all benchmarks (3 runs each)
#   make bench-alloc   — Alloc/* benchmarks only
#   make bench-free    — Free/* benchmarks only
#   make bench-slot    — Slot benchmark only
#   make bench-occ     — Occupancy/* benchmarks (sorted vs unsorted curve)
#   make bench-sort    — Sort/* benchmarks (sort cost at varying fill)
#   make bench-par     — Parallel/* benchmarks across -cpu=1,2,4,8
#   make bench-arena   — Arena/* benchmarks
#   make bench-compare — cross-variant comparison (v0 vs v1 vs v2 vs v3)
#   make bench-compare-par — parallel comparison 1 bucket, -cpu=1,2,4,8
#   make bench-compare-nb  — parallel comparison N buckets (primary), -cpu=1,2,4,8
#   make vet           — run go vet
#   make fmt           — format source files
#   make fmt-check     — check formatting without modifying files
#   make release-check — verify VERSION, version.go, and CHANGELOG are in sync
#   make clean         — remove build artefacts and benchmark output
#   make help          — print this message

PACKAGE   := github.com/ha1tch/slabber
BENCH_OUT := bench.out
BENCH_N   := 3
BENCH_TIME := 1s

GO        := go
GOFLAGS   :=

.PHONY: all build test test-race test-verbose vet fmt fmt-check \
        bench bench-alloc bench-free bench-slot bench-occ bench-sort \
        bench-par bench-arena \
        bench-compare bench-compare-par bench-compare-nb \
        release-check clean help

all: vet test

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

build:
	$(GO) build $(GOFLAGS) ./...

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

test:
	$(GO) test $(GOFLAGS) -count=1 ./...

test-race:
	$(GO) test $(GOFLAGS) -race -count=1 ./...

test-verbose:
	$(GO) test $(GOFLAGS) -v -count=1 ./...

# ---------------------------------------------------------------------------
# Benchmarks
#
# -benchmem    — report allocations per op
# -count=N     — repeat each benchmark N times (helps with variance)
# -benchtime   — minimum time per benchmark run
# ---------------------------------------------------------------------------

BENCH_FLAGS := -benchmem -count=$(BENCH_N) -benchtime=$(BENCH_TIME)

bench:
	$(GO) test $(GOFLAGS) -run='^$$' -bench=. $(BENCH_FLAGS) ./... \
	    | tee $(BENCH_OUT)

bench-alloc:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkAlloc' $(BENCH_FLAGS) ./...

bench-free:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkFree' $(BENCH_FLAGS) ./...

bench-slot:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkSlot' $(BENCH_FLAGS) ./...

bench-occ:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkOccupancy' $(BENCH_FLAGS) ./...

bench-sort:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkSort' $(BENCH_FLAGS) ./...

bench-par:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkParallel' $(BENCH_FLAGS) \
	    -cpu=1,2,4,8 ./...

bench-arena:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkArena' $(BENCH_FLAGS) ./...

bench-compare:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkCompare_' $(BENCH_FLAGS) ./...

bench-compare-par:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkCompare_Parallel' $(BENCH_FLAGS) \
	    -cpu=1,2,4,8 ./...

bench-compare-nb:
	$(GO) test $(GOFLAGS) -run='^$$' -bench='^BenchmarkCompare_Parallel.*_NB' $(BENCH_FLAGS) \
	    -cpu=1,2,4,8 ./...

# ---------------------------------------------------------------------------
# Code quality
# ---------------------------------------------------------------------------

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then \
	    echo "Files not formatted:"; \
	    echo "$$out"; \
	    exit 1; \
	fi

# ---------------------------------------------------------------------------
# Release hygiene
#
# Verifies that VERSION, pkg/version/version.go, and the top entry in
# docs/CHANGELOG.md all agree on the current version string.
# Run before cutting any tag or zip checkpoint.
# ---------------------------------------------------------------------------

release-check:
	@version=$$(cat VERSION | tr -d '[:space:]'); \
	changelog=$$(grep -m1 '^## \[' docs/CHANGELOG.md | sed 's/^## \[//;s/\].*//' | tr -d '[:space:]'); \
	code=$$(grep 'Version =' version.go | sed 's/.*"//;s/".*//' | tr -d '[:space:]'); \
	echo "VERSION file : $$version"; \
	echo "version.go   : $$code"; \
	echo "docs/CHANGELOG.md: $$changelog"; \
	failed=0; \
	if [ "$$version" != "$$changelog" ]; then \
	    echo "MISMATCH: VERSION ($$version) != docs/CHANGELOG.md ($$changelog)"; failed=1; \
	fi; \
	if [ "$$version" != "$$code" ]; then \
	    echo "MISMATCH: VERSION ($$version) != version.go ($$code)"; failed=1; \
	fi; \
	if [ "$$failed" = "0" ]; then \
	    echo "OK — all three agree on v$$version"; \
	fi; \
	exit $$failed

# ---------------------------------------------------------------------------
# Clean
# ---------------------------------------------------------------------------

clean:
	$(GO) clean ./...
	rm -f $(BENCH_OUT)

# ---------------------------------------------------------------------------
# Help
# ---------------------------------------------------------------------------

help:
	@grep -E '^#   make' Makefile | sed 's/^# //'
