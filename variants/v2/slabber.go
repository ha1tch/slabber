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
// Concurrency model (v2):
//   - s.mu is a sync.RWMutex. Readers (Alloc scan, Free, Slot) take RLock
//     briefly to read the bucket list pointer; only grow() takes a full Lock.
//   - lockMask (atomic.Uint64) advertises which buckets are currently locked.
//     Alloc reads ~lockMask to steer TryLock attempts toward unlocked buckets,
//     avoiding contention before it happens. Covers buckets 0-63.
//   - freeCount (atomic.Int32) per bucket is readable without b.mu as a hint
//     to skip full buckets without acquiring any lock. Authoritatively updated
//     under b.mu.
//   - doSort() uses a counting sort (O(n)) rather than sort.Slice (O(n log n)),
//     shortening the window during which b.mu is held by a background sort.
//
// The GC sees one []byte per bucket regardless of keycount.
package v2

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
)

const (
	BitmapWords      = 1024
	SlotsPerBucket   = BitmapWords * 64
	DefaultSlotSize  = 4096
	DefaultSortThreshold = 256
	lockMaskCap      = 64
)

type Config struct {
	SlotSize      int
	SortThreshold int
	// Buckets is the number of arenas to pre-allocate at construction time.
	// Set to runtime.NumCPU() or higher so goroutines can spread across
	// buckets immediately without racing to grow. 0 defaults to 1.
	Buckets int
}

func DefaultConfig() Config {
	return Config{SlotSize: DefaultSlotSize, SortThreshold: DefaultSortThreshold, Buckets: 1}
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

type Ref struct {
	Bucket uint32
	Slot   uint32
}

type bucket struct {
	mu           sync.Mutex
	bitmap       [BitmapWords]uint64
	order        [BitmapWords]uint16
	inverseOrder [BitmapWords]uint16
	data         []byte
	freeCount    atomic.Int32 // updated under mu; readable without mu as hint
	cursor       int
	sortPending  atomic.Bool
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

type Slabber struct {
	cfg      Config
	mu       sync.RWMutex
	buckets  []*bucket
	lockMask atomic.Uint64
}

// New returns a Slabber configured by cfg.
// Pre-allocates cfg.Buckets arenas (default 1); set cfg.Buckets to
// runtime.NumCPU() so goroutines spread across arenas from the first alloc.
func New(cfg Config) *Slabber {
	n := cfg.initialBuckets()
	s := &Slabber{cfg: cfg}
	s.buckets = make([]*bucket, n)
	for i := range s.buckets {
		s.buckets[i] = newBucket(cfg.SlotSize)
	}
	return s
}

// NewSlabber is a convenience constructor for the common case.
// slotSize is the byte size of each slot; buckets is the number of arenas
// to pre-allocate — pass runtime.NumCPU() for best concurrent performance.
// All other settings use defaults (SortThreshold = 256).
func NewSlabber(slotSize, buckets int) *Slabber {
	return New(Config{
		SlotSize:      slotSize,
		SortThreshold: DefaultSortThreshold,
		Buckets:       buckets,
	})
}

func (s *Slabber) grow() *bucket {
	b := newBucket(s.cfg.SlotSize)
	s.buckets = append(s.buckets, b)
	return b
}

func maskBitFor(i int) (bit uint64, ok bool) {
	if i >= lockMaskCap {
		return 0, false
	}
	return uint64(1) << i, true
}

func (s *Slabber) Alloc() (Ref, []byte, bool) {
	threshold := s.cfg.sortThreshold()

	s.mu.RLock()
	buckets := s.buckets
	s.mu.RUnlock()

	n := len(buckets)

	// --- Fast path: lock steering ---
	//
	// validMask covers only the buckets we know about (0..n-1).
	// tried accumulates bits for buckets we have already attempted this
	// pass so we never re-visit one; lockMask is re-read on every
	// iteration so we see buckets that became free while we looped.
	//
	// The bit is set in lockMask BEFORE attempting TryLock, signalling
	// intent immediately. If TryLock fails the bit is cleared and we
	// move on; other goroutines that read the mask in the meantime will
	// steer away from this bucket — correct, since we were about to
	// contend for it anyway.
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
	s.mu.RLock()
	buckets = s.buckets
	s.mu.RUnlock()

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
	s.mu.Lock()
	b := s.grow()
	idx := uint32(len(s.buckets) - 1)
	s.mu.Unlock()

	bit, hasBit := maskBitFor(int(idx))
	if hasBit {
		s.lockMask.Or(bit) // signal intent before locking new bucket
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

func (s *Slabber) Free(ref Ref) bool {
	s.mu.RLock()
	if int(ref.Bucket) >= len(s.buckets) {
		s.mu.RUnlock()
		return false
	}
	b := s.buckets[ref.Bucket]
	s.mu.RUnlock()

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
		return false
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
// No per-bucket lock required: bucket data is immutable after creation.
func (s *Slabber) Slot(ref Ref) ([]byte, bool) {
	s.mu.RLock()
	if int(ref.Bucket) >= len(s.buckets) {
		s.mu.RUnlock()
		return nil, false
	}
	b := s.buckets[ref.Bucket]
	s.mu.RUnlock()
	return s.slotBytes(b, ref.Slot), true
}

func (s *Slabber) slotBytes(b *bucket, slot uint32) []byte {
	start := int(slot) * s.cfg.SlotSize
	return b.data[start : start+s.cfg.SlotSize]
}

func (s *Slabber) Sort() {
	s.mu.RLock()
	buckets := s.buckets
	s.mu.RUnlock()

	for _, b := range buckets {
		for b.sortPending.Load() {
			runtime.Gosched()
		}
		b.mu.Lock()
		b.doSort()
		b.mu.Unlock()
	}
}

type Stats struct {
	Buckets    int
	TotalSlots int
	UsedSlots  int
	FreeSlots  int
	SlotSize   int
	MemoryMB   float64
}

// Stats reads freeCount atomically; no per-bucket lock required.
func (s *Slabber) Stats() Stats {
	s.mu.RLock()
	buckets := s.buckets
	cfg := s.cfg
	s.mu.RUnlock()

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
// Arena
// ---------------------------------------------------------------------------

type SizeClass struct {
	MaxSize       int
	SlotSize      int
	SortThreshold int
}

type ArenaRef struct {
	Ref
	Class uint8
}

type Arena struct {
	classes  []SizeClass
	slabbers []*Slabber
}

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

