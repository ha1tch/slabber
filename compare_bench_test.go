package slabber_test

// Cross-variant benchmark suite.
//
// Compares the same workloads across all four implementations:
//
//   v0 — original: global Mutex, cursor reset to 0 on Free, manual Sort only
//   v1 — inverseOrder + auto-sort + Arena, still Mutex throughout
//   v2 — lock steering (lockMask), RWMutex, atomic freeCount, counting sort
//   v3 — atomic.Pointer bucket list (lock-free reads), growMu, clean lockMask
//
// All benchmarks use 64-byte slots for consistent comparison. The variants
// have identical public APIs; only the package paths differ.
//
// Benchmark groups:
//   _1B  — single bucket (degenerate case; all goroutines share one arena)
//   _NB  — N = runtime.NumCPU() pre-allocated buckets (intended use case)
//
// The _NB group is the primary comparison. The _1B group exists to isolate
// per-operation overhead from concurrency effects.
//
// Run with:
//   make bench-compare               — all groups
//   make bench-compare-par           — parallel groups, -cpu=1,2,4,8

import (
	"runtime"
	"testing"

	v0 "github.com/ha1tch/slabber/variants/v0"
	v1 "github.com/ha1tch/slabber/variants/v1"
	v2 "github.com/ha1tch/slabber/variants/v2"
	v3 "github.com/ha1tch/slabber"
)

const smallSlot = 64

// ---------------------------------------------------------------------------
// AllocFree — round-trip at low occupancy
// ---------------------------------------------------------------------------

func BenchmarkCompare_AllocFree(b *testing.B) {
	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot})
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
	})

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot})
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
	})

	b.Run("v2", func(b *testing.B) {
		s := v2.New(v2.Config{SlotSize: smallSlot})
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
	})

	b.Run("v3", func(b *testing.B) {
		s := v3.New(v3.Config{SlotSize: smallSlot})
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
	})
}

// ---------------------------------------------------------------------------
// SlotRead — read-only lookup
// ---------------------------------------------------------------------------

func BenchmarkCompare_SlotRead(b *testing.B) {
	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot})
		ref, _, _ := s.Alloc()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = s.Slot(ref)
		}
		b.StopTimer()
		s.Free(ref)
	})

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot})
		ref, _, _ := s.Alloc()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = s.Slot(ref)
		}
		b.StopTimer()
		s.Free(ref)
	})

	b.Run("v2", func(b *testing.B) {
		s := v2.New(v2.Config{SlotSize: smallSlot})
		ref, _, _ := s.Alloc()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = s.Slot(ref)
		}
		b.StopTimer()
		s.Free(ref)
	})

	b.Run("v3", func(b *testing.B) {
		s := v3.New(v3.Config{SlotSize: smallSlot})
		ref, _, _ := s.Alloc()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			_, _ = s.Slot(ref)
		}
		b.StopTimer()
		s.Free(ref)
	})
}

// ---------------------------------------------------------------------------
// HighOccupancy — alloc+free at 90% fill, unsorted
// ---------------------------------------------------------------------------

func BenchmarkCompare_HighOccupancy(b *testing.B) {
	fill := v0.SlotsPerBucket * 9 / 10

	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot})
		held := make([]v0.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
		b.StopTimer()
		for _, r := range held {
			s.Free(r)
		}
	})

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot})
		held := make([]v1.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
		b.StopTimer()
		for _, r := range held {
			s.Free(r)
		}
	})

	b.Run("v2", func(b *testing.B) {
		s := v2.New(v2.Config{SlotSize: smallSlot})
		held := make([]v2.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
		b.StopTimer()
		for _, r := range held {
			s.Free(r)
		}
	})

	b.Run("v3", func(b *testing.B) {
		s := v3.New(v3.Config{SlotSize: smallSlot})
		held := make([]v3.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ref, _, ok := s.Alloc()
			if !ok {
				b.Fatal("Alloc failed")
			}
			s.Free(ref)
		}
		b.StopTimer()
		for _, r := range held {
			s.Free(r)
		}
	})
}

// ---------------------------------------------------------------------------
// HighOccupancyAfterSort — 90% fill, scattered frees, then Sort()
// ---------------------------------------------------------------------------

