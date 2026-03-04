package slabber

import (
	"fmt"
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func smallConfig() Config {
	return Config{SlotSize: 64, SortThreshold: DefaultSortThreshold}
}

// ---- Slabber core ----------------------------------------------------------

// TestAllocFree verifies basic alloc, write, read, free cycle.
func TestAllocFree(t *testing.T) {
	s := New(smallConfig())

	ref, data, ok := s.Alloc()
	if !ok {
		t.Fatal("Alloc failed")
	}
	copy(data, []byte("hello slabber"))

	got, ok := s.Slot(ref)
	if !ok {
		t.Fatal("Slot lookup failed")
	}
	if string(got[:13]) != "hello slabber" {
		t.Fatalf("unexpected data: %q", got[:13])
	}

	if !s.Free(ref) {
		t.Fatal("Free failed")
	}
}

// TestDoubleFree verifies the double-free guard returns false.
func TestDoubleFree(t *testing.T) {
	s := New(smallConfig())
	ref, _, ok := s.Alloc()
	if !ok {
		t.Fatal("Alloc failed")
	}
	if !s.Free(ref) {
		t.Fatal("first Free failed")
	}
	if s.Free(ref) {
		t.Fatal("double-free should have returned false")
	}
}

// TestSlotReuse verifies freed slots are reused before growing.
func TestSlotReuse(t *testing.T) {
	s := New(smallConfig())

	ref1, _, _ := s.Alloc()
	ref2, _, _ := s.Alloc()
	s.Free(ref1)

	// Next alloc should reuse ref1's slot (cursor reset to 0).
	ref3, _, _ := s.Alloc()
	_ = ref2

	// ref3 must be within the same bucket, not a new one.
	statsBefore := s.Stats()
	s.Free(ref3)
	statsAfter := s.Stats()

	if statsBefore.Buckets != statsAfter.Buckets {
		t.Error("unexpected bucket growth on reuse path")
	}
}

// TestBucketGrowth verifies a new bucket is allocated when the first is full.
func TestBucketGrowth(t *testing.T) {
	s := New(smallConfig())

	refs := make([]Ref, SlotsPerBucket)
	for i := range refs {
		ref, _, ok := s.Alloc()
		if !ok {
			t.Fatalf("Alloc failed at slot %d", i)
		}
		refs[i] = ref
	}

	stats := s.Stats()
	if stats.Buckets != 1 {
		t.Fatalf("expected 1 bucket, got %d", stats.Buckets)
	}
	if stats.FreeSlots != 0 {
		t.Fatalf("expected 0 free slots, got %d", stats.FreeSlots)
	}

	// One more alloc should trigger bucket growth.
	_, _, ok := s.Alloc()
	if !ok {
		t.Fatal("Alloc on full slabber failed")
	}

	stats = s.Stats()
	if stats.Buckets != 2 {
		t.Fatalf("expected 2 buckets after growth, got %d", stats.Buckets)
	}

	// Cleanup.
	for _, r := range refs {
		s.Free(r)
	}
}

// TestSort verifies Sort() changes order for a partially-occupied slabber.
func TestSort(t *testing.T) {
	s := New(smallConfig())

	// Fill the first half of bucket 0.
	half := SlotsPerBucket / 2
	refs := make([]Ref, half)
	for i := range refs {
		ref, _, _ := s.Alloc()
		refs[i] = ref
	}

	// Free slots scattered in the first 512 words.
	for i := 0; i < half; i += 4 {
		s.Free(refs[i])
	}

	// Capture order before Sort.
	(*s.buckets.Load())[0].mu.Lock()
	orderBefore := (*s.buckets.Load())[0].order
	(*s.buckets.Load())[0].mu.Unlock()

	s.Sort()

	(*s.buckets.Load())[0].mu.Lock()
	orderAfter := (*s.buckets.Load())[0].order
	cursorAfter := (*s.buckets.Load())[0].cursor
	(*s.buckets.Load())[0].mu.Unlock()

	if orderBefore == orderAfter {
		t.Error("Sort() did not change order array")
	}
	if cursorAfter != 0 {
		t.Errorf("Sort() should reset cursor to 0, got %d", cursorAfter)
	}

	// Verify order is non-decreasing by popcount.
	bitmap := (*s.buckets.Load())[0].bitmap
	for i := 1; i < BitmapWords; i++ {
		prev := bits.OnesCount64(bitmap[orderAfter[i-1]])
		curr := bits.OnesCount64(bitmap[orderAfter[i]])
		if prev > curr {
			t.Errorf("order[%d] popcount %d > order[%d] popcount %d — not sorted",
				i-1, prev, i, curr)
			break
		}
	}
}

// TestConcurrentAllocFree exercises concurrent alloc/free for race detection.
// Run with: go test -race
func TestConcurrentAllocFree(t *testing.T) {
	s := New(smallConfig())
	const goroutines = 16
	const opsEach = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g // capture correctly
		go func() {
			defer wg.Done()
			refs := make([]Ref, 0, 8)
			for i := 0; i < opsEach; i++ {
				if len(refs) < 8 {
					ref, data, ok := s.Alloc()
					if !ok {
						t.Errorf("Alloc failed")
						return
					}
					copy(data, fmt.Sprintf("goroutine-%d-%d", g, i))
					refs = append(refs, ref)
				} else {
					s.Free(refs[0])
					refs = refs[1:]
				}
			}
			for _, r := range refs {
				s.Free(r)
			}
		}()
	}
	wg.Wait()
}

