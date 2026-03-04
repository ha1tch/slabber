// Package v0 is the original slabber implementation (prototype).
//
// Characteristics:
//   - sync.Mutex on both Slabber and each bucket
//   - No inverseOrder: Free() resets cursor to 0 unconditionally
//   - No auto-sort: Sort() must be called manually
//   - No SortThreshold config field
//   - sort.Slice for bitmap word reordering
//   - No Arena
//
// Preserved for benchmarking comparison against later variants.
// Do not use in production code; import github.com/ha1tch/slabber instead.
package v0
