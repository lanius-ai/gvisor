// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mm

import (
	"bytes"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/usermem"
)

func testMemoryManagerWithMmapDirection(ctx context.Context, t *testing.T, mmapDirection arch.MmapDirection) *MemoryManager {
	p := platform.FromContext(ctx)
	mm, err := NewMemoryManager(p, pgalloc.MemoryFileFromContext(ctx))
	if err != nil {
		t.Fatalf("failed to create MemoryManager: %s", err)
	}
	mm.layout = arch.MmapLayout{
		MinAddr:          p.MinUserAddress(),
		MaxAddr:          p.MaxUserAddress(),
		BottomUpBase:     p.MinUserAddress(),
		TopDownBase:      p.MaxUserAddress(),
		DefaultDirection: mmapDirection,
	}
	return mm
}

func testMemoryManager(ctx context.Context, t *testing.T) *MemoryManager {
	return testMemoryManagerWithMmapDirection(ctx, t, arch.MmapBottomUp)
}

func (mm *MemoryManager) realUsageAS() uint64 {
	return uint64(mm.vmas.Span())
}

func TestUsageASUpdates(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:  2 * hostarch.PageSize,
		Private: true,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	realUsage := mm.realUsageAS()
	if mm.usageAS != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", mm.usageAS, realUsage)
	}

	mm.MUnmap(ctx, addr, hostarch.PageSize)
	realUsage = mm.realUsageAS()
	if mm.usageAS != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", mm.usageAS, realUsage)
	}
}

func (mm *MemoryManager) realDataAS() uint64 {
	var sz uint64
	for seg := mm.vmas.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		vma := seg.ValuePtr()
		if vma.isPrivateDataLocked() {
			sz += uint64(seg.Range().Length())
		}
	}
	return sz
}

func TestDataASUpdates(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   3 * hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.Write,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	if mm.dataAS == 0 {
		t.Fatalf("dataAS is 0, wanted not 0")
	}
	realDataAS := mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MUnmap(ctx, addr, hostarch.PageSize)
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MProtect(addr+hostarch.PageSize, hostarch.PageSize, hostarch.Read, false)
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MRemap(ctx, addr+2*hostarch.PageSize, hostarch.PageSize, 2*hostarch.PageSize, MRemapOpts{
		Move: MRemapMayMove,
	})
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}
}

func TestBrkDataLimitUpdates(t *testing.T) {
	limitSet := limits.NewLimitSet()
	limitSet.Set(limits.Data, limits.Limit{}, true /* privileged */) // zero RLIMIT_DATA

	ctx := contexttest.WithLimitSet(contexttest.Context(t), limitSet)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Try to extend the brk by one page and expect doing so to fail.
	oldBrk, _ := mm.Brk(ctx, 0)
	if newBrk, _ := mm.Brk(ctx, oldBrk+hostarch.PageSize); newBrk != oldBrk {
		t.Errorf("brk() increased data segment above RLIMIT_DATA (old brk = %#x, new brk = %#x", oldBrk, newBrk)
	}
}

// TestIOAfterUnmap ensures that IO fails after unmap.
func TestIOAfterUnmap(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.Read,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// IO works before munmap.
	b := make([]byte, 1)
	n, err := mm.CopyIn(ctx, addr, b, usermem.IOOpts{})
	if err != nil {
		t.Errorf("CopyIn got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyIn got %d want 1", n)
	}

	err = mm.MUnmap(ctx, addr, hostarch.PageSize)
	if err != nil {
		t.Fatalf("MUnmap got err %v want nil", err)
	}

	n, err = mm.CopyIn(ctx, addr, b, usermem.IOOpts{})
	if !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("CopyIn got err %v want EFAULT", err)
	}
	if n != 0 {
		t.Errorf("CopyIn got %d want 0", n)
	}
}

// TestIOAfterMProtect tests IO interaction with mprotect permissions.
func TestIOAfterMProtect(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// Writing works before mprotect.
	b := make([]byte, 1)
	n, err := mm.CopyOut(ctx, addr, b, usermem.IOOpts{})
	if err != nil {
		t.Errorf("CopyOut got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyOut got %d want 1", n)
	}

	err = mm.MProtect(addr, hostarch.PageSize, hostarch.Read, false)
	if err != nil {
		t.Errorf("MProtect got err %v want nil", err)
	}

	// Without IgnorePermissions, CopyOut should no longer succeed.
	n, err = mm.CopyOut(ctx, addr, b, usermem.IOOpts{})
	if !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("CopyOut got err %v want EFAULT", err)
	}
	if n != 0 {
		t.Errorf("CopyOut got %d want 0", n)
	}

	// With IgnorePermissions, CopyOut should succeed despite mprotect.
	n, err = mm.CopyOut(ctx, addr, b, usermem.IOOpts{
		IgnorePermissions: true,
	})
	if err != nil {
		t.Errorf("CopyOut got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyOut got %d want 1", n)
	}
}