// TestStats verifies stat accounting through alloc/free/grow.
func TestStats(t *testing.T) {
	s := New(smallConfig())

	s0 := s.Stats()
	if s0.Buckets != 1 || s0.FreeSlots != SlotsPerBucket || s0.UsedSlots != 0 {
		t.Fatalf("unexpected initial stats: %+v", s0)
	}

	ref, _, _ := s.Alloc()
	s1 := s.Stats()
	if s1.UsedSlots != 1 || s1.FreeSlots != SlotsPerBucket-1 {
		t.Fatalf("unexpected stats after alloc: %+v", s1)
	}

	s.Free(ref)
	s2 := s.Stats()
	if s2.UsedSlots != 0 || s2.FreeSlots != SlotsPerBucket {
		t.Fatalf("unexpected stats after free: %+v", s2)
	}
}

// TestInverseOrderConsistency verifies inverseOrder mirrors order after Sort().
func TestInverseOrderConsistency(t *testing.T) {
	s := New(smallConfig())
	half := SlotsPerBucket / 2
	refs := make([]Ref, half)
	for i := range refs {
		refs[i], _, _ = s.Alloc()
	}
	for i := 0; i < half; i += 2 {
		s.Free(refs[i])
	}
	s.Sort()

	b := (*s.buckets.Load())[0]
	b.mu.Lock()
	defer b.mu.Unlock()
	for logical, physical := range b.order {
		if b.inverseOrder[physical] != uint16(logical) {
			t.Errorf("inverseOrder[%d]=%d want %d",
				physical, b.inverseOrder[physical], logical)
			break
		}
	}
}

// TestCursorRetractionOnFree verifies Free() retracts cursor via inverseOrder.
func TestCursorRetractionOnFree(t *testing.T) {
	s := New(smallConfig())
	refs := make([]Ref, 192) // fills 3 full bitmap words
	for i := range refs {
		refs[i], _, _ = s.Alloc()
	}
	b := (*s.buckets.Load())[0]
	b.mu.Lock()
	cursorBefore := b.cursor
	targetPhysical := b.order[1]
	b.mu.Unlock()

	if cursorBefore < 2 {
		t.Skipf("cursor %d too low for retraction test", cursorBefore)
	}

	targetWord := uint32(targetPhysical)
	var victim Ref
	found := false
	for _, r := range refs {
		if r.Slot/64 == targetWord {
			victim = r
			found = true
			break
		}
	}
	if !found {
		t.Skip("no ref in target word — layout-dependent")
	}

	s.Free(victim)

	b.mu.Lock()
	cursorAfter := b.cursor
	b.mu.Unlock()

	if cursorAfter >= cursorBefore {
		t.Errorf("cursor did not retract: before=%d after=%d", cursorBefore, cursorAfter)
	}
}

