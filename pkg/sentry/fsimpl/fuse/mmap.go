// Copyright 2026 The gVisor Authors.
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

package fuse

import (
	"io"
	"math"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

// Memory mappings of FUSE regular files.
//
// Mapped file contents are cached per inode in inode.cache (pages of the
// sentry MemoryFile), in the style of Linux's FUSE page cache and of gofer's
// cache. Only mappings use the cache: read(2) and write(2) keep going to the
// server, which stays authoritative for everything except dirty mapped pages.
//
// The FUSE server is a task in the same sandbox, and its syscalls take
// kernel.TaskSet.mu. Translate runs under mm.MemoryManager.activeMu, which
// ranks below TaskSet.mu, so a Translate that waits for the server - directly
// or through a lock held across a server request - can deadlock (observed
// with git gc: a task holding TaskSet.mu waits for the faulting mm's activeMu).
// Hence:
//
//   - Translate never sends requests; it only serves cached pages and takes
//     only dataMu. For uncached pages within EOF it returns memmap.ErrFill.
//   - mm then releases its locks and calls Fill, which reads the missing pages
//     (plus a read-around window, like Linux's mmap readahead) from the server
//     with the faulting FD's handle, and retries Translate.
//   - dataMu is never held across a server request. Fills and writebacks are
//     serialized by ioMu and copy data with dataMu released.
//   - Clean cached pages are reread in place when the cache is revalidated on
//     open.
//
// Coherence rules:
//
//   - write(2) is sent to the server and then copied into any cached pages it
//     overlaps, so mappings observe it immediately.
//   - read(2) first writes back dirty pages it overlaps, so it observes stores
//     made through shared mappings.
//   - Dirty pages are written back with FUSE_WRITE on msync(MS_SYNC), fsync,
//     close (before FUSE_FLUSH), when the last mapping of a range goes away
//     (as Linux's fuse_vma_close()), on eviction, and on release of the last
//     shared-writable FD.
//   - Truncation, including a smaller size reported by the server, drops the
//     cache beyond EOF and invalidates mappings there (SIGBUS on access).
//   - An open reply without FOPEN_KEEP_CACHE writes back dirty pages, drops
//     unmapped cached pages and rereads mapped clean pages in place (Linux's
//     fuse_finish_open() => invalidate_inode_pages2()).
//   - Unmapped cached pages are evictable (gofer's inode.Evict).
//
// The cache lives in the MemoryFile, which is saved with the sandbox, so
// cached and dirty pages survive checkpoint/restore without talking to the
// (paused, in-sandbox) server.
//
// Each regularFileFD is the memmap.Mappable of its own mappings, so fills use
// that FD's FUSE file handle as Linux uses vma->vm_file. The VMA holds a
// reference on the FD (MappingIdentity), keeping the handle open.
//
// Lock order: inode.attrMu > inode.mapsMu > inode.ioMu > inode.dataMu.
//
// ponytail: file contents written by someone other than this sandbox's VFS
// (e.g. the server itself) are not coherent with pages already cached: the
// zeroes past the old EOF in a cached last page are kept when the file grows,
// and writably mapped pages are not revalidated on open. The FUSE server must
// be the only writer, as for AgentFS; FUSE_NOTIFY_INVAL_INODE would lift this.

var (
	_ memmap.Mappable             = (*regularFileFD)(nil)
	_ memmap.Filler               = (*regularFileFD)(nil)
	_ pgalloc.EvictableMemoryUser = (*inode)(nil)
)

// fillAround is the aligned window that Fill reads around a missing page, as
// Linux's filemap_fault() reads around a fault (ra_pages, which FUSE sets from
// the negotiated max_readahead).
const fillAround = fuseDefaultMaxReadahead

// wholeFile covers every page-aligned file offset.
var wholeFile = memmap.MappableRange{Start: 0, End: hostarch.PageRoundDown(uint64(math.MaxInt64))}

// ConfigureMMap implements vfs.FileDescriptionImpl.ConfigureMMap.
func (fd *regularFileFD) ConfigureMMap(ctx context.Context, opts *memmap.MMapOpts) error {
	i := fd.inode()
	if i.fs.mf == nil {
		return linuxerr.ENODEV
	}
	// Like Linux without FUSE_DIRECT_IO_ALLOW_MMAP: direct_io files only
	// support private mappings.
	if fd.DirectIO && !opts.Private {
		return linuxerr.ENODEV
	}
	i.dataMu.Lock()
	if !opts.Private && opts.MaxPerms.Write && !fd.writer {
		// This FD's handle may be used to write back pages dirtied through
		// any shared mapping (Linux's fuse_link_write_file()).
		fd.writer = true
		i.writers = append(i.writers, fd)
	}
	i.dataMu.Unlock()
	return vfs.GenericConfigureMMap(&fd.vfsfd, fd, opts)
}

// AddMapping implements memmap.Mappable.AddMapping.
func (fd *regularFileFD) AddMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) error {
	i := fd.inode()
	i.mapsMu.Lock()
	defer i.mapsMu.Unlock()
	mapped := i.mappings.AddMapping(ms, ar, offset, writable)
	// As gofer's, Evict only drops unmapped pages, so that it needn't
	// invalidate translations.
	for _, r := range mapped {
		i.fs.mf.MarkUnevictable(i, pgalloc.EvictableRange{Start: r.Start, End: r.End})
	}
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (fd *regularFileFD) RemoveMapping(ctx context.Context, ms memmap.MappingSpace, ar hostarch.AddrRange, offset uint64, writable bool) {
	i := fd.inode()
	i.mapsMu.Lock()
	defer i.mapsMu.Unlock()
	unmapped := i.mappings.RemoveMapping(ms, ar, offset, writable)
	// Hold ioMu from AllowClean through writeback, so that a concurrent
	// writeback can't mark stores made through this mapping clean.
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	for _, r := range unmapped {
		// No longer mapped, so no longer dirtyable through a mapping.
		i.dataMu.Lock()
		i.dirty.AllowClean(r)
		i.dataMu.Unlock()
		// Linux's fuse_vma_close() writes back on every unmap. On failure
		// keep the dirty pages for a later fsync/close/release/eviction.
		if err := i.writebackLocked(ctx, r); err != nil {
			log.Warningf("fuse: writeback of unmapped range %v failed: %v", r, err)
		}
		i.fs.mf.MarkEvictable(i, pgalloc.EvictableRange{Start: r.Start, End: r.End})
	}
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (fd *regularFileFD) CopyMapping(ctx context.Context, ms memmap.MappingSpace, srcAR, dstAR hostarch.AddrRange, offset uint64, writable bool) error {
	return fd.AddMapping(ctx, ms, dstAR, offset, writable)
}

// Translate implements memmap.Mappable.Translate.
func (fd *regularFileFD) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	i := fd.inode()
	i.dataMu.Lock()
	defer i.dataMu.Unlock()

	// Constrain translations to the file size (rounded up) to prevent
	// translation to pages that may be concurrently truncated.
	pgend, _ := hostarch.PageRoundUp(i.size.Load())
	var beyondEOF bool
	if required.End > pgend {
		if required.Start >= pgend {
			return nil, &memmap.BusError{Err: io.EOF}
		}
		beyondEOF = true
		required.End = pgend
	}
	if optional.End > pgend {
		optional.End = pgend
	}

	var ts []memmap.Translation
	translatedEnd := required.Start
	for seg := i.cache.FindSegment(required.Start); seg.Ok() && seg.Start() <= translatedEnd && seg.Start() < required.End; seg, _ = seg.NextNonEmpty() {
		segMR := seg.Range().Intersect(optional)
		perms := hostarch.ReadExecute
		if at.Write {
			// From here on the pages can be dirtied through the mapping.
			i.dirty.KeepDirty(segMR)
			perms.Write = true
		}
		ts = append(ts, memmap.Translation{
			Source: segMR,
			File:   i.fs.mf,
			Offset: seg.FileRangeOf(segMR).Start,
			Perms:  perms,
		})
		translatedEnd = segMR.End
	}
	if translatedEnd < required.End {
		// Read the missing pages in Fill, without mm locks.
		return ts, memmap.ErrFill
	}
	if beyondEOF {
		return ts, &memmap.BusError{Err: io.EOF}
	}
	return ts, nil
}

// Fill implements memmap.Filler.Fill. It reads the uncached pages of required
// within EOF, and those of the fillAround-aligned window around it within
// optional.
func (fd *regularFileFD) Fill(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) error {
	mr := memmap.MappableRange{Start: required.Start &^ (fillAround - 1), End: (required.End + fillAround - 1) &^ (fillAround - 1)}
	if mr.End < required.End {
		mr.End = required.End // overflow
	}
	mr = mr.Intersect(optional)
	i := fd.inode()
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	return i.fillLocked(ctx, fd, mr)
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
//
// Translations point into the MemoryFile, which is saved; there is nothing
// to do (and the in-sandbox server is paused, so no writeback is possible).
func (fd *regularFileFD) InvalidateUnsavable(ctx context.Context) error {
	return nil
}

// Evict implements pgalloc.EvictableMemoryUser.Evict.
func (i *inode) Evict(ctx context.Context, er pgalloc.EvictableRange) {
	mr := memmap.MappableRange{Start: er.Start, End: er.End}
	i.mapsMu.Lock()
	defer i.mapsMu.Unlock()
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	// Only pages that are no longer mapped may be evicted.
	for mgap := i.mappings.LowerBoundGap(mr.Start); mgap.Ok() && mgap.Start() < mr.End; mgap = mgap.NextGap() {
		gr := mgap.Range().Intersect(mr)
		if gr.Length() == 0 {
			continue
		}
		if err := i.writebackLocked(ctx, gr); err != nil {
			log.Warningf("fuse: writeback of evicted range %v failed: %v", gr, err)
			continue
		}
		i.dataMu.Lock()
		i.cache.Drop(gr, i.fs.mf)
		i.dirty.KeepClean(gr)
		i.dataMu.Unlock()
	}
}

// fillLocked reads the uncached pages of mr within EOF from the server with
// fd's handle and inserts them into the cache.
//
// Preconditions: i.ioMu is locked; i.dataMu is not. The caller must not hold
// mm locks (see the file comment).
func (i *inode) fillLocked(ctx context.Context, fd *regularFileFD, mr memmap.MappableRange) error {
	size := i.size.Load()
	pgend, _ := hostarch.PageRoundUp(size)
	if mr.End > pgend {
		mr.End = pgend
	}
	if mr.Start >= mr.End {
		return nil
	}
	i.dataMu.Lock()
	var gaps []memmap.MappableRange
	for gap := i.cache.LowerBoundGap(mr.Start); gap.Ok() && gap.Start() < mr.End; gap = gap.NextGap() {
		if gr := gap.Range().Intersect(mr); gr.Length() != 0 {
			gaps = append(gaps, gr)
		}
	}
	i.dataMu.Unlock()
	for _, gr := range gaps {
		// Fill a private set without dataMu (fills and removals are
		// serialized by ioMu), then move its pages into the cache.
		var tmp fsutil.FileRangeSet
		_, err := tmp.Fill(ctx, gr, gr, size, i.fs.mf, pgalloc.AllocOpts{
			Kind:    usage.PageCache,
			MemCgID: pgalloc.MemoryCgroupIDFromContext(ctx),
			Mode:    pgalloc.AllocateAndWritePopulate,
		}, fd.readToBlocksAt)
		i.dataMu.Lock()
		for seg := tmp.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			i.cache.InsertRange(seg.Range(), seg.Value())
		}
		i.dataMu.Unlock()
		// Fill zero-fills a short read (the server's EOF).
		if err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

// readToBlocksAt reads from the server into dsts using fd's handle.
func (fd *regularFileFD) readToBlocksAt(ctx context.Context, dsts safemem.BlockSeq, off uint64) (uint64, error) {
	var done uint64
	for !dsts.IsEmpty() {
		n := dsts.NumBytes()
		if max := uint64(fd.inode().fs.conn.maxRead); n > max {
			n = max
		}
		bufs, got, err := fd.inode().fs.readPages(ctx, fd, off+done, uint32(n))
		for _, b := range bufs {
			c, cerr := safemem.CopySeq(dsts, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(b)))
			done += c
			dsts = dsts.DropFirst64(c)
			if cerr != nil {
				return done, cerr
			}
		}
		if err != nil {
			return done, err
		}
		if uint64(got) < n {
			return done, io.EOF
		}
	}
	return done, nil
}

// writebackLocked writes dirty cached pages in mr back to the server.
//
// Preconditions: i.ioMu is locked; i.dataMu is not.
func (i *inode) writebackLocked(ctx context.Context, mr memmap.MappableRange) error {
	type piece struct {
		mr memmap.MappableRange
		fr memmap.FileRange
	}
	var pieces []piece
	i.dataMu.Lock()
	if i.dirty.IsEmpty() {
		i.dataMu.Unlock()
		return nil
	}
	if len(i.writers) == 0 {
		i.dataMu.Unlock()
		// Unreachable: only shared mappings of writable FDs dirty pages, and
		// those FDs write back before leaving i.writers.
		return linuxerr.EIO
	}
	w := i.writers[0]
	size := i.size.Load()
	for dseg := i.dirty.LowerBoundSegment(mr.Start); dseg.Ok() && dseg.Start() < mr.End; dseg = dseg.NextSegment() {
		dr := dseg.Range().Intersect(mr)
		for cseg := i.cache.LowerBoundSegment(dr.Start); cseg.Ok() && cseg.Start() < dr.End; cseg = cseg.NextSegment() {
			r := cseg.Range().Intersect(dr)
			if r.Start >= size {
				break
			}
			if r.End > size {
				r.End = size
			}
			pieces = append(pieces, piece{r, cseg.FileRangeOf(r)})
		}
	}
	i.dataMu.Unlock()
	// ioMu keeps the pages cached; stores through writable mappings may race
	// with the copy, but those ranges stay dirty (KeepDirty).
	for _, p := range pieces {
		ims, err := i.fs.mf.MapInternal(p.fr, hostarch.Read)
		if err != nil {
			return err
		}
		buf := make([]byte, p.mr.Length())
		if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), ims); err != nil {
			return err
		}
		if _, err := i.fs.writeBytes(ctx, w, p.mr.Start, buf); err != nil {
			return err
		}
	}
	i.dataMu.Lock()
	i.dirty.MarkClean(mr)
	i.dataMu.Unlock()
	return nil
}

