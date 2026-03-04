package slabber

// Benchmark suite for slabber.
//
// Groups:
//
//   Alloc       — allocation throughput, accumulating and round-trip
//   Free        — release cost in isolation
//   Slot        — read-only slot lookup cost
//   Occupancy   — alloc+free round-trip across the occupancy curve (0–90%)
//                 run this pair with and without Sort() to see cursor degradation
//   Sort        — explicit sort cost at various fill levels
//   Parallel    — concurrent alloc+free (vary GOMAXPROCS with -cpu flag)
//   Arena       — multi-size-class routing, round-trip, parallel
//
// Recommended invocations (see Makefile targets):
//
//   make bench               — all benchmarks, 3 runs each
//   make bench-alloc         — Alloc/* group
//   make bench-occupancy     — Occupancy/* group
//   make bench-sort          — Sort/* group
//   make bench-parallel      — Parallel/* group, -cpu=1,2,4,8
//   make bench-arena         — Arena/* group

import (
	"fmt"
	"testing"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// fillSlabber pre-allocates frac of a single bucket's slots and returns the
// held refs. frac must be in [0,1). The slabber has small (64-byte) slots.
func fillSlabber(frac float64) (*Slabber, []Ref) {
	s := New(smallConfig())
	n := int(float64(SlotsPerBucket) * frac)
	held := make([]Ref, n)
	for i := range held {
		ref, _, ok := s.Alloc()
		if !ok {
			panic("fillSlabber: Alloc failed")
		}
		held[i] = ref
	}
	return s, held
}

// fillAndScatter pre-allocates frac of slots then frees every stride-th one,
// producing scattered free pockets — a realistic steady-state pattern.
func fillAndScatter(frac float64, stride int) (*Slabber, []Ref) {
	s, held := fillSlabber(frac)
	for i := 0; i < len(held); i += stride {
		s.Free(held[i])
		held[i] = Ref{}
	}
	return s, held
}

// drainHeld releases all non-zero refs in held.
func drainHeld(s *Slabber, held []Ref) {
	for _, r := range held {
		if r != (Ref{}) {
			s.Free(r)
		}
	}
}

// ---------------------------------------------------------------------------
// Alloc group
// ---------------------------------------------------------------------------

// BenchmarkAlloc/Accumulate measures raw allocation throughput including
// bucket growth. Each iteration allocates and never frees, so the slabber
// grows across bucket boundaries. This exercises the grow() path.
func BenchmarkAlloc(b *testing.B) {
	b.Run("Accumulate", func(b *testing.B) {
		s := New(smallConfig())
		refs := make([]Ref, 0, b.N)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			refs = append(refs, ref)
		}
		b.StopTimer()
		for _, r := range refs {
			s.Free(r)
		}
	})

	// BenchmarkAlloc/RoundTrip: steady-state alloc+free on an empty bucket.
	// The hot path: bitmap scan from cursor 0, single word, set bit, return.
	b.Run("RoundTrip", func(b *testing.B) {
		s := New(smallConfig())
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
	})

	// BenchmarkAlloc/RoundTripAfterGrow: same but slabber has two buckets,
	// so Alloc() must scan bucket 0 (full) before finding space in bucket 1.
	b.Run("RoundTripAfterGrow", func(b *testing.B) {
		s, held := fillSlabber(1.0 - 1.0/float64(SlotsPerBucket))
		// Force bucket 1 into existence.
		extra, _, _ := s.Alloc()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
		b.StopTimer()
		s.Free(extra)
		drainHeld(s, held)
	})
}

// ---------------------------------------------------------------------------
// Free group
// ---------------------------------------------------------------------------

// BenchmarkFree measures the cost of releasing a slot in isolation.
// We pre-allocate a pool and cycle through it to avoid re-using the same
// slot (which would be in L1 cache and give an unrealistically fast result).
func BenchmarkFree(b *testing.B) {
	b.Run("Sequential", func(b *testing.B) {
		s := New(smallConfig())
		pool := make([]Ref, SlotsPerBucket/2)
		for i := range pool {
			pool[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			idx := i % len(pool)
			if pool[idx] == (Ref{}) {
				pool[idx], _, _ = s.Alloc()
			}
			s.Free(pool[idx])
			pool[idx] = Ref{}
		}
	})
}

// ---------------------------------------------------------------------------
// Slot (read) group
// ---------------------------------------------------------------------------

// BenchmarkSlot measures the cost of looking up the backing []byte for an
// already-allocated ref — the read hot path for a cache.
func BenchmarkSlot(b *testing.B) {
	s := New(smallConfig())
	ref, _, _ := s.Alloc()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Slot(ref)
	}
	b.StopTimer()
	s.Free(ref)
}

// ---------------------------------------------------------------------------
// Occupancy sweep
// ---------------------------------------------------------------------------

