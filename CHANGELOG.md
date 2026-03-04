# Changelog

All notable changes to slabber will be documented here.

Format: [Semantic Versioning](https://semver.org/spec/v2.0.0.html)

---

## [0.2.3] - 2026-03-03

### Added

- **`README.md`**: installation, quick start, configuration, Arena usage,
  benchmark results table (Apple M1, Go 1.24), concurrency model description,
  safety summary, variant history, and full make target reference.
- **`LICENSE`**: Apache 2.0.
- **`docs/CHANGELOG.md`**: changelog moved to `docs/`; root `CHANGELOG.md`
  retained for GitHub rendering.
- **`make release-check`**: verifies `VERSION`, `version.go`, and
  `docs/CHANGELOG.md` all agree on the current version string before any
  tag or checkpoint is cut.

### Removed

- Backup files (`*.bak`, `*.bak2`) removed from repository.

---

## [0.2.2] - 2026-03-03

### Added

- **`TestConcurrentGrow`**: 32 goroutines race to exhaust bucket 0 simultaneously,
  exercising the `growMu` re-check that prevents duplicate bucket appends.
  Verifies no double-allocation occurs and bucket count stays bounded.
- **`TestConcurrentMultiBucket`**: `runtime.NumCPU()` pre-warmed buckets with
  concurrent writer goroutines (alloc/free) and reader goroutines (`Slot()`)
  running simultaneously. The primary race-detector test for the intended
  production configuration.
- **`TestConcurrentSlotWhileGrowing`**: a single ref allocated in bucket 0 is
  continuously read via `Slot()` while other goroutines force repeated bucket
  growth. Verifies the `atomic.Pointer` snapshot safety property — old
  snapshots remain valid after a grow.

---

## [0.2.1] - 2026-03-03

### Fixed

- **Native atomic methods restored**: `atomicOr64`/`atomicAnd64` CAS-loop
  shims removed from root and `variants/v2`; replaced with native
  `atomic.Uint64.Or`/`.And` (available since Go 1.23). The shims were
  introduced for Go 1.22 compatibility but caused measurable throughput
  regression in `variants/v2` benchmarks.
- **`go.mod` minimum version**: updated to `go 1.23`.
- **Missing v3 sub-benchmark in `BenchmarkCompare_Parallel_SlotRead_NB`**:
  the v3 `Run` block was absent due to a substitution error during benchmark
  construction. Added; v3 SlotRead now appears in benchmark output.

---

## [0.2.0] - 2026-03-03

### Added

- **`variants/v2`**: the previous root (v2, RWMutex + lockMask) frozen as an
  importable subpackage for benchmarking comparison.
- **v3 root (this release)**: `atomic.Pointer` bucket list replaces
  `sync.RWMutex` on the read path.
  - `Slabber.buckets` is now `atomic.Pointer[[]*bucket]`. Readers (`Slot`,
    `Free`, Alloc scan) load the pointer with a single atomic read — no mutex
    at all on the read path.
  - `growMu sync.Mutex` serialises `grow()` only. It builds a new slice,
    appends the bucket, and atomically swaps the pointer. Concurrent readers
    that loaded the old snapshot continue safely; they will find no free slot
    and retry with the fresh pointer.
  - `grow()` re-checks for free slots under `growMu` before appending, so
    concurrent growers do not each add a redundant bucket.
  - `Slot()` is now fully lock-free: one `atomic.Pointer.Load()`, one slice
    index, one slice return.
  - `lockMask` semantics unchanged from v2 (intent before contention).
  - `Stats()` lock-free on the bucket list (freeCount still atomic).
- **Go 1.22 compatibility**: `atomic.Uint64.Or` / `.And` (added in Go 1.23)
  replaced with CAS-loop helpers `atomicOr64` / `atomicAnd64`.
- **v3 sub-benchmarks** added to all comparison benchmark functions.

### Changed

- `slabber_test.go`: internal `s.buckets[i]` accesses updated to
  `(*s.buckets.Load())[i]` to match the new field type.

---

## [0.1.2] - 2026-03-03

### Added

- **`Config.Buckets` field**: number of arenas to pre-allocate at construction
  time. Set to `runtime.NumCPU()` so goroutines can spread across arenas
  immediately without racing to grow. 0 defaults to 1 (previous behaviour).
- **`NewSlabber(slotSize, buckets int)`**: ergonomic constructor for the common
  case. Equivalent to `New(Config{SlotSize: s, Buckets: n})` with default
  `SortThreshold`. `New(cfg)` is unchanged.
- **`_NB` comparison benchmarks** (`BenchmarkCompare_Parallel_AllocFree_NB`,
  `BenchmarkCompare_Parallel_SlotRead_NB`): pre-allocate `runtime.NumCPU()`
  buckets before measuring. This is the intended use case and the primary
  benchmark for evaluating lock steering. Previous parallel benchmarks renamed
  to `_1B` (single-bucket, degenerate case).
- **`make bench-compare-nb`**: runs only the `_NB` parallel group.
- `Config.Buckets` and `initialBuckets()` helper added to `variants/v0` and
  `variants/v1` for consistent comparison.

---

## [0.1.1] - 2026-03-03

### Fixed

- **Lock steering intent ordering** (all four lock sites): `lockMask.Or(bit)`
  now executes *before* `b.mu.Lock()` or `TryLock` in every code path —
  fast path, slow path, grow path, and `Free()`. Previously the bit was set
  after the lock was acquired, meaning the sign went up only after contention
  was already resolved. Other goroutines now see the intent signal while the
  lock is being contested, which is the point at which steering helps.
- **Fast path mask refresh**: the hint bitmask (`hints`) is now recomputed
  from `lockMask` on every loop iteration using a `tried` accumulator to
  avoid re-visiting the same bucket. Previously a stale snapshot was taken
  once before the loop, causing goroutines to miss buckets that became
  available mid-loop.
- **Fast path TryLock ordering**: `lockMask.Or(bit)` now precedes the
  `TryLock` call. If `TryLock` fails the bit is cleared immediately. Other
  goroutines that read the mask between `Or` and `TryLock` correctly see the
  bucket as contested and steer away.

---

## [0.1.0] - 2026-03-03

### Added

- **Lock steering** (`lockMask atomic.Uint64`): Alloc reads the inverted lock
  mask to steer `TryLock` attempts toward unlocked buckets, avoiding contention
  before it occurs. Covers buckets 0–63; falls back to blocking scan beyond that.
- **`sync.RWMutex` on `Slabber`**: Alloc scan, Free, and Slot take `RLock`
  briefly to read the bucket list pointer; only `grow()` takes a write lock.
  Slot reads now scale near-linearly with core count.
- **`atomic.Int32` freeCount per bucket**: readable without `b.mu` as a cheap
  hint to skip full buckets. Authoritatively updated under `b.mu`.
- **Counting sort in `doSort()`**: O(n) counting sort on popcount values [0,64]
  replaces O(n log n) `sort.Slice`, shortening the window during which `b.mu`
  is held by background sort goroutines.
- **`runtime.Gosched()` in `Sort()` spin-wait**: replaces bare busy-loop,
  preventing potential livelock under goroutine pressure.
- **Fast path in `grow()`**: new bucket slot 0 is allocated directly without
  calling `findFreeSlot`, saving one redundant scan.
- **Variant preservation**: `variants/v0` and `variants/v1` are importable
  subpackages preserving earlier implementations for benchmarking comparison.
- **`compare_bench_test.go`** (`package slabber_test`): blackbox head-to-head
  benchmarks across v0, v1, and v2 for AllocFree, SlotRead, HighOccupancy,
  HighOccupancyAfterSort, and parallel variants of each.
- **Makefile targets**: `bench-compare`, `bench-compare-par`.

### Changed

- `Slabber.mu` is now `sync.RWMutex` (was `sync.Mutex`).
- `bucket.freeCount` is now `atomic.Int32` (was `int`).

---

## [0.0.1] - 2026-03-03

Initial release.

### Features

- Fixed-size slot allocator over large contiguous byte arenas
- `[1024]uint64` bitmap per bucket — one bit per slot, O(1) alloc/free via `bits.TrailingZeros64`
- `[1024]uint16` order array — controls scan sequence, sorted ascending by popcount so sparsely-occupied words are found first
- `[1024]uint16` inverseOrder array — maps physical word → logical scan position, enabling O(1) cursor retraction on `Free()`
- Auto-triggered background sort via `sortAsync()` when cursor crosses `SortThreshold`
- Configurable `SortThreshold` per `Slabber` (default: 256 bitmap words)
- Explicit `Sort()` for guaranteed sort points (e.g. before benchmarks)
- Double-free guard
- Bucket growth on demand — new 256MB arena appended when all existing buckets are full
- `Arena` multi-size-class allocator routing allocations to the appropriate `Slabber` by value size
- `DefaultArena()` with four size classes: ≤64B, ≤512B, ≤4KB, ≤64KB
- `Stats()` on both `Slabber` and `Arena`
- Comprehensive test suite: correctness, double-free, slot reuse, bucket growth, inverseOrder consistency, cursor retraction, auto-sort, concurrent alloc/free (race-detector clean)
- Benchmark suite across seven groups: Alloc, Free, Slot, Occupancy sweep (0–90% fill × sorted/unsorted), Sort cost, Parallel, Arena

### Notes

- `Slabber` and `Arena` are safe for concurrent use
- Bucket data (`[]byte` arenas) is never freed; the GC sees one object per bucket regardless of keycount
- Slot size is fixed per `Slabber`; use `Arena` for mixed-size workloads


Initial release.

### Features

- Fixed-size slot allocator over large contiguous byte arenas
- `[1024]uint64` bitmap per bucket — one bit per slot, O(1) alloc/free via `bits.TrailingZeros64`
- `[1024]uint16` order array — controls scan sequence, sorted ascending by popcount so sparsely-occupied words are found first
- `[1024]uint16` inverseOrder array — maps physical word → logical scan position, enabling O(1) cursor retraction on `Free()`
- Auto-triggered background sort via `sortAsync()` when cursor crosses `SortThreshold`
- Configurable `SortThreshold` per `Slabber` (default: 256 bitmap words)
- Explicit `Sort()` for guaranteed sort points (e.g. before benchmarks)
- Double-free guard
- Bucket growth on demand — new 256MB arena appended when all existing buckets are full
- `Arena` multi-size-class allocator routing allocations to the appropriate `Slabber` by value size
- `DefaultArena()` with four size classes: ≤64B, ≤512B, ≤4KB, ≤64KB
- `Stats()` on both `Slabber` and `Arena`
- Comprehensive test suite: correctness, double-free, slot reuse, bucket growth, inverseOrder consistency, cursor retraction, auto-sort, concurrent alloc/free (race-detector clean)
- Benchmark suite across seven groups: Alloc, Free, Slot, Occupancy sweep (0–90% fill × sorted/unsorted), Sort cost, Parallel, Arena

### Notes

- `Slabber` and `Arena` are safe for concurrent use
- Bucket data (`[]byte` arenas) is never freed; the GC sees one object per bucket regardless of keycount
- Slot size is fixed per `Slabber`; use `Arena` for mixed-size workloads
