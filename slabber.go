// Package slabber implements a fixed-size slot allocator over large byte
// arenas, using bitmap-tracked free lists and popcount-sorted scan order
// to minimise GC pressure and allocation overhead.
//
// Design:
//   - Each bucket holds SlotsPerBucket (65536) slots of fixed SlotSize bytes.
//   - A [BitmapWords]uint64 bitmap tracks occupancy: 1 = occupied, 0 = free.
//   - A [BitmapWords]uint16 order array controls scan sequence; sorted
//     ascending by popcount so sparsely-occupied words are scanned first.
//   - A [BitmapWords]uint16 inverseOrder maps physical word -> logical position
//     in order[], enabling O(1) cursor retraction on Free().
//   - A cursor within order[] provides a fast-path hint; doSort() resets it.
//   - When cursor advances past SortThreshold, a background sort is scheduled.
//   - New buckets are appended on demand; bucket data is never freed.
//
// Concurrency model (v3):
//   - buckets is an atomic.Pointer to an immutable []*bucket snapshot.
//     Slot(), Free(), and the Alloc scan load this pointer with a single
//     atomic read — no mutex at all on the read path.
//   - growMu serialises grow() only. It takes a fresh snapshot, appends a new
//     bucket, and atomically swaps the pointer. Readers that loaded the old
//     snapshot continue safely; they will find no free slot and retry, at
//     which point they load the new snapshot.
//   - lockMask (atomic.Uint64) is the intent signpost: bit i is set BEFORE
//     b.mu is contested (Lock or TryLock), so arriving goroutines can steer
//     away during the contention window. Covers buckets 0-63.
//   - freeCount (atomic.Int32) per bucket is readable without b.mu as a
//     cheap hint to skip full buckets before touching any lock.
//   - doSort() uses a counting sort (O(n)) on popcount values [0,64].
//
// The GC sees one []byte per bucket regardless of keycount.
package slabber

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
)

const (
	BitmapWords          = 1024
	SlotsPerBucket       = BitmapWords * 64
	DefaultSlotSize      = 4096
	DefaultSortThreshold = 256
	lockMaskCap          = 64
)

// Config controls memory layout and concurrency tuning.
type Config struct {
	// SlotSize is the number of bytes per slot. All slots in a Slabber
	// are the same size. Use Arena for mixed-size workloads.
	SlotSize int

	// SortThreshold is the cursor position (in bitmap words) at which a
	// background sort is triggered. 0 uses DefaultSortThreshold.
	SortThreshold int

	// Buckets is the number of arenas to pre-allocate at construction time.
	// Set to runtime.NumCPU() so goroutines spread across arenas immediately
	// without racing to grow. 0 defaults to 1.
	Buckets int
}

func DefaultConfig() Config {
	return Config{
		SlotSize:      DefaultSlotSize,
		SortThreshold: DefaultSortThreshold,
		Buckets:       1,
	}
}

func (c Config) sortThreshold() int {
	if c.SortThreshold <= 0 {
		return DefaultSortThreshold
	}
	return c.SortThreshold
}

func (c Config) initialBuckets() int {
	if c.Buckets <= 0 {
		return 1
	}
	return c.Buckets
}

// Ref is an opaque slot reference. Do not use after Free().
type Ref struct {
	Bucket uint32
	Slot   uint32
}

// bucket is a single arena: one large []byte, one bitmap, one order array.
type bucket struct {
	mu           sync.Mutex
	bitmap       [BitmapWords]uint64
	order        [BitmapWords]uint16
	inverseOrder [BitmapWords]uint16
	data         []byte
	// freeCount is authoritative under mu; readable atomically as a hint.
	freeCount   atomic.Int32
	cursor      int
	sortPending atomic.Bool
}

func newBucket(slotSize int) *bucket {
	b := &bucket{
		data: make([]byte, SlotsPerBucket*slotSize),
	}
	b.freeCount.Store(int32(SlotsPerBucket))
	for i := range b.order {
		b.order[i] = uint16(i)
		b.inverseOrder[i] = uint16(i)
	}
	return b
}