// writebackAll writes back all dirty cached pages.
func (i *inode) writebackAll(ctx context.Context) error {
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	return i.writebackLocked(ctx, wholeFile)
}

// writebackRange writes back dirty cached pages overlapping [off, off+n).
func (i *inode) writebackRange(ctx context.Context, off, n uint64) error {
	i.dataMu.Lock()
	clean := i.dirty.IsEmpty()
	i.dataMu.Unlock()
	if clean {
		return nil
	}
	end, ok := hostarch.PageRoundUp(off + n)
	if !ok {
		end = wholeFile.End
	}
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	return i.writebackLocked(ctx, memmap.MappableRange{Start: hostarch.PageRoundDown(off), End: end})
}

// updateCache copies data just written to the server at off into overlapping
// cached pages, so that mappings see write(2).
func (i *inode) updateCache(off uint64, data []byte) {
	i.dataMu.Lock()
	defer i.dataMu.Unlock()
	end := off + uint64(len(data))
	for seg := i.cache.LowerBoundSegment(hostarch.PageRoundDown(off)); seg.Ok() && seg.Start() < end; seg = seg.NextSegment() {
		r := seg.Range().Intersect(memmap.MappableRange{Start: off, End: end})
		if r.Length() == 0 {
			continue
		}
		ims, err := i.fs.mf.MapInternal(seg.FileRangeOf(r), hostarch.Write)
		if err != nil {
			log.Warningf("fuse: updating cached pages %v: %v", r, err)
			continue
		}
		safemem.CopySeq(ims, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data[r.Start-off:r.End-off])))
	}
}