// TestAIOPrepareAfterDestroy tests that AIOContext should not be able to be
// prepared after destruction.
func TestAIOPrepareAfterDestroy(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	id, err := mm.NewAIOContext(ctx, 1)
	if err != nil {
		t.Fatalf("mm.NewAIOContext got err %v want nil", err)
	}
	aioCtx, ok := mm.LookupAIOContext(ctx, id)
	if !ok {
		t.Fatalf("AIOContext not found")
	}
	mm.DestroyAIOContext(ctx, id)

	// Prepare should fail because aioCtx should be destroyed.
	if err := aioCtx.Prepare(); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("aioCtx.Prepare got err %v want nil", err)
	} else if err == nil {
		aioCtx.CancelPendingRequest()
	}
}

// TestAIOLookupAfterDestroy tests that AIOContext should not be able to be
// looked up after memory manager is destroyed.
func TestAIOLookupAfterDestroy(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)

	id, err := mm.NewAIOContext(ctx, 1)
	if err != nil {
		mm.DecUsers(ctx)
		t.Fatalf("mm.NewAIOContext got err %v want nil", err)
	}
	mm.DecUsers(ctx) // This destroys the AIOContext manager.

	if _, ok := mm.LookupAIOContext(ctx, id); ok {
		t.Errorf("AIOContext found even after AIOContext manager is destroyed")
	}
}

func TestGetAllocationDirection(t *testing.T) {
	testCases := []struct {
		name          string
		mmapDirection arch.MmapDirection
		ar            hostarch.AddrRange
		vma           *vma
		expected      pgalloc.Direction
	}{
		{
			"No last fault in vma with mmap direction BottomUp",
			arch.MmapBottomUp,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 0},
			pgalloc.BottomUp,
		},
		{
			"No last fault in vma with mmap direction TopDown",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 0},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma equals to addr range, with mmap direction BottomUp",
			arch.MmapBottomUp,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 123},
			pgalloc.BottomUp,
		},
		{
			"Last fault in vma equals to addr range, with mmap direction TopDown",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 123},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma greater than addr range",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 456},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma smaller than addr range",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 100},
			pgalloc.BottomUp,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm := testMemoryManagerWithMmapDirection(ctx, t, test.mmapDirection)
			actual := mm.getAllocationDirection(test.ar, test.vma)
			if actual != test.expected {
				t.Errorf("Unexpected allocation direction. Expected: %s, Actual: %s", test.expected, actual)
			}
		})
	}
}

// readPinnedRange returns the current contents of the pinned range pr.
func readPinnedRange(t *testing.T, pr PinnedRange) []byte {
	t.Helper()
	ims, err := pr.File.MapInternal(pr.FileRange(), hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal got err %v want nil", err)
	}
	buf := make([]byte, pr.Source.Length())
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), ims); err != nil {
		t.Fatalf("CopySeq got err %v want nil", err)
	}
	return buf
}