func BenchmarkCompare_HighOccupancyAfterSort(b *testing.B) {
	fill := v0.SlotsPerBucket * 9 / 10

	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot})
		held := make([]v0.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		for i := 0; i < fill; i += 10 {
			s.Free(held[i])
			held[i] = v0.Ref{}
		}
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
		for _, r := range held {
			if r != (v0.Ref{}) {
				s.Free(r)
			}
		}
	})

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot})
		held := make([]v1.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		for i := 0; i < fill; i += 10 {
			s.Free(held[i])
			held[i] = v1.Ref{}
		}
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
		for _, r := range held {
			if r != (v1.Ref{}) {
				s.Free(r)
			}
		}
	})

	b.Run("v2", func(b *testing.B) {
		s := v2.New(v2.Config{SlotSize: smallSlot})
		held := make([]v2.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		for i := 0; i < fill; i += 10 {
			s.Free(held[i])
			held[i] = v2.Ref{}
		}
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
		for _, r := range held {
			if r != (v2.Ref{}) {
				s.Free(r)
			}
		}
	})

	b.Run("v3", func(b *testing.B) {
		s := v3.New(v3.Config{SlotSize: smallSlot})
		held := make([]v3.Ref, fill)
		for i := range held {
			held[i], _, _ = s.Alloc()
		}
		for i := 0; i < fill; i += 10 {
			s.Free(held[i])
			held[i] = v3.Ref{}
		}
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
		for _, r := range held {
			if r != (v3.Ref{}) {
				s.Free(r)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Parallel AllocFree — the key contention benchmark
// Run with -cpu=1,2,4,8 via make bench-compare-par
// ---------------------------------------------------------------------------

func BenchmarkCompare_Parallel_AllocFree_1B(b *testing.B) {
	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot})
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

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot})
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

	b.Run("v2", func(b *testing.B) {
		s := v2.New(v2.Config{SlotSize: smallSlot})
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

	b.Run("v3", func(b *testing.B) {
		s := v3.New(v3.Config{SlotSize: smallSlot})
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
}

// ---------------------------------------------------------------------------
// Parallel SlotRead
// ---------------------------------------------------------------------------

func BenchmarkCompare_Parallel_SlotRead_1B(b *testing.B) {
	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot})
		ref, _, _ := s.Alloc()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = s.Slot(ref)
			}
		})
		b.StopTimer()
		s.Free(ref)
	})

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot})
		ref, _, _ := s.Alloc()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = s.Slot(ref)
			}
		})
		b.StopTimer()
		s.Free(ref)
	})

	b.Run("v2", func(b *testing.B) {
		s := v2.New(v2.Config{SlotSize: smallSlot})
		ref, _, _ := s.Alloc()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				_, _ = s.Slot(ref)
			}
		})
		b.StopTimer()
		s.Free(ref)
	})

	b.Run("v3", func(b *testing.B) {
		s := v3.New(v3.Config{SlotSize: smallSlot})
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
// Parallel AllocFree — N buckets (primary benchmark)
// N = runtime.NumCPU(); each goroutine has its own arena to land in.
// ---------------------------------------------------------------------------

func BenchmarkCompare_Parallel_AllocFree_NB(b *testing.B) {
	n := runtime.NumCPU()

	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot, Buckets: n})
		b.ResetTimer()
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

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot, Buckets: n})
		b.ResetTimer()
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

	b.Run("v2", func(b *testing.B) {
		s := v2.NewSlabber(smallSlot, n)
		b.ResetTimer()
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

	b.Run("v3", func(b *testing.B) {
		s := v3.NewSlabber(smallSlot, n)
		b.ResetTimer()
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
}

// ---------------------------------------------------------------------------
// Parallel SlotRead — N buckets (primary benchmark)
// ---------------------------------------------------------------------------

func BenchmarkCompare_Parallel_SlotRead_NB(b *testing.B) {
	n := runtime.NumCPU()

	b.Run("v0", func(b *testing.B) {
		s := v0.New(v0.Config{SlotSize: smallSlot, Buckets: n})
		refs := make([]v0.Ref, n)
		for i := range refs {
			refs[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				_, _ = s.Slot(refs[i%n])
				i++
			}
		})
		b.StopTimer()
		for _, r := range refs {
			s.Free(r)
		}
	})

	b.Run("v1", func(b *testing.B) {
		s := v1.New(v1.Config{SlotSize: smallSlot, Buckets: n})
		refs := make([]v1.Ref, n)
		for i := range refs {
			refs[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				_, _ = s.Slot(refs[i%n])
				i++
			}
		})
		b.StopTimer()
		for _, r := range refs {
			s.Free(r)
		}
	})

	b.Run("v2", func(b *testing.B) {
		s := v2.NewSlabber(smallSlot, n)
		refs := make([]v2.Ref, n)
		for i := range refs {
			refs[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				_, _ = s.Slot(refs[i%n])
				i++
			}
		})
		b.StopTimer()
		for _, r := range refs {
			s.Free(r)
		}
	})

	b.Run("v3", func(b *testing.B) {
		s := v3.NewSlabber(smallSlot, n)
		refs := make([]v3.Ref, n)
		for i := range refs {
			refs[i], _, _ = s.Alloc()
		}
		b.ResetTimer()
		b.RunParallel(func(pb *testing.PB) {
			i := 0
			for pb.Next() {
				_, _ = s.Slot(refs[i%n])
				i++
			}
		})
		b.StopTimer()
		for _, r := range refs {
			s.Free(r)
		}
	})
}