// truncateCache drops cached pages past a new, smaller size and invalidates
// their mappings.
//
// Preconditions: i.attrMu is locked; mapsMu, ioMu and dataMu are not.
func (i *inode) truncateCache(oldSize, newSize uint64) {
	if newSize >= oldSize || i.fs.mf == nil {
		return
	}
	oldpgend, _ := hostarch.PageRoundUp(oldSize)
	newpgend, _ := hostarch.PageRoundUp(newSize)
	i.mapsMu.Lock()
	defer i.mapsMu.Unlock()
	if oldpgend != newpgend {
		i.mappings.Invalidate(memmap.MappableRange{Start: newpgend, End: oldpgend}, memmap.InvalidateOpts{
			// Compare Linux's mm/truncate.c:truncate_setsize() =>
			// truncate_pagecache() => unmap_mapping_range(evencows=1).
			InvalidatePrivate: true,
		})
	}
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	i.dataMu.Lock()
	defer i.dataMu.Unlock()
	i.cache.Truncate(newSize, i.fs.mf)
	i.dirty.KeepClean(memmap.MappableRange{Start: newSize, End: oldpgend})
}

// revalidateCache implements an open reply without FOPEN_KEEP_CACHE (Linux's
// invalidate_inode_pages2()): write back dirty pages, drop unmapped cached
// pages and reread mapped clean pages in place, so mappings observe changes
// made behind the sentry's back. Mapped pages aren't dropped: Translate takes
// only dataMu, so it could hand out a page between Invalidate and Drop.
//
// Preconditions: i.attrMu is locked; mapsMu, ioMu and dataMu are not.
func (i *inode) revalidateCache(ctx context.Context, fd *regularFileFD) {
	i.mapsMu.Lock()
	defer i.mapsMu.Unlock()
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	i.dataMu.Lock()
	empty := i.cache.IsEmpty()
	i.dataMu.Unlock()
	if empty {
		return
	}
	if err := i.writebackLocked(ctx, wholeFile); err != nil {
		log.Warningf("fuse: writeback before cache revalidation failed: %v", err)
		return
	}
	var reread []memmap.MappableRange
	i.dataMu.Lock()
	for mgap := i.mappings.FirstGap(); mgap.Ok(); mgap = mgap.NextGap() {
		// Empty gaps occur between adjacent mappings; Drop of an empty range
		// would remove the segment that starts there.
		if mgap.Range().Length() != 0 {
			i.cache.Drop(mgap.Range(), i.fs.mf)
		}
	}
	for cseg := i.cache.FirstSegment(); cseg.Ok(); cseg = cseg.NextSegment() {
		// Skip pages kept dirty by writable mappings: rereading could lose
		// concurrent stores.
		for dgap := i.dirty.LowerBoundGap(cseg.Start()); dgap.Ok() && dgap.Start() < cseg.End(); dgap = dgap.NextGap() {
			if r := dgap.Range().Intersect(cseg.Range()); r.Length() != 0 {
				reread = append(reread, r)
			}
		}
	}
	i.dataMu.Unlock()
	for _, r := range reread {
		buf := make([]byte, r.Length())
		n, err := fd.readToBlocksAt(ctx, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), r.Start)
		if err != nil && err != io.EOF {
			log.Warningf("fuse: rereading cached range %v: %v", r, err)
			continue
		}
		clear(buf[n:])
		i.updateCache(r.Start, buf)
	}
}