// TestPinnedPagesCopiedOnFork tests that Fork copies pinned pages to the
// child immediately instead of making them copy-on-write, so that the
// parent's mappings never diverge from pages registered for DMA.
func TestPinnedPagesCopiedOnFork(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Map 3 pages, but pin only the middle page, so that Fork must handle
	// both the pinned page and the copy-on-write pages surrounding it.
	const npages = 3
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   npages * hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	preFork := bytes.Repeat([]byte{'A'}, npages*hostarch.PageSize)
	if _, err := mm.CopyOut(ctx, addr, preFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	pinAR := hostarch.AddrRange{
		Start: addr + hostarch.PageSize,
		End:   addr + 2*hostarch.PageSize,
	}
	prs, err := mm.Pin(ctx, pinAR, hostarch.ReadWrite, false /* ignorePermissions */)
	if err != nil {
		t.Fatalf("Pin got err %v want nil", err)
	}
	defer Unpin(prs)

	// Fork twice so that both the first fork of a pinned pma, and the fork
	// of the resulting parent pma state, are tested.
	for i := 0; i < 2; i++ {
		mm2, err := mm.Fork(ctx)
		if err != nil {
			t.Fatalf("Fork got err %v want nil", err)
		}
		defer mm2.DecUsers(ctx)

		// The parent's writes to the pinned page must be visible through the
		// pinned range, i.e. the parent must not have been moved to a new
		// copy of the page by copy-on-write.
		postFork := bytes.Repeat([]byte{'B' + byte(i)}, npages*hostarch.PageSize)
		if _, err := mm.CopyOut(ctx, addr, postFork, usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyOut got err %v want nil", err)
		}
		if got, want := readPinnedRange(t, prs[0]), postFork[:hostarch.PageSize]; !bytes.Equal(got, want) {
			t.Errorf("pinned page contains %q..., want %q...; parent's mapping of the pinned page diverged", got[:4], want[:4])
		}

		// The child must see the pre-fork contents of all pages.
		childBuf := make([]byte, npages*hostarch.PageSize)
		if _, err := mm2.CopyIn(ctx, addr, childBuf, usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyIn got err %v want nil", err)
		}
		if !bytes.Equal(childBuf, preFork) {
			t.Errorf("child read %q..., want %q...", childBuf[:4], preFork[:4])
		}

		// The child's writes must not be visible through the pinned range.
		if _, err := mm2.CopyOut(ctx, addr, bytes.Repeat([]byte{'z'}, npages*hostarch.PageSize), usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyOut got err %v want nil", err)
		}
		if got, want := readPinnedRange(t, prs[0]), postFork[:hostarch.PageSize]; !bytes.Equal(got, want) {
			t.Errorf("pinned page contains %q..., want %q...; child's writes are visible through the pinned range", got[:4], want[:4])
		}

		preFork = postFork
	}
}

// TestPinUnsharesCopyOnWritePages tests that pinning copy-on-write pages
// breaks copy-on-write, so that the pinned pages are those mapped by the
// pinning process, even if the pin does not require write access.
func TestPinUnsharesCopyOnWritePages(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	preFork := bytes.Repeat([]byte{'A'}, hostarch.PageSize)
	if _, err := mm.CopyOut(ctx, addr, preFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	// Make the page copy-on-write by forking, then pin it for reading only.
	mm2, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork got err %v want nil", err)
	}
	defer mm2.DecUsers(ctx)
	ar := hostarch.AddrRange{
		Start: addr,
		End:   addr + hostarch.PageSize,
	}
	prs, err := mm.Pin(ctx, ar, hostarch.Read, false /* ignorePermissions */)
	if err != nil {
		t.Fatalf("Pin got err %v want nil", err)
	}
	defer Unpin(prs)

	// The parent's writes must be visible through the pinned range, and must
	// not be visible to the child.
	postFork := bytes.Repeat([]byte{'B'}, hostarch.PageSize)
	if _, err := mm.CopyOut(ctx, addr, postFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}
	if got := readPinnedRange(t, prs[0]); !bytes.Equal(got, postFork) {
		t.Errorf("pinned page contains %q..., want %q...; pin did not unshare the copy-on-write page", got[:4], postFork[:4])
	}
	childBuf := make([]byte, hostarch.PageSize)
	if _, err := mm2.CopyIn(ctx, addr, childBuf, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyIn got err %v want nil", err)
	}
	if !bytes.Equal(childBuf, preFork) {
		t.Errorf("child read %q..., want %q...", childBuf[:4], preFork[:4])
	}
}

// TestUnpinnedPagesCopyOnWriteAfterFork tests that pages that were pinned but
// have been unpinned are once again made copy-on-write by Fork.
func TestUnpinnedPagesCopyOnWriteAfterFork(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	if _, err := mm.CopyOut(ctx, addr, bytes.Repeat([]byte{'A'}, hostarch.PageSize), usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	ar := hostarch.AddrRange{
		Start: addr,
		End:   addr + hostarch.PageSize,
	}
	prs, err := mm.Pin(ctx, ar, hostarch.ReadWrite, false /* ignorePermissions */)
	if err != nil {
		t.Fatalf("Pin got err %v want nil", err)
	}
	Unpin(prs)

	mm2, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork got err %v want nil", err)
	}
	defer mm2.DecUsers(ctx)

	// Since no pages remain pinned, all pages should be copy-on-write, not
	// copied for the child.
	mm.activeMu.RLock()
	pseg := mm.pmas.FindSegment(addr)
	if !pseg.Ok() {
		mm.activeMu.RUnlock()
		t.Fatalf("no pma for addr %#x", addr)
	}
	needCOW := pseg.ValuePtr().needCOW
	mm.activeMu.RUnlock()
	if !needCOW {
		t.Errorf("pma is not copy-on-write after fork of unpinned page")
	}
}

func TestMRemapMappableOffsetOverflow(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	mf := pgalloc.MemoryFileFromContext(ctx)
	fr, err := mf.Allocate(3*hostarch.PageSize, pgalloc.AllocOpts{})
	if err != nil {
		t.Fatalf("failed to allocate memory file range: %v", err)
	}
	mappable := NewSpecialMappable("test_mappable", mf, fr)
	defer mappable.DecRef(ctx)

	// Map a page at a high offset (2^64 - 2*PageSize).
	// This mapping itself is valid because offset + length (2^64 - PageSize) does not overflow.
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:          hostarch.PageSize,
		MappingIdentity: mappable,
		Mappable:        mappable,
		Offset:          ^uint64(0) - 2*hostarch.PageSize + 1,
		Private:         true,
		MaxPerms:        hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// 1. A shrink should succeed, even if offset+newSize overflows.
	// We remap with oldSize = 3*PageSize, newSize = 2*PageSize.
	if _, err := mm.MRemap(ctx, addr, 3*hostarch.PageSize, 2*hostarch.PageSize, MRemapOpts{}); err != nil {
		t.Errorf("MRemap shrink got err %v want nil", err)
	}

	// 2. A grow should fail if offset+newSize overflows.
	// We remap with oldSize = PageSize, newSize = 3*PageSize.
	if _, err := mm.MRemap(ctx, addr, hostarch.PageSize, 3*hostarch.PageSize, MRemapOpts{}); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("MRemap grow got err %v want EINVAL", err)
	}
}

