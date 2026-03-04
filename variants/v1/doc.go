// Package v1 is the second slabber implementation.
//
// Improvements over v0:
//   - inverseOrder array: Free() retracts cursor in O(1) instead of resetting to 0
//   - Auto-sort: background sort triggered when cursor crosses SortThreshold
//   - SortThreshold config field
//   - sort.Slice for bitmap word reordering (same as v0)
//   - sync.Mutex on both Slabber and each bucket (same as v0)
//   - Arena multi-size-class allocator
//
// Preserved for benchmarking comparison against later variants.
// Do not use in production code; import github.com/ha1tch/slabber instead.
package v1
