// Package slabber implements a fixed-size slot allocator over large byte
// arenas, using bitmap-tracked free lists and popcount-sorted scan order
// to minimise GC pressure and allocation overhead.
//
// Design:
//   - Each bucket holds SlotsPerBucket (65536) slots of fixed SlotSize bytes.
//   - A [BitmapWords]uint64 bitmap tracks occupancy: 1 = occupied, 0 = free.
//   - A [BitmapWords]uint16 order array controls scan sequence; sorted
//     ascending by popcount so sparsely-occupied words are scanned first.
//   - A [BitmapWords]uint16 inverseOrder maps physical word → logical position
//     in order[], enabling O(1) cursor retraction on Free().
//   - A cursor within order[] provides a fast-path hint; doSort() resets it.
//   - When cursor advances past SortThreshold, a background sort is scheduled.
//   - New buckets are appended on demand; bucket data is never freed.
//
// The GC sees one []byte per bucket regardless of keycount.
package v1

import (
	"math/bits"
	"sort"
	"sync"
	"sync/atomic"
)

const (
	// BitmapWords is the number of uint64 words per bitmap.
	// BitmapWords * 64 = SlotsPerBucket.
	BitmapWords = 1024

	// SlotsPerBucket is the number of addressable slots per bucket.
	SlotsPerBucket = BitmapWords * 64 // 65536

	// DefaultSlotSize produces 256MB buckets at 4KB per slot.
	DefaultSlotSize = 4096

	// DefaultSortThreshold triggers a background sort when the cursor
	// advances past this many bitmap words.
	DefaultSortThreshold = 256
)

// Config controls memory layout. A Slabber is typically instantiated
// once per size class.
type Config struct {
	// SlotSize is the number of bytes per slot. All slots in a Slabber
	// are the same size. Use multiple Slabbers for multiple size classes.
	SlotSize int

	// SortThreshold is the cursor position (in bitmap words) at which a
	// background sort is triggered. 0 uses DefaultSortThreshold.
	SortThreshold int

	// Buckets is the number of arenas to pre-allocate. 0 defaults to 1.
	Buckets int
}

// DefaultConfig returns a Config with 4KB slots and default sort threshold.
func DefaultConfig() Config {
	return Config{SlotSize: DefaultSlotSize, SortThreshold: DefaultSortThreshold}
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

// Ref is an opaque slot reference. It encodes bucket index and slot index.
// Keep refs alive only as long as the backing data is needed;
// after Free(ref) the slot may be reused.
type Ref struct {
	Bucket uint32
	Slot   uint32
}

// bucket is a single arena: one large []byte, one bitmap, one order array.
type bucket struct {
	mu           sync.Mutex
	bitmap       [BitmapWords]uint64
	order        [BitmapWords]uint16 // logical → physical word index
	inverseOrder [BitmapWords]uint16 // physical word → logical position
	data         []byte
	freeCount    int
	cursor       int // logical scan position; reset by doSort()
	sortPending  atomic.Bool
}

func newBucket(slotSize int) *bucket {
	b := &bucket{
		data:      make([]byte, SlotsPerBucket*slotSize),
		freeCount: SlotsPerBucket,
	}
	for i := range b.order {
		b.order[i] = uint16(i)
		b.inverseOrder[i] = uint16(i)
	}
	return b
}

// findFreeSlot scans bitmap words in order[] from cursor forward.
// Returns slot index, found bool, and whether a sort should be triggered.
// Must be called with b.mu held.
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

// doSort reorders order[] by ascending popcount and rebuilds inverseOrder.
// Must be called with b.mu held.
func (b *bucket) doSort() {
	var counts [BitmapWords]uint8
	for i, w := range b.bitmap {
		counts[i] = uint8(bits.OnesCount64(w))
	}
	sort.Slice(b.order[:], func(i, j int) bool {
		return counts[b.order[i]] < counts[b.order[j]]
	})
	for logical, physical := range b.order {
		b.inverseOrder[physical] = uint16(logical)
	}
	b.cursor = 0
}

// sortAsync schedules a one-shot background sort. No-ops if one is pending.
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
	cfg     Config
	mu      sync.Mutex
	buckets []*bucket
}

// New returns a Slabber with one pre-allocated bucket.
func New(cfg Config) *Slabber {
	n := cfg.initialBuckets()
	s := &Slabber{cfg: cfg}
	s.buckets = make([]*bucket, n)
	for i := range s.buckets {
		s.buckets[i] = newBucket(cfg.SlotSize)
	}
	return s
}

// grow appends a new bucket. Must be called with s.mu held.
func (s *Slabber) grow() *bucket {
	b := newBucket(s.cfg.SlotSize)
	s.buckets = append(s.buckets, b)
	return b
}