func (b *bucket) findFreeSlot(threshold int) (slot uint32, ok bool, needsSort bool) {
	for i := b.cursor; i < BitmapWords; i++ {
		wi := b.order[i]
		w := b.bitmap[wi]
		if w == ^uint64(0) {
			b.cursor = i + 1
			continue
		}
		b.cursor = i
		bit := bits.TrailingZeros64(^w)
		needsSort = b.cursor >= threshold && !b.sortPending.Load()
		return uint32(wi)*64 + uint32(bit), true, needsSort
	}
	return 0, false, false
}

// doSort uses a counting sort on popcount values [0,64] — O(n), no allocs.
// Must be called with b.mu held.
func (b *bucket) doSort() {
	var count [65]int
	for _, w := range b.bitmap {
		count[bits.OnesCount64(w)]++
	}
	var pos [65]int
	for i := 1; i <= 64; i++ {
		pos[i] = pos[i-1] + count[i-1]
	}
	var sorted [BitmapWords]uint16
	for wi := 0; wi < BitmapWords; wi++ {
		pc := bits.OnesCount64(b.bitmap[wi])
		sorted[pos[pc]] = uint16(wi)
		pos[pc]++
	}
	copy(b.order[:], sorted[:])
	for logical, physical := range b.order {
		b.inverseOrder[physical] = uint16(logical)
	}
	b.cursor = 0
}

func (b *bucket) sortAsync() {
	if !b.sortPending.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer b.sortPending.Store(false)
		b.mu.Lock()
		b.doSort()
		b.mu.Unlock()
	}()
}

// Slabber manages fixed-size slot allocation over one or more byte arenas.
// Safe for concurrent use.
type Slabber struct {
	cfg Config
	// buckets holds an immutable snapshot of the bucket list.
	// Readers load it with a single atomic read; no mutex required.
	// grow() builds a new slice and swaps the pointer atomically.
	buckets  atomic.Pointer[[]*bucket]
	growMu   sync.Mutex   // serialises grow() only
	lockMask atomic.Uint64 // intent signpost: bit i set before contesting bucket i
}

// New returns a Slabber configured by cfg.
// Pre-allocates cfg.Buckets arenas (default 1); set cfg.Buckets to
// runtime.NumCPU() so goroutines spread across arenas from the first alloc.
func New(cfg Config) *Slabber {
	n := cfg.initialBuckets()
	s := &Slabber{cfg: cfg}
	bs := make([]*bucket, n)
	for i := range bs {
		bs[i] = newBucket(cfg.SlotSize)
	}
	s.buckets.Store(&bs)
	return s
}

// NewSlabber is a convenience constructor for the common case.
// slotSize is the byte size of each slot; buckets is the number of arenas
// to pre-allocate — pass runtime.NumCPU() for best concurrent performance.
func NewSlabber(slotSize, buckets int) *Slabber {
	return New(Config{
		SlotSize:      slotSize,
		SortThreshold: DefaultSortThreshold,
		Buckets:       buckets,
	})
}

// grow appends a new bucket under growMu. Returns the new bucket and its
// index. Callers that race to grow will each take growMu in turn; the second
// one re-checks whether the first already grew and, if so, returns early.
// Returns (nil, 0, false) if the new bucket was already added by a racing
// goroutine — caller should retry the scan.
func (s *Slabber) grow() (b *bucket, idx uint32, appended bool) {
	s.growMu.Lock()
	defer s.growMu.Unlock()

	old := *s.buckets.Load()
	// Re-check: a concurrent grow() may have already added a bucket with
	// free slots. If so, let the caller retry rather than appending again.
	for _, bkt := range old {
		if bkt.freeCount.Load() > 0 {
			return nil, 0, false
		}
	}
	newSlice := make([]*bucket, len(old)+1)
	copy(newSlice, old)
	b = newBucket(s.cfg.SlotSize)
	newSlice[len(old)] = b
	s.buckets.Store(&newSlice)
	return b, uint32(len(old)), true
}

func maskBitFor(i int) (bit uint64, ok bool) {
	if i >= lockMaskCap {
		return 0, false
	}
	return uint64(1) << i, true
}