// fillMappable is a memmap.Mappable, memmap.Filler and memmap.MappingIdentity
// over pages of a MemoryFile, whose Translate returns memmap.ErrFill for pages
// that Fill hasn't filled yet.
type fillMappable struct {
	t  *testing.T
	mm *MemoryManager
	mf *pgalloc.MemoryFile
	fr memmap.FileRange

	refs atomic.Int64

	mu sync.Mutex
	// fillPages is the number of missing pages that each Fill fills.
	fillPages int
	// fillErr is returned by Fill.
	fillErr error
	filled  []bool
	fills   int
}

// newFillMapping maps a fillMappable of pages pages, where page i holds byte
// i+1, shared and readable.
func newFillMapping(ctx context.Context, t *testing.T, mm *MemoryManager, pages int) (*fillMappable, hostarch.Addr) {
	mf := pgalloc.MemoryFileFromContext(ctx)
	buf := make([]byte, pages*hostarch.PageSize)
	for i := range buf {
		buf[i] = byte(i/hostarch.PageSize + 1)
	}
	reader := safemem.BlockSeqReader{Blocks: safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf))}
	fr, err := mf.Allocate(uint64(len(buf)), pgalloc.AllocOpts{
		Kind:       usage.Anonymous,
		Mode:       pgalloc.AllocateAndWritePopulate,
		ReaderFunc: reader.ReadToBlocks,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	t.Cleanup(func() { mf.DecRef(fr) })
	m := &fillMappable{t: t, mm: mm, mf: mf, fr: fr, filled: make([]bool, pages)}
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:          uint64(len(buf)),
		MappingIdentity: m,
		Mappable:        m,
		Perms:           hostarch.Read,
		MaxPerms:        hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap failed: %v", err)
	}
	return m, addr
}

func (m *fillMappable) IncRef()                    { m.refs.Add(1) }
func (m *fillMappable) DecRef(ctx context.Context) { m.refs.Add(-1) }
func (m *fillMappable) MappedName(ctx context.Context) string {
	return "fill"
}
func (m *fillMappable) DeviceID() uint64 { return 0 }
func (m *fillMappable) InodeID() uint64  { return 0 }
func (m *fillMappable) Msync(ctx context.Context, mr memmap.MappableRange) error {
	return nil
}
func (m *fillMappable) AddMapping(context.Context, memmap.MappingSpace, hostarch.AddrRange, uint64, bool) error {
	return nil
}
func (m *fillMappable) RemoveMapping(context.Context, memmap.MappingSpace, hostarch.AddrRange, uint64, bool) {
}
func (m *fillMappable) CopyMapping(context.Context, memmap.MappingSpace, hostarch.AddrRange, hostarch.AddrRange, uint64, bool) error {
	return nil
}
func (m *fillMappable) InvalidateUnsavable(context.Context) error { return nil }