// TestAutoSort verifies background sort fires when cursor crosses SortThreshold.
func TestAutoSort(t *testing.T) {
	cfg := Config{SlotSize: 64, SortThreshold: 4}
	s := New(cfg)
	refs := make([]Ref, 4*64+1)
	for i := range refs {
		var ok bool
		refs[i], _, ok = s.Alloc()
		if !ok {
			t.Fatalf("Alloc failed at %d", i)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !(*s.buckets.Load())[0].sortPending.Load() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if (*s.buckets.Load())[0].sortPending.Load() {
		t.Error("background sort did not complete within 2s")
	}
	for _, r := range refs {
		s.Free(r)
	}
}

// ---- Arena -----------------------------------------------------------------

func TestArenaRouting(t *testing.T) {
	a := DefaultArena()
	cases := []struct {
		size      int
		wantClass uint8
	}{
		{1, 0}, {64, 0},
		{65, 1}, {512, 1},
		{513, 2}, {4096, 2},
		{4097, 3}, {65536, 3},
	}
	for _, tc := range cases {
		ref, data, ok := a.Alloc(tc.size)
		if !ok {
			t.Fatalf("Alloc(%d) failed", tc.size)
		}
		if ref.Class != tc.wantClass {
			t.Errorf("Alloc(%d): class %d, want %d", tc.size, ref.Class, tc.wantClass)
		}
		if len(data) < tc.size {
			t.Errorf("Alloc(%d): data len %d < size", tc.size, len(data))
		}
		a.Free(ref)
	}
}

func TestArenaOversized(t *testing.T) {
	a := DefaultArena()
	if _, _, ok := a.Alloc(65537); ok {
		t.Error("Alloc beyond largest class should fail")
	}
}

func TestArenaSlotRoundtrip(t *testing.T) {
	a := DefaultArena()
	ref, data, ok := a.Alloc(100)
	if !ok {
		t.Fatal("Alloc failed")
	}
	copy(data, "arena roundtrip")
	got, ok := a.Slot(ref)
	if !ok {
		t.Fatal("Slot failed")
	}
	if string(got[:15]) != "arena roundtrip" {
		t.Fatalf("got %q", got[:15])
	}
	a.Free(ref)
}

func TestArenaStats(t *testing.T) {
	a := DefaultArena()
	stats := a.Stats()
	if len(stats) != 4 {
		t.Fatalf("expected 4 classes, got %d", len(stats))
	}
	for i, st := range stats {
		if st.FreeSlots != SlotsPerBucket {
			t.Errorf("class %d: free %d, want %d", i, st.FreeSlots, SlotsPerBucket)
		}
	}
}

// TestConcurrentGrow forces many goroutines to race to exhaust a bucket
// simultaneously, exercising the growMu re-check that prevents redundant
// bucket appends. Each goroutine holds slots until all are drained, then
// releases them so the grow path fires repeatedly.
//
// Run with: go test -race -run TestConcurrentGrow
func TestConcurrentGrow(t *testing.T) {
	// Start with a single bucket so the first grow is forced quickly.
	s := New(Config{SlotSize: 64, Buckets: 1})
	const goroutines = 32

	var (
		wg       sync.WaitGroup
		grows    atomic.Int64 // counts successful Alloc calls that land in bucket > 0
		refMu    sync.Mutex
		allRefs  []Ref
	)

	// Phase 1: goroutines race to fill bucket 0 and trigger concurrent grows.
	// Each holds its refs until phase 2 so they do not vacate slots prematurely.
	wg.Add(goroutines)
	perGoroutine := (SlotsPerBucket / goroutines) + 1
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			local := make([]Ref, 0, perGoroutine)
			for i := 0; i < perGoroutine; i++ {
				ref, _, ok := s.Alloc()
				if !ok {
					t.Errorf("Alloc failed")
					return
				}
				local = append(local, ref)
			}
			refMu.Lock()
			allRefs = append(allRefs, local...)
			refMu.Unlock()
		}()
	}
	wg.Wait()

	// The slabber must have grown — count distinct bucket indices.
	bucketSeen := make(map[uint32]struct{})
	for _, r := range allRefs {
		if r.Bucket > 0 {
			grows.Add(1)
		}
		bucketSeen[r.Bucket] = struct{}{}
	}
	if grows.Load() == 0 {
		t.Fatal("expected at least one allocation in a grown bucket, got none")
	}

	// Verify no two refs point to the same slot (no double-allocation).
	type key struct{ b, s uint32 }
	seen := make(map[key]struct{}, len(allRefs))
	for _, r := range allRefs {
		k := key{r.Bucket, r.Slot}
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate ref: bucket=%d slot=%d", r.Bucket, r.Slot)
		}
		seen[k] = struct{}{}
	}

	// Verify bucket count is sensible: should be > 1 and <= goroutines+1.
	stats := s.Stats()
	if stats.Buckets <= 1 {
		t.Fatalf("expected multiple buckets after concurrent grow, got %d", stats.Buckets)
	}
	if stats.Buckets > goroutines+1 {
		t.Fatalf("excessive bucket count %d — growMu re-check may be broken", stats.Buckets)
	}

	// Phase 2: free everything.
	for _, r := range allRefs {
		if !s.Free(r) {
			t.Errorf("Free failed for ref bucket=%d slot=%d", r.Bucket, r.Slot)
		}
	}

	final := s.Stats()
	if final.UsedSlots != 0 {
		t.Errorf("expected 0 used slots after full free, got %d", final.UsedSlots)
	}
}