// Alloc reserves a slot and returns its Ref and backing []byte.
//
// Fast path: reads lockMask and freeCount without holding any lock to steer
// TryLock attempts toward buckets that are uncontested and have free slots.
// Intent is signalled (bit set) before TryLock so other goroutines see the
// contention during the window it exists, not after it resolves.
//
// Slow path: blocking scan in order, used when all hinted buckets are busy
// or the slabber has more than lockMaskCap buckets.
//
// Grow path: all buckets full; appends a new arena under growMu.
func (s *Slabber) Alloc() (Ref, []byte, bool) {
	threshold := s.cfg.sortThreshold()

	for {
		buckets := *s.buckets.Load()
		n := len(buckets)

		// --- Fast path: lock steering ---
		var validMask uint64
		if n >= lockMaskCap {
			validMask = ^uint64(0)
		} else if n > 0 {
			validMask = (uint64(1) << n) - 1
		}
		var tried uint64
		for {
			hints := ^(s.lockMask.Load() | tried) & validMask
			if hints == 0 {
				break
			}
			i := bits.TrailingZeros64(hints)
			tried |= uint64(1) << i
			b := buckets[i]
			if b.freeCount.Load() == 0 {
				continue
			}
			bit, _ := maskBitFor(i)
			s.lockMask.Or(bit) // signal intent before TryLock
			if !b.mu.TryLock() {
				s.lockMask.And(^bit) // failed — withdraw signal
				continue
			}
			slot, ok, needsSort := b.findFreeSlot(threshold)
			if ok {
				b.bitmap[slot/64] |= 1 << (slot % 64)
				b.freeCount.Add(-1)
				data := s.slotBytes(b, slot)
				s.lockMask.And(^bit)
				b.mu.Unlock()
				if needsSort {
					b.sortAsync()
				}
				return Ref{Bucket: uint32(i), Slot: slot}, data, true
			}
			s.lockMask.And(^bit)
			b.mu.Unlock()
		}

		// --- Slow path: blocking scan ---
		for i, b := range buckets {
			if b.freeCount.Load() == 0 {
				continue
			}
			bit, hasBit := maskBitFor(i)
			if hasBit {
				s.lockMask.Or(bit) // signal intent before blocking
			}
			b.mu.Lock()
			slot, ok, needsSort := b.findFreeSlot(threshold)
			if ok {
				b.bitmap[slot/64] |= 1 << (slot % 64)
				b.freeCount.Add(-1)
				data := s.slotBytes(b, slot)
				if hasBit {
					s.lockMask.And(^bit)
				}
				b.mu.Unlock()
				if needsSort {
					b.sortAsync()
				}
				return Ref{Bucket: uint32(i), Slot: slot}, data, true
			}
			if hasBit {
				s.lockMask.And(^bit)
			}
			b.mu.Unlock()
		}

		// --- Grow ---
		b, idx, appended := s.grow()
		if !appended {
			// A concurrent goroutine grew; retry with fresh snapshot.
			continue
		}
		// New bucket: slot 0 is always free — skip findFreeSlot.
		bit, hasBit := maskBitFor(int(idx))
		if hasBit {
			s.lockMask.Or(bit)
		}
		b.mu.Lock()
		b.bitmap[0] |= 1
		b.freeCount.Add(-1)
		data := s.slotBytes(b, 0)
		if hasBit {
			s.lockMask.And(^bit)
		}
		b.mu.Unlock()
		return Ref{Bucket: idx, Slot: 0}, data, true
	}
}

// Free releases a slot. Returns false for invalid ref or double-free.
func (s *Slabber) Free(ref Ref) bool {
	buckets := *s.buckets.Load()
	if int(ref.Bucket) >= len(buckets) {
		return false
	}
	b := buckets[ref.Bucket]

	wordIdx := ref.Slot / 64
	mask := uint64(1) << (ref.Slot % 64)
	bit, hasBit := maskBitFor(int(ref.Bucket))
	if hasBit {
		s.lockMask.Or(bit) // signal intent before blocking
	}
	b.mu.Lock()
	if b.bitmap[wordIdx]&mask == 0 {
		if hasBit {
			s.lockMask.And(^bit)
		}
		b.mu.Unlock()
		return false // double-free
	}
	b.bitmap[wordIdx] &^= mask
	b.freeCount.Add(1)
	if lp := int(b.inverseOrder[wordIdx]); lp < b.cursor {
		b.cursor = lp
	}
	if hasBit {
		s.lockMask.And(^bit)
	}
	b.mu.Unlock()
	return true
}