// releaseCache frees the cache of an inode that is being destroyed.
func (i *inode) releaseCache() {
	if i.fs.mf == nil {
		return
	}
	i.fs.mf.MarkAllUnevictable(i)
	i.ioMu.Lock()
	defer i.ioMu.Unlock()
	i.dataMu.Lock()
	defer i.dataMu.Unlock()
	if !i.dirty.IsEmpty() {
		log.Warningf("fuse: dropping dirty pages of destroyed inode %d", i.nodeID)
	}
	i.cache.DropAll(i.fs.mf)
	i.dirty.RemoveAllAndAccount()
}

// Release implements vfs.FileDescriptionImpl.Release.
func (fd *regularFileFD) Release(ctx context.Context) {
	i := fd.inode()
	if fd.writer {
		i.ioMu.Lock()
		i.dataMu.Lock()
		last := len(i.writers) == 1
		i.dataMu.Unlock()
		// If this is the last handle that can write back, do it now.
		if last {
			if err := i.writebackLocked(ctx, wholeFile); err != nil {
				log.Warningf("fuse: writeback on release failed, dropping dirty pages: %v", err)
				i.dataMu.Lock()
				i.dirty.RemoveAllAndAccount()
				i.dataMu.Unlock()
			}
		}
		i.dataMu.Lock()
		i.writers = removeFD(i.writers, fd)
		i.dataMu.Unlock()
		i.ioMu.Unlock()
	}
	fd.fileDescription.Release(ctx)
}

func removeFD(fds []*regularFileFD, fd *regularFileFD) []*regularFileFD {
	for j, f := range fds {
		if f == fd {
			return append(fds[:j], fds[j+1:]...)
		}
	}
	return fds
}

// OnClose implements vfs.FileDescriptionImpl.OnClose.
func (fd *regularFileFD) OnClose(ctx context.Context) error {
	// As Linux's fuse_flush(): write back before FUSE_FLUSH.
	if err := fd.inode().writebackAll(ctx); err != nil {
		return err
	}
	return fd.fileDescription.OnClose(ctx)
}

// Sync implements vfs.FileDescriptionImpl.Sync. It also implements msync(2)
// with MS_SYNC through vfs.FileDescription.Msync.
func (fd *regularFileFD) Sync(ctx context.Context, opts vfs.SyncOptions) error {
	if err := fd.inode().writebackAll(ctx); err != nil {
		return err
	}
	return fd.fileDescription.Sync(ctx, opts)
}