// Translate implements memmap.Mappable.Translate.
func (m *fillMappable) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ts []memmap.Translation
	for off := required.Start; off < required.End; off += hostarch.PageSize {
		if !m.filled[off/hostarch.PageSize] {
			return ts, memmap.ErrFill
		}
		ts = append(ts, memmap.Translation{
			Source: memmap.MappableRange{Start: off, End: off + hostarch.PageSize},
			File:   m.mf,
			Offset: m.fr.Start + off,
			Perms:  hostarch.AnyAccess,
		})
	}
	return ts, nil
}

// Fill implements memmap.Filler.Fill.
func (m *fillMappable) Fill(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) error {
	// Fill may block on tasks that need the mm's locks.
	locked := make(chan struct{})
	go func() {
		m.mm.mappingMu.Lock()
		m.mm.activeMu.Lock()
		m.mm.activeMu.Unlock()
		m.mm.mappingMu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(10 * time.Second):
		m.t.Errorf("Fill(%v) called with mm locks held", required)
	}
	if m.refs.Load() < 2 {
		m.t.Errorf("Fill(%v) called without a MappingIdentity reference: refs %d", required, m.refs.Load())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fills++
	n := m.fillPages
	for off := required.Start; off < required.End && n > 0; off += hostarch.PageSize {
		if i := off / hostarch.PageSize; !m.filled[i] {
			m.filled[i] = true
			n--
		}
	}
	return m.fillErr
}

func TestCopyInFillsMappable(t *testing.T) {
	for _, test := range []struct {
		name      string
		prefilled int
		fillPages int
		wantFills int
	}{
		{name: "one fill", fillPages: 8, wantFills: 1},
		// Translations for the filled prefix must not cut the copy short.
		{name: "half filled", prefilled: 4, fillPages: 8, wantFills: 1},
		// Fills that each make progress aren't bounded by maxFillRetries.
		{name: "page per fill", fillPages: 1, wantFills: 8},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm := testMemoryManager(ctx, t)
			defer mm.DecUsers(ctx)
			m, addr := newFillMapping(ctx, t, mm, 8)
			for i := 0; i < test.prefilled; i++ {
				m.filled[i] = true
			}
			m.fillPages = test.fillPages

			buf := make([]byte, 8*hostarch.PageSize)
			n, err := mm.CopyIn(ctx, addr, buf, usermem.IOOpts{})
			if err != nil || n != len(buf) {
				t.Fatalf("CopyIn got (%d, %v) want (%d, nil)", n, err, len(buf))
			}
			for i, b := range buf {
				if want := byte(i/hostarch.PageSize + 1); b != want {
					t.Fatalf("byte %d got %d want %d", i, b, want)
				}
			}
			if m.fills != test.wantFills {
				t.Errorf("got %d fills want %d", m.fills, test.wantFills)
			}
			if got := m.refs.Load(); got != 1 {
				t.Errorf("MappingIdentity refs got %d want 1 (the vma's)", got)
			}
		})
	}
}

func TestCopyInFillFails(t *testing.T) {
	for _, test := range []struct {
		name      string
		fillPages int
		fillErr   error
		wantErr   error
		wantFills int
	}{
		{name: "no progress", wantErr: linuxerr.EFAULT, wantFills: maxFillRetries},
		{name: "server error", fillErr: linuxerr.EIO, wantErr: linuxerr.EFAULT, wantFills: 1},
		// The syscall is restarted after the signal is handled.
		{name: "interrupted", fillErr: linuxerr.ErrInterrupted, wantErr: linuxerr.ErrInterrupted, wantFills: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm := testMemoryManager(ctx, t)
			defer mm.DecUsers(ctx)
			m, addr := newFillMapping(ctx, t, mm, 1)
			m.fillErr = test.fillErr

			buf := make([]byte, hostarch.PageSize)
			n, err := mm.CopyIn(ctx, addr, buf, usermem.IOOpts{})
			if err != test.wantErr || n != 0 {
				t.Errorf("CopyIn got (%d, %v) want (0, %v)", n, err, test.wantErr)
			}
			if m.fills != test.wantFills {
				t.Errorf("got %d fills want %d", m.fills, test.wantFills)
			}
			if got := m.refs.Load(); got != 1 {
				t.Errorf("MappingIdentity refs got %d want 1 (the vma's)", got)
			}
		})
	}
}