// Slot returns the backing []byte for an allocated Ref.
// Lock-free: one atomic pointer load, no mutex.
// Do not retain the slice after Free(ref).
func (s *Slabber) Slot(ref Ref) ([]byte, bool) {
	buckets := *s.buckets.Load()
	if int(ref.Bucket) >= len(buckets) {
		return nil, false
	}
	return s.slotBytes(buckets[ref.Bucket], ref.Slot), true
}

func (s *Slabber) slotBytes(b *bucket, slot uint32) []byte {
	start := int(slot) * s.cfg.SlotSize
	return b.data[start : start+s.cfg.SlotSize]
}

// Sort synchronously reorders all buckets by popcount.
func (s *Slabber) Sort() {
	buckets := *s.buckets.Load()
	for _, b := range buckets {
		for b.sortPending.Load() {
			runtime.Gosched()
		}
		b.mu.Lock()
		b.doSort()
		b.mu.Unlock()
	}
}

// Stats holds a point-in-time snapshot of allocator state.
type Stats struct {
	Buckets    int
	TotalSlots int
	UsedSlots  int
	FreeSlots  int
	SlotSize   int
	MemoryMB   float64
}

// Stats reads freeCount atomically; no locks required.
func (s *Slabber) Stats() Stats {
	buckets := *s.buckets.Load()
	cfg := s.cfg
	var free int
	for _, b := range buckets {
		free += int(b.freeCount.Load())
	}
	total := len(buckets) * SlotsPerBucket
	return Stats{
		Buckets:    len(buckets),
		TotalSlots: total,
		UsedSlots:  total - free,
		FreeSlots:  free,
		SlotSize:   cfg.SlotSize,
		MemoryMB:   float64(len(buckets)*SlotsPerBucket*cfg.SlotSize) / (1024 * 1024),
	}
}

// ---------------------------------------------------------------------------
// Arena — multi-size-class allocator
// ---------------------------------------------------------------------------

// SizeClass defines one allocation tier within an Arena.
type SizeClass struct {
	MaxSize       int
	SlotSize      int
	SortThreshold int
}

// ArenaRef extends Ref with the size class index.
type ArenaRef struct {
	Ref
	Class uint8
}

// Arena routes allocations to the appropriate size-class Slabber.
type Arena struct {
	classes  []SizeClass
	slabbers []*Slabber
}

// NewArena constructs an Arena. Classes must be in ascending MaxSize order.
func NewArena(classes []SizeClass) *Arena {
	a := &Arena{classes: classes}
	for _, sc := range classes {
		slotSize := sc.SlotSize
		if slotSize == 0 {
			slotSize = nextPow2(sc.MaxSize)
		}
		a.slabbers = append(a.slabbers, New(Config{
			SlotSize:      slotSize,
			SortThreshold: sc.SortThreshold,
		}))
	}
	return a
}

// DefaultArena returns an Arena with four size classes:
//
//	Class 0: <=64B, Class 1: <=512B, Class 2: <=4KB, Class 3: <=64KB
func DefaultArena() *Arena {
	return NewArena([]SizeClass{
		{MaxSize: 64},
		{MaxSize: 512},
		{MaxSize: 4096},
		{MaxSize: 65536},
	})
}

func (a *Arena) Alloc(size int) (ArenaRef, []byte, bool) {
	for i, sc := range a.classes {
		if size <= sc.MaxSize {
			ref, data, ok := a.slabbers[i].Alloc()
			if !ok {
				return ArenaRef{}, nil, false
			}
			return ArenaRef{Ref: ref, Class: uint8(i)}, data, true
		}
	}
	return ArenaRef{}, nil, false
}

func (a *Arena) Free(ref ArenaRef) bool {
	if int(ref.Class) >= len(a.slabbers) {
		return false
	}
	return a.slabbers[ref.Class].Free(ref.Ref)
}

func (a *Arena) Slot(ref ArenaRef) ([]byte, bool) {
	if int(ref.Class) >= len(a.slabbers) {
		return nil, false
	}
	return a.slabbers[ref.Class].Slot(ref.Ref)
}

func (a *Arena) Stats() []Stats {
	out := make([]Stats, len(a.slabbers))
	for i, s := range a.slabbers {
		out[i] = s.Stats()
	}
	return out
}

func nextPow2(v int) int {
	if v <= 1 {
		return 1
	}
	v--
	v |= v >> 1
	v |= v >> 2
	v |= v >> 4
	v |= v >> 8
	v |= v >> 16
	v |= v >> 32
	return v + 1
}