// BenchmarkOccupancy measures alloc+free round-trip at increasing fill levels.
// Each sub-benchmark pre-fills the named fraction of the bucket then runs
// alloc+free in steady state. Compares unsorted vs sorted to show the value
// of Sort() at high occupancy.

func BenchmarkOccupancy(b *testing.B) {
	fracs := []struct {
		name string
		frac float64
	}{
		{"Fill00pct", 0.00},
		{"Fill25pct", 0.25},
		{"Fill50pct", 0.50},
		{"Fill75pct", 0.75},
		{"Fill90pct", 0.90},
	}

	for _, tc := range fracs {
		tc := tc
		b.Run(tc.name+"/Unsorted", func(b *testing.B) {
			// Scatter frees every 8 slots to create realistic fragmentation.
			s, held := fillAndScatter(tc.frac, 8)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ref, _, ok := s.Alloc()
				if !ok {
					b.Fatal("Alloc failed")
				}
				s.Free(ref)
			}
			b.StopTimer()
			drainHeld(s, held)
		})

		b.Run(tc.name+"/Sorted", func(b *testing.B) {
			s, held := fillAndScatter(tc.frac, 8)
			s.Sort()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ref, _, ok := s.Alloc()
				if !ok {
					b.Fatal("Alloc failed")
				}
				s.Free(ref)
			}
			b.StopTimer()
			drainHeld(s, held)
		})
	}
}

// ---------------------------------------------------------------------------
// Sort group
// ---------------------------------------------------------------------------

// BenchmarkSort measures the cost of an explicit Sort() call at varying
// fill levels. This informs how frequently Sort() can be called without
// materially impacting throughput.
func BenchmarkSort(b *testing.B) {
	fracs := []struct {
		name string
		frac float64
	}{
		{"Fill25pct", 0.25},
		{"Fill50pct", 0.50},
		{"Fill75pct", 0.75},
		{"Fill90pct", 0.90},
	}
	for _, tc := range fracs {
		tc := tc
		b.Run(tc.name, func(b *testing.B) {
			s, held := fillSlabber(tc.frac)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s.Sort()
			}
			b.StopTimer()
			drainHeld(s, held)
		})
	}
}

// ---------------------------------------------------------------------------
// Parallel group
// ---------------------------------------------------------------------------

// BenchmarkParallel runs concurrent alloc+free using b.RunParallel.
// Use -cpu=1,2,4,8 to observe scalability. The slabber global mutex is the
// expected bottleneck; this benchmark quantifies contention cost.
func BenchmarkParallel(b *testing.B) {
	b.Run("AllocFree", func(b *testing.B) {
		s := New(smallConfig())
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				ref, _, ok := s.Alloc()
				if !ok {
					b.Fatal("Alloc failed")
				}
				s.Free(ref)
			}
		})
	})

	// SlotRead: concurrent reads of a pre-allocated slot.
	// Slot() only holds s.mu briefly to look up the bucket pointer,
	// then releases it; this should scale better than AllocFree.
	b.Run("SlotRead", func(b *testing.B) {
		s := New(smallConfig())
		ref, _, _ := s.Alloc()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = s.Slot(ref)
			}
		})
		b.StopTimer()
		s.Free(ref)
	})
}

// ---------------------------------------------------------------------------
// Arena group
// ---------------------------------------------------------------------------

// BenchmarkArena measures Arena routing overhead across all four size classes.
func BenchmarkArena(b *testing.B) {
	// RoundTrip cycles through the four size classes in round-robin.
	b.Run("RoundTrip", func(b *testing.B) {
		a := DefaultArena()
		sizes := []int{32, 200, 1500, 8000}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := a.Alloc(sizes[i%len(sizes)])
			if !ok {
				b.Fatal("Arena.Alloc failed")
			}
			a.Free(ref)
		}
	})

	// PerClass benchmarks each size class in isolation to separate routing
	// cost from per-Slabber cost.
	classes := []struct {
		name string
		size int
	}{
		{"Small/64B", 64},
		{"Medium/512B", 512},
		{"Large/4KB", 4096},
		{"XLarge/64KB", 65536},
	}
	for _, tc := range classes {
		tc := tc
		b.Run(fmt.Sprintf("PerClass/%s", tc.name), func(b *testing.B) {
			a := DefaultArena()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ref, _, ok := a.Alloc(tc.size)
				if !ok {
					b.Fatal("Arena.Alloc failed")
				}
				a.Free(ref)
			}
		})
	}

	// Parallel: concurrent alloc+free through the Arena.
	b.Run("Parallel", func(b *testing.B) {
		a := DefaultArena()
		sizes := []int{32, 200, 1500, 8000}
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				ref, _, ok := a.Alloc(sizes[i%len(sizes)])
				if !ok {
					b.Fatal("Arena.Alloc failed")
				}
				a.Free(ref)
				i++
			}
		})
	})
}