// Alloc reserves a slot and returns its Ref and backing []byte.
// The slice is exactly SlotSize bytes; callers track actual content length
// themselves (e.g. in a separate index).
func (s *Slabber) Alloc() (Ref, []byte, bool) {
	threshold := s.cfg.sortThreshold()

	s.mu.Lock()
	defer s.mu.Unlock()

	for i, b := range s.buckets {
		b.mu.Lock()
		slot, ok, needsSort := b.findFreeSlot(threshold)
		if ok {
			b.bitmap[slot/64] |= 1 << (slot % 64)
			b.freeCount--
			data := s.slotBytes(b, slot)
			b.mu.Unlock()
			if needsSort {
				b.sortAsync()
			}
			return Ref{Bucket: uint32(i), Slot: slot}, data, true
		}
		b.mu.Unlock()
	}

	// All existing buckets full — grow.
	b := s.grow()
	idx := uint32(len(s.buckets) - 1)
	b.mu.Lock()
	slot, ok, _ := b.findFreeSlot(threshold)
	if !ok {
		b.mu.Unlock()
		return Ref{}, nil, false
	}
	b.bitmap[slot/64] |= 1 << (slot % 64)
	b.freeCount--
	data := s.slotBytes(b, slot)
	b.mu.Unlock()
	return Ref{Bucket: idx, Slot: slot}, data, true
}

// Free releases a slot. Returns false if the ref is invalid or the slot
// was already free (double-free guard).
func (s *Slabber) Free(ref Ref) bool {
	s.mu.Lock()
	if int(ref.Bucket) >= len(s.buckets) {
		s.mu.Unlock()
		return false
	}
	b := s.buckets[ref.Bucket]
	s.mu.Unlock()

	wordIdx := ref.Slot / 64
	mask := uint64(1) << (ref.Slot % 64)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.bitmap[wordIdx]&mask == 0 {
		return false // double-free
	}
	b.bitmap[wordIdx] &^= mask
	b.freeCount++

	// Retract cursor to freed word's logical position if earlier. O(1).
	if lp := int(b.inverseOrder[wordIdx]); lp < b.cursor {
		b.cursor = lp
	}
	return true
}

// Slot returns the backing []byte for an allocated Ref without re-allocating.
// The slice shares memory with the arena — do not retain it after Free(ref).
func (s *Slabber) Slot(ref Ref) ([]byte, bool) {
	s.mu.Lock()
	if int(ref.Bucket) >= len(s.buckets) {
		s.mu.Unlock()
		return nil, false
	}
	b := s.buckets[ref.Bucket]
	s.mu.Unlock()

	return s.slotBytes(b, ref.Slot), true
}

// slotBytes returns the byte slice for slot within b. No locking — caller
// must ensure the slot is validly allocated.
func (s *Slabber) slotBytes(b *bucket, slot uint32) []byte {
	start := int(slot) * s.cfg.SlotSize
	return b.data[start : start+s.cfg.SlotSize]
}

// Sort synchronously reorders all buckets by popcount. Prefer the
// auto-triggered background sort; call this only when a guaranteed sort
// point is needed (e.g. before benchmarks).
func (s *Slabber) Sort() {
	s.mu.Lock()
	buckets := s.buckets
	s.mu.Unlock()

	for _, b := range buckets {
		for b.sortPending.Load() {
		} // wait for any background sort
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

// Stats returns current allocation statistics.
func (s *Slabber) Stats() Stats {
	s.mu.Lock()
	buckets := s.buckets
	cfg := s.cfg
	s.mu.Unlock()

	free := 0
	for _, b := range buckets {
		b.mu.Lock()
		free += b.freeCount
		b.mu.Unlock()
	}
	total := len(buckets) * SlotsPerBucket
	memBytes := len(buckets) * SlotsPerBucket * cfg.SlotSize
	return Stats{
		Buckets:    len(buckets),
		TotalSlots: total,
		UsedSlots:  total - free,
		FreeSlots:  free,
		SlotSize:   cfg.SlotSize,
		MemoryMB:   float64(memBytes) / (1024 * 1024),
	}
}

// ---------------------------------------------------------------------------
// Arena — multi-size-class allocator
// ---------------------------------------------------------------------------

// SizeClass defines one allocation tier within an Arena.
type SizeClass struct {
	// MaxSize is the maximum value size this class accepts.
	MaxSize int
	// SlotSize is the actual allocated slot size (must be >= MaxSize).
	// 0 defaults to nextPow2(MaxSize).
	SlotSize int
	// SortThreshold for the backing Slabber. 0 uses DefaultSortThreshold.
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
//	Class 0: ≤64B, Class 1: ≤512B, Class 2: ≤4KB, Class 3: ≤64KB
func DefaultArena() *Arena {
	return NewArena([]SizeClass{
		{MaxSize: 64},
		{MaxSize: 512},
		{MaxSize: 4096},
		{MaxSize: 65536},
	})
}

// Alloc allocates a slot for size bytes. Returns false if size exceeds
// the largest class.
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

// Free releases the slot identified by ref.
func (a *Arena) Free(ref ArenaRef) bool {
	if int(ref.Class) >= len(a.slabbers) {
		return false
	}
	return a.slabbers[ref.Class].Free(ref.Ref)
}

// Slot returns the backing []byte for ref.
func (a *Arena) Slot(ref ArenaRef) ([]byte, bool) {
	if int(ref.Class) >= len(a.slabbers) {
		return nil, false
	}
	return a.slabbers[ref.Class].Slot(ref.Ref)
}

// Stats returns per-class allocation statistics.
func (a *Arena) Stats() []Stats {
	out := make([]Stats, len(a.slabbers))
	for i, s := range a.slabbers {
		out[i] = s.Stats()
	}
	return out
}

// nextPow2 returns the smallest power of two >= v.
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
