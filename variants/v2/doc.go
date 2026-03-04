// Package v2 is the third slabber implementation.
//
// Improvements over v1:
//   - sync.RWMutex on Slabber: Alloc scan, Free, Slot take RLock briefly to
//     read the bucket list pointer; only grow() takes a write lock.
//   - lockMask (atomic.Uint64): intent signpost advertises which buckets are
//     contested. Bit set BEFORE b.mu.Lock() so arriving goroutines steer away
//     during the contention window, not after it resolves.
//   - Fast path re-reads lockMask on every iteration (tried accumulator
//     prevents re-visiting the same bucket).
//   - freeCount (atomic.Int32) per bucket: skip full buckets without any lock.
//   - Counting sort in doSort(): O(n) replaces O(n log n) sort.Slice.
//   - runtime.Gosched() in Sort() spin-wait: prevents livelock.
//   - Config.Buckets: pre-allocate N arenas at construction.
//   - NewSlabber(slotSize, buckets): ergonomic constructor.
//
// Limitation: s.mu (RWMutex) is still taken on every Slot() call to read
// the bucket list pointer, adding reader-count cache-line traffic at high
// core counts.
//
// Preserved for benchmarking comparison. Do not use in production code;
// import github.com/ha1tch/slabber instead.
package v2
