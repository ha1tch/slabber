package v0

import (
	"math/bits"
	"sort"
	"sync"
)

const (
	BitmapWords    = 1024
	SlotsPerBucket = BitmapWords * 64
	DefaultSlotSize = 4096
)

type Config struct {
	SlotSize int
	Buckets  int
}

func DefaultConfig() Config {
	return Config{SlotSize: DefaultSlotSize}
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
	mu        sync.Mutex
	bitmap    [BitmapWords]uint64
	order     [BitmapWords]uint16
	data      []byte
	freeCount int
	cursor    int
}

func newBucket(slotSize int) *bucket {
	b := &bucket{
		data:      make([]byte, SlotsPerBucket*slotSize),
		freeCount: SlotsPerBucket,
	}
	for i := range b.order {
		b.order[i] = uint16(i)
	}
	return b
}

func (b *bucket) findFreeSlot() (uint32, bool) {
	for i := b.cursor; i < BitmapWords; i++ {
		wi := b.order[i]
		w := b.bitmap[wi]
		if w == ^uint64(0) {
			b.cursor = i + 1
			continue
		}
		b.cursor = i
		bit := bits.TrailingZeros64(^w)
		return uint32(wi)*64 + uint32(bit), true
	}
	return 0, false
}

type Slabber struct {
	cfg     Config
	mu      sync.Mutex
	buckets []*bucket
}

func New(cfg Config) *Slabber {
	n := cfg.initialBuckets()
	s := &Slabber{cfg: cfg}
	s.buckets = make([]*bucket, n)
	for i := range s.buckets {
		s.buckets[i] = newBucket(cfg.SlotSize)
	}
	return s
}

func (s *Slabber) grow() *bucket {
	b := newBucket(s.cfg.SlotSize)
	s.buckets = append(s.buckets, b)
	return b
}

func (s *Slabber) Alloc() (Ref, []byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i, b := range s.buckets {
		b.mu.Lock()
		slot, ok := b.findFreeSlot()
		if ok {
			b.bitmap[slot/64] |= 1 << (slot % 64)
			b.freeCount--
			data := s.slotBytes(b, slot)
			b.mu.Unlock()
			return Ref{Bucket: uint32(i), Slot: slot}, data, true
		}
		b.mu.Unlock()
	}

	b := s.grow()
	idx := uint32(len(s.buckets) - 1)
	b.mu.Lock()
	slot, ok := b.findFreeSlot()
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

func (s *Slabber) Free(ref Ref) bool {
	s.mu.Lock()
	if int(ref.Bucket) >= len(s.buckets) {
		s.mu.Unlock()
		return false
	}
	b := s.buckets[ref.Bucket]
	s.mu.Unlock()

	wordIdx := ref.Slot / 64
	bitIdx := ref.Slot % 64
	mask := uint64(1) << bitIdx

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.bitmap[wordIdx]&mask == 0 {
		return false
	}
	b.bitmap[wordIdx] &^= mask
	b.freeCount++
	if b.cursor > 0 {
		b.cursor = 0
	}
	return true
}

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

func (s *Slabber) slotBytes(b *bucket, slot uint32) []byte {
	start := int(slot) * s.cfg.SlotSize
	return b.data[start : start+s.cfg.SlotSize]
}

func (s *Slabber) Sort() {
	s.mu.Lock()
	buckets := s.buckets
	s.mu.Unlock()

	for _, b := range buckets {
		b.mu.Lock()
		var counts [BitmapWords]uint8
		for i, w := range b.bitmap {
			counts[i] = uint8(bits.OnesCount64(w))
		}
		sort.Slice(b.order[:], func(i, j int) bool {
			return counts[b.order[i]] < counts[b.order[j]]
		})
		b.cursor = 0
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
	return Stats{
		Buckets:    len(buckets),
		TotalSlots: total,
		UsedSlots:  total - free,
		FreeSlots:  free,
		SlotSize:   cfg.SlotSize,
		MemoryMB:   float64(len(buckets)*SlotsPerBucket*cfg.SlotSize) / (1024 * 1024),
	}
}