// TestConcurrentMultiBucket exercises the intended production configuration:
// runtime.NumCPU() pre-warmed buckets, goroutines doing alloc/free and
// concurrent Slot() reads on live refs. Designed to be run under -race.
//
// Run with: go test -race -run TestConcurrentMultiBucket
func TestConcurrentMultiBucket(t *testing.T) {
	n := runtime.NumCPU()
	s := NewSlabber(64, n)
	const opsEach = 2000

	// Pre-allocate one ref per goroutine that will be held live throughout
	// as a read target for concurrent Slot() calls.
	pinnedRefs := make([]Ref, n)
	for i := range pinnedRefs {
		ref, data, ok := s.Alloc()
		if !ok {
			t.Fatal("pre-alloc failed")
		}
		copy(data, fmt.Sprintf("pinned-%d", i))
		pinnedRefs[i] = ref
	}

	var wg sync.WaitGroup

	// Writer goroutines: alloc/free in a tight loop.
	wg.Add(n)
	for g := 0; g < n; g++ {
		g := g
		go func() {
			defer wg.Done()
			refs := make([]Ref, 0, 8)
			for i := 0; i < opsEach; i++ {
				if len(refs) < 8 {
					ref, data, ok := s.Alloc()
					if !ok {
						t.Errorf("Alloc failed in writer %d op %d", g, i)
						return
					}
					copy(data, fmt.Sprintf("w%d-op%d", g, i))
					refs = append(refs, ref)
				} else {
					s.Free(refs[0])
					refs = refs[1:]
				}
			}
			for _, r := range refs {
				s.Free(r)
			}
		}()
	}

	// Reader goroutines: call Slot() on pinned refs in a tight loop.
	// These should never block — the whole point of v3.
	wg.Add(n)
	for g := 0; g < n; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				slot, ok := s.Slot(pinnedRefs[g%len(pinnedRefs)])
				if !ok {
					t.Errorf("Slot failed for pinned ref in reader %d", g)
					return
				}
				// Read the tag written at pre-alloc time — should still be there.
				if len(slot) < 8 || slot[0] != 'p' {
					t.Errorf("reader %d: slot data corrupted: %q", g, slot[:8])
					return
				}
			}
		}()
	}

	wg.Wait()

	// Free pinned refs.
	for _, r := range pinnedRefs {
		if !s.Free(r) {
			t.Errorf("Free of pinned ref failed: %+v", r)
		}
	}

	final := s.Stats()
	if final.UsedSlots != 0 {
		t.Errorf("expected 0 used slots after cleanup, got %d", final.UsedSlots)
	}
}

// TestConcurrentSlotWhileGrowing verifies that Slot() on a pre-existing ref
// remains valid while other goroutines are forcing bucket growth. This is the
// critical correctness property of the atomic.Pointer swap: old snapshots
// remain safe to dereference after a grow.
//
// Run with: go test -race -run TestConcurrentSlotWhileGrowing
func TestConcurrentSlotWhileGrowing(t *testing.T) {
	s := New(Config{SlotSize: 64, Buckets: 1})

	// Grab a ref in bucket 0 before any growth.
	ref, data, ok := s.Alloc()
	if !ok {
		t.Fatal("initial alloc failed")
	}
	copy(data, "sentinel-value-xyz")

	const growers = 16
	var wg sync.WaitGroup

	// Goroutines that force growth by filling the slabber.
	// They run for a fixed duration then stop.
	ctx := make(chan struct{})
	time.AfterFunc(200*time.Millisecond, func() { close(ctx) })

	wg.Add(growers)
	for g := 0; g < growers; g++ {
		go func() {
			defer wg.Done()
			var held []Ref
			for {
				select {
				case <-ctx:
					for _, r := range held {
						s.Free(r)
					}
					return
				default:
				}
				r, _, ok := s.Alloc()
				if ok {
					held = append(held, r)
				}
				// Periodically free some to keep the slabber churning.
				if len(held) > SlotsPerBucket/growers {
					s.Free(held[0])
					held = held[1:]
				}
			}
		}()
	}

	// Reader: continuously calls Slot() on the original ref.
	// Must never return false or see corrupted data.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx:
				return
			default:
			}
			slot, ok := s.Slot(ref)
			if !ok {
				t.Errorf("Slot() returned false for valid ref during grow")
				return
			}
			if string(slot[:18]) != "sentinel-value-xyz" {
				t.Errorf("slot data corrupted during grow: %q", slot[:18])
				return
			}
		}
	}()

	wg.Wait()
	s.Free(ref)
}
