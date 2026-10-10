// Copyright 2020 The gVisor Authors.
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
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/usermem"
)

// +stateify savable
type directoryFD struct {
	fileDescription
}

// Allocate implements directoryFD.Allocate.
func (*directoryFD) Allocate(ctx context.Context, mode, offset, length uint64) error {
	return linuxerr.EISDIR
}

// PRead implements vfs.FileDescriptionImpl.PRead.
func (*directoryFD) PRead(ctx context.Context, dst usermem.IOSequence, offset int64, opts vfs.ReadOptions) (int64, error) {
	return 0, linuxerr.EISDIR
}

// Read implements vfs.FileDescriptionImpl.Read.
func (*directoryFD) Read(ctx context.Context, dst usermem.IOSequence, opts vfs.ReadOptions) (int64, error) {
	return 0, linuxerr.EISDIR
}

// PWrite implements vfs.FileDescriptionImpl.PWrite.
func (*directoryFD) PWrite(ctx context.Context, src usermem.IOSequence, offset int64, opts vfs.WriteOptions) (int64, error) {
	return 0, linuxerr.EISDIR
}

// Write implements vfs.FileDescriptionImpl.Write.
func (*directoryFD) Write(ctx context.Context, src usermem.IOSequence, opts vfs.WriteOptions) (int64, error) {
	return 0, linuxerr.EISDIR
}

// OnClose implements vfs.FileDescriptionImpl.OnClose.
func (*directoryFD) OnClose(ctx context.Context) error {
	return nil
}

// Seek implements vfs.FileDescriptionImpl.Seek.
func (dir *directoryFD) Seek(ctx context.Context, offset int64, whence int32) (int64, error) {
	switch whence {
	case linux.SEEK_SET:
		if offset < 0 {
			return 0, linuxerr.EINVAL
		}
		dir.off.Store(offset)
		return offset, nil
	case linux.SEEK_CUR:
		newOffset := dir.off.Load() + offset
		if newOffset < 0 {
			return 0, linuxerr.EINVAL
		}
		dir.off.Store(newOffset)
		return newOffset, nil
	default:
		return 0, linuxerr.EINVAL
	}
}

// IterDirents implements vfs.FileDescriptionImpl.IterDirents.
//
// If the directory was opened with FOPEN_CACHE_DIR (the default when the
// server does not implement FUSE_OPENDIR), entries are served from the
// inode's listing cache, as Linux's fuse_readdir_cached() does.
func (dir *directoryFD) IterDirents(ctx context.Context, callback vfs.IterDirentsCallback) error {
	i := dir.inode()
	off := dir.off.Load()
	var (
		ents   []vfs.Dirent
		cached bool
		err    error
	)
	if dir.OpenFlag&linux.FOPEN_CACHE_DIR != 0 {
		if ents, cached, err = i.cachedDirents(ctx, dir, off == 0); err != nil {
			return err
		}
		if cached {
			var start int
			if start, cached = direntsFrom(ents, off); cached {
				ents = ents[start:]
			}
		}
	}
	if !cached {
		// TODO(gVisor.dev/issue/3404): Support FUSE_READDIRPLUS.
		if ents, err = i.readDirents(ctx, dir, uint64(off), linux.FUSE_PAGE_SIZE); err != nil {
			return err
		}
	}
	for _, dirent := range ents {
		if err := callback.Handle(dirent); err != nil {
			return err
		}
		dir.off.Store(dirent.NextOff)
	}
	return nil
}

// readDirents sends one FUSE_READDIR at offset off.
func (i *inode) readDirents(ctx context.Context, dir *directoryFD, off uint64, size uint32) ([]vfs.Dirent, error) {
	in := linux.FUSEReadIn{
		Fh:     dir.Fh,
		Offset: off,
		Size:   size,
		Flags:  dir.statusFlags(),
	}
	var out linux.FUSEDirents
	if err := i.call(ctx, linux.FUSE_READDIR, &in, &out); err != nil {
		return nil, err
	}
	ents := make([]vfs.Dirent, 0, len(out.Dirents))
	for _, fuseDirent := range out.Dirents {
		if len(fuseDirent.Name) == 0 {
			return nil, linuxerr.EIO
		}
		ents = append(ents, vfs.Dirent{
			Name:    fuseDirent.Name,
			Type:    uint8(fuseDirent.Meta.Type),
			Ino:     fuseDirent.Meta.Ino,
			NextOff: int64(fuseDirent.Meta.Off),
		})
	}
	return ents, nil
}

// cachedDirents returns the directory's complete listing from the cache,
// first reading it from the server if the cache is empty or, when restart is
// true (offset 0, i.e. after opendir(3) or rewinddir(3)), stale because the
// directory's mtime changed. ok is false if a listing that didn't start at
// offset 0 must continue uncached.
func (i *inode) cachedDirents(ctx context.Context, dir *directoryFD, restart bool) (ents []vfs.Dirent, ok bool, err error) {
	i.attrMu.Lock()
	mtime := i.mtime.Load()
	i.attrMu.Unlock()
	i.dirMu.Lock()
	if i.direntsOK && (!restart || i.direntsMtime == mtime) {
		ents = i.dirents
		i.dirMu.Unlock()
		return ents, true, nil
	}
	version := i.dirVersion
	i.dirMu.Unlock()
	if !restart {
		return nil, false, nil
	}

	// Read the whole listing, in requests as large as reads may be.
	size := uint32(i.fs.conn.maxPages) << hostarch.PageShift
	if size > i.fs.conn.maxRead {
		size = i.fs.conn.maxRead
	}
	if size < linux.FUSE_PAGE_SIZE {
		size = linux.FUSE_PAGE_SIZE
	}
	var off uint64
	for {
		got, err := i.readDirents(ctx, dir, off, size)
		if err != nil {
			return nil, false, err
		}
		if len(got) == 0 {
			break
		}
		next := uint64(got[len(got)-1].NextOff)
		if next == off {
			return nil, false, linuxerr.EIO // the server's offsets don't advance
		}
		ents, off = append(ents, got...), next
	}
	i.dirMu.Lock()
	// Don't cache a listing that may predate a concurrent change.
	if i.dirVersion == version {
		i.dirents, i.direntsOK, i.direntsMtime = ents, true, mtime
	}
	i.dirMu.Unlock()
	return ents, true, nil
}

// direntsFrom returns the index in ents of the entry at directory offset off.
func direntsFrom(ents []vfs.Dirent, off int64) (int, bool) {
	if off == 0 {
		return 0, true
	}
	for k, d := range ents {
		if d.NextOff == off {
			return k + 1, true
		}
	}
	return 0, false
}

// dirChanged drops the listing cache and any negative entry for name after
// a change to the directory's entry name made through this filesystem
// (Linux's fuse_dir_changed() and d_instantiate()).
func (i *inode) dirChanged(name string) {
	i.dirMu.Lock()
	defer i.dirMu.Unlock()
	i.dirVersion++
	i.dirents, i.direntsOK = nil, false
	if _, ok := i.negative[name]; ok {
		delete(i.negative, name)
		i.fs.negatives.Add(-1)
	}
}
