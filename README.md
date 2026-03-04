# slabber

A fixed-size slot allocator for Go, built for cache-heavy workloads where `Slot()` dominates.

[![Go Reference](https://pkg.go.dev/badge/github.com/ha1tch/slabber.svg)](https://pkg.go.dev/github.com/ha1tch/slabber)
[![Go 1.23+](https://img.shields.io/badge/go-1.23+-blue.svg)](https://golang.org/dl/)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

## What it does

slabber manages a pool of fixed-size byte slices across one or more arenas (buckets). Each bucket holds 65,536 slots tracked by a bitmap. Slots are allocated, read, and freed by opaque `Ref` values.

The key design choice is the read path. `Slot()` — the call that retrieves the backing `[]byte` for an allocated ref — holds no lock. It performs a single atomic pointer load and returns. Under eight concurrent readers on an Apple M1 this costs **0.37 ns/op**.

For comparison, a `sync.RWMutex`-protected equivalent costs around 98 ns/op at the same concurrency — roughly 265× slower.

## When to use it

slabber is the right tool when:

- All allocations are the same size (or you want separate slabbers per size class via `Arena`)
- Reads (`Slot()`) are far more frequent than writes (`Alloc()`, `Free()`)
- You want predictable, GC-invisible memory — one `[]byte` per bucket, forever

It is not a general-purpose allocator. If you need variable-size allocations with arbitrary lifetimes, use `sync.Pool` or a general allocator.

## Install

```
go get github.com/ha1tch/slabber
```

Requires Go 1.23 or later.

## Quick start

```go
import (
    "runtime"
    "github.com/ha1tch/slabber"
)

// One arena per CPU — goroutines spread across buckets immediately.
s := slabber.NewSlabber(4096, runtime.NumCPU())

// Allocate a slot.
ref, data, ok := s.Alloc()
if !ok {
    // slabber never returns false from Alloc — it grows automatically.
    // ok is false only if the internal bitmap is exhausted, which cannot
    // happen with automatic growth enabled.
}

// Write into the slot.
copy(data, myBytes)

// Retrieve the slot later — lock-free.
buf, ok := s.Slot(ref)

// Release the slot.
s.Free(ref)
```

## Configuration

```go
s := slabber.New(slabber.Config{
    SlotSize:      4096,           // bytes per slot; all slots are this size
    Buckets:       runtime.NumCPU(), // arenas to pre-allocate
    SortThreshold: 256,            // bitmap words before background re-sort (0 = default)
})
```

`Config.Buckets` is the most important tuning parameter. Set it to `runtime.NumCPU()` so goroutines can land in separate arenas from the first allocation without racing to grow.

## Multi-size workloads: Arena

`Arena` routes allocations to the right size class automatically:

```go
a := slabber.DefaultArena()
// Four classes: <=64B, <=512B, <=4KB, <=64KB

ref, data, ok := a.Alloc(200) // lands in the 512B class
a.Free(ref)
```

Custom classes:

```go
a := slabber.NewArena([]slabber.SizeClass{
    {MaxSize: 128},
    {MaxSize: 1024},
    {MaxSize: 8192},
})
```

## Benchmark results (Apple M1, Go 1.24)

All results use `runtime.NumCPU()` (8) pre-warmed buckets.

### AllocFree — round-trip latency

| goroutines | v0 (Mutex) | v1 (Mutex+sort) | v3 (slabber) |
|-----------|-----------|----------------|-------------|
| 1 | 43 ns | 44 ns | **35 ns** |
| 2 | 76 ns | 70 ns | 93 ns |
| 4 | 165 ns | 169 ns | **138 ns** |
| 8 | 204 ns | 210 ns | **136 ns** |

At 4+ goroutines slabber outperforms the mutex-based implementations. The 4→8 transition is flat (138→136 ns) — the allocator is scaling, not degrading.

### Slot — read latency

| goroutines | sync.Mutex | sync.RWMutex | slabber |
|-----------|-----------|-------------|--------|
| 1 | 13.8 ns | 14.1 ns | **1.4 ns** |
| 2 | 28 ns | 41 ns | **0.75 ns** |
| 4 | 65 ns | 48 ns | **0.37 ns** |
| 8 | 79 ns | 98 ns | **0.37 ns** |

`Slot()` reaches the instruction throughput ceiling of the CPU at 4+ goroutines. The allocator has disappeared from the read path.

To reproduce:

```
make bench-compare-nb
```

## Concurrency model

- **`buckets`** is an `atomic.Pointer[[]*bucket]`. Readers load it with a single atomic read — no mutex on the read path.
- **`growMu`** serialises bucket growth only. A grow builds a new slice, appends the bucket, and swaps the pointer atomically. Concurrent readers on the old snapshot continue safely.
- **`lockMask`** (`atomic.Uint64`) is an intent signpost: bit `i` is set before contesting bucket `i`'s mutex, so arriving goroutines steer toward uncontested buckets.
- **`freeCount`** (`atomic.Int32`) per bucket is updated under the bucket mutex but readable without it as a cheap hint to skip full buckets.
- **`b.mu`** (`sync.Mutex`) per bucket serialises bitmap mutations.

## Safety

The race detector passes on all concurrent tests including:

- 32 goroutines racing to trigger bucket growth simultaneously
- `runtime.NumCPU()` concurrent readers and writers on pre-warmed buckets
- Continuous `Slot()` calls on a live ref while other goroutines force repeated growth

```
make test-race
```

## Development variants

`variants/` contains frozen prior implementations for benchmarking comparison:

| Package | Description |
|---------|-------------|
| `variants/v0` | Original prototype — global `sync.Mutex`, manual sort |
| `variants/v1` | `inverseOrder` cursor retraction, auto background sort |
| `variants/v2` | `sync.RWMutex` + `lockMask` intent signpost |

The root package is v3: `atomic.Pointer` bucket list, `growMu`, clean lockMask.

## Make targets

```
make               — vet + test
make test          — run all tests
make test-race     — run all tests with race detector
make bench-compare-nb  — primary benchmark: N buckets, -cpu=1,2,4,8
make bench         — full benchmark suite
make vet           — go vet
make fmt           — gofmt
make release-check — verify VERSION, version.go, and CHANGELOG are consistent
make help          — list all targets
```

## License

Copyright (c) 2026 haitch  
Apache License 2.0 — see [LICENSE](LICENSE) for details.  
https://www.apache.org/licenses/LICENSE-2.0
