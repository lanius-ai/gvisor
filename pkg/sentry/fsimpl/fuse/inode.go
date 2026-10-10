// Copyright 2022 The gVisor Authors.
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
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/marshal"
	"gvisor.dev/gvisor/pkg/marshal/primitive"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/fsutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
)

// +stateify savable
type fileHandle struct {
	new    bool
	handle uint64
	flags  uint32
}

// inode implements kernfs.Inode.
//
// +stateify savable
type inode struct {
	inodeRefs
	kernfs.InodeNotAnonymous
	kernfs.InodeNotSymlink
	kernfs.InodeWatches
	kernfs.OrderedChildren
	kernfs.InodeFSOwned

	// the owning filesystem. fs is immutable.
	fs *filesystem

	// nodeID is a unique id which identifies the inode between userspace and
	// the sentry. generation is used to distinguish inodes in case of nodeID
	// reuse. Both are immutable.
	nodeID     uint64
	generation uint64

	// entryTime is the time at which the entry must be revalidated. Reading
	// entryTime requires either using entryTimeSeq and SeqAtomicLoadTime, or
	// that attrMu is locked. Writing entryTime requires that attrMu is locked
	// and that entryTimeSeq is in a writer critical section.
	entryTimeSeq sync.SeqCount `state:"nosave"`
	entryTime    ktime.Time

	// attrVersion is the version of the last attribute change.
	attrVersion atomicbitops.Uint64

	// attrTime is the time at which the attributes become invalid.
	attrTime ktime.Time

	// link is result of following a symbolic link.
	link string

	// fh caches the file handle returned by the server from a FUSE_CREATE request
	// so we don't have to send a separate FUSE_OPEN request.
	fh fileHandle

	locks   vfs.FileLocks
	watches vfs.Watches

	// attrMu protects the attributes of this inode.
	attrMu inodeAttrMutex `state:"nosave"`

	// +checklocks:attrMu
	ino atomicbitops.Uint64 // Stat data, not accessed for path walking.
	// +checklocks:attrMu
	uid atomicbitops.Uint32 // auth.KUID, but stored as raw uint32 for sync/atomic.
	// +checklocks:attrMu
	gid atomicbitops.Uint32 // auth.KGID, but...
	// +checklocks:attrMu
	mode atomicbitops.Uint32 // File type and mode.

	// Timestamps in nanoseconds from the unix epoch.
	// +checklocks:attrMu
	atime atomicbitops.Int64
	// +checklocks:attrMu
	mtime atomicbitops.Int64
	// +checklocks:attrMu
	ctime atomicbitops.Int64

	// +checklocks:attrMu
	size atomicbitops.Uint64

	// nlink counts the number of hard links to this inode. It's updated and
	// accessed used atomic operations but not protected by attrMu.
	nlink atomicbitops.Uint32

	// +checklocks:attrMu
	blockSize atomicbitops.Uint32 // 0 if unknown.

	// The fields below implement memory mappings of regular files; see
	// mmap.go.

	// mapsMu protects mappings.
	mapsMu inodeMapsMutex `state:"nosave"`

	// mappings tracks mappings of the file into memmap.MappingSpaces.
	//
	// +checklocks:mapsMu
	mappings memmap.MappingSet

	// ioMu serializes filling, writing back and dropping cached pages, which
	// happen with dataMu released (see mmap.go).
	ioMu inodeIOMutex `state:"nosave"`

	// dataMu protects cache, dirty and writers. It is never held
	// across a request to the server.
	dataMu inodeDataMutex `state:"nosave"`

	// cache maps file offsets to fs.mf pages holding mapped file contents.
	//
	// +checklocks:dataMu
	cache fsutil.FileRangeSet

	// dirty tracks cached pages that must be written back to the server.
	//
	// +checklocks:dataMu
	dirty fsutil.DirtySet

	// writers are writable FDs that were mapped shared; any of their handles
	// can write back dirty pages (Linux's fuse_inode.write_files).
	//
	// +checklocks:dataMu
	writers []*regularFileFD

	// The fields below cache a directory's listing (FOPEN_CACHE_DIR, see
	// directory.go) and the names known to be absent from it. They aren't
	// saved: a restored sandbox asks the server again.

	// dirMu protects the directory caches. It is never held across a
	// request to the server.
	dirMu sync.Mutex `state:"nosave"`

	// dirents is the directory's complete listing if direntsOK, read when
	// the directory's mtime was direntsMtime.
	//
	// +checklocks:dirMu
	dirents []vfs.Dirent `state:"nosave"`
	// +checklocks:dirMu
	direntsOK bool `state:"nosave"`
	// +checklocks:dirMu
	direntsMtime int64 `state:"nosave"`

	// dirVersion counts changes to the directory's entries made through this
	// filesystem, which drop the listing.
	//
	// +checklocks:dirMu
	dirVersion uint64 `state:"nosave"`

	// negative maps names that the server reported absent with a negative
	// entry (a FUSE_LOOKUP reply with nodeid 0) to when that expires.
	//
	// +checklocks:dirMu
	negative map[string]ktime.Time `state:"nosave"`
}

func (i *inode) Mode() linux.FileMode {
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	return i.filemode()
}

func (i *inode) UID() auth.KUID {
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	return auth.KUID(i.uid.Load())
}

func (i *inode) GID() auth.KGID {
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	return auth.KGID(i.gid.Load())
}

// +checklocks:i.attrMu
func (i *inode) filemode() linux.FileMode {
	return linux.FileMode(i.mode.Load())
}

// touchCMTime updates the ctime and mtime attributes to be the current time.
//
// +checklocks:i.attrMu
func (i *inode) touchCMtime() {
	now := i.fs.clock.Now().Nanoseconds()
	i.mtime.Store(now)
	i.ctime.Store(now)
}

// touchAtime updates the atime attribute to be the current time.
//
// +checklocks:i.attrMu
func (i *inode) touchAtime() {
	i.atime.Store(i.fs.clock.Now().Nanoseconds())
}

// Precondition: isValidType(mode) == true.
// +checklocks:i.attrMu
func (i *inode) init(creds *auth.Credentials, devMajor, devMinor uint32, nodeid uint64, mode linux.FileMode, nlink uint32) {
	i.nodeID = nodeid
	i.ino.Store(nodeid)
	i.mode.Store(uint32(mode))
	i.uid.Store(uint32(creds.EffectiveKUID))
	i.gid.Store(uint32(creds.EffectiveKGID))
	i.nlink.Store(nlink)
	i.blockSize.Store(4096) // Default block size.

	now := i.fs.clock.Now().Nanoseconds()
	i.atime.Store(now)
	i.mtime.Store(now)
	i.ctime.Store(now)
}

// DecRef implements kernfs.Inode.DecRef.
func (i *inode) DecRef(ctx context.Context) {
	i.inodeRefs.DecRef(func() {
		i.releaseCache()
		i.dropDirCaches()
		i.Destroy(ctx)
	})
}

func pidFromContext(ctx context.Context) uint32 {
	kernelTask := kernel.TaskFromContext(ctx)
	if kernelTask == nil {
		return 0
	}
	return uint32(kernelTask.ThreadID())
}

func umaskFromContext(ctx context.Context) uint32 {
	kernelTask := kernel.TaskFromContext(ctx)
	umask := uint32(0)
	if kernelTask != nil {
		umask = uint32(kernelTask.FSContext().Umask())
	}
	return umask
}

// maxValidSec caps entry_valid/attr_valid timeouts (about 68 years), as
// Linux's fuse_time_to_jiffies() caps them at MAX_JIFFY_OFFSET.
const maxValidSec = 1 << 31

// validTimeout converts an entry_valid/attr_valid timeout. The protocol's
// timeouts are unsigned; callers pass them as int64, so huge values (e.g.
// Rust's Duration::MAX from fuser servers) arrive negative and used to
// expire immediately, turning every stat and path walk into a round trip.
func validTimeout(sec, nsec int64) ktime.Time {
	if sec < 0 || sec > maxValidSec {
		sec, nsec = maxValidSec, 0
	}
	return ktime.FromTimespec(linux.Timespec{Sec: sec, Nsec: nsec})
}

// +checklocks:i.attrMu
func (i *inode) updateEntryTime(entrySec, entryNSec int64) {
	entryTime := validTimeout(entrySec, entryNSec)
	SeqAtomicStoreTime(&i.entryTimeSeq, &i.entryTime, i.fs.clock.Now().AddTime(entryTime))
}

func (i *inode) allowCredentials(creds *auth.Credentials) bool {
	// Since FUSE operations are ultimately backed by a userspace process (the
	// fuse daemon), allowing a process to call into fusefs grants the daemon
	// ptrace-like capabilities over the calling process. Because of this, by
	// default FUSE only allows the mount owner to interact with the
	// filesystem. This explicitly excludes setuid/setgid processes.
	//
	// This behaviour can be overridden with the 'allow_other' mount option.
	//
	// See fs/fuse/dir.c:fuse_allow_current_process() in Linux.
	if i.fs.opts.allowOther {
		// FIXME: Linux requires current_in_userns(fc->user_ns) in this case,
		// but we currently don't associate connections with a user namespace.
		return true
	}
	return creds.RealKUID == i.fs.opts.uid &&
		creds.EffectiveKUID == i.fs.opts.uid &&
		creds.SavedKUID == i.fs.opts.uid &&
		creds.RealKGID == i.fs.opts.gid &&
		creds.EffectiveKGID == i.fs.opts.gid &&
		creds.SavedKGID == i.fs.opts.gid
}

// newEntry calls FUSE server for entry creation and allocates corresponding
// entry according to response. Shared by FUSE_MKNOD, FUSE_MKDIR, FUSE_SYMLINK,
// FUSE_LINK and FUSE_LOOKUP.
func (i *inode) newEntry(ctx context.Context, name string, fileType linux.FileMode, opcode linux.FUSEOpcode, payload marshal.Marshallable) (kernfs.Inode, error) {
	out := linux.FUSECreateOut{}
	var err error
	if opcode == linux.FUSE_CREATE {
		err = i.call(ctx, opcode, payload, &out)
	} else {
		err = i.call(ctx, opcode, payload, &out.FUSEEntryOut)
	}
	if err != nil {
		return nil, err
	}
	if opcode != linux.FUSE_LOOKUP && ((out.Attr.Mode&linux.S_IFMT)^uint32(fileType) != 0 || out.NodeID == 0 || out.NodeID == linux.FUSE_ROOT_ID) {
		return nil, linuxerr.EIO
	}
	child, err := i.fs.newInode(ctx, out.FUSEEntryOut)
	if err != nil {
		return nil, err
	}
	if opcode == linux.FUSE_CREATE {
		// File handler is returned by fuse server at a time of file create.
		// Save it temporary in a created child, so Open could return it when invoked
		// to be sure after fh is consumed reset 'isNewFh' flag of inode
		childI, ok := child.(*inode)
		if ok {
			childI.fh.new = true
			childI.fh.handle = out.FUSEOpenOut.Fh
			childI.fh.flags = out.FUSEOpenOut.OpenFlag
		}
	}
	return child, nil
}

// getFUSEAttr returns a linux.FUSEAttr of this inode stored in local cache.
//
// +checklocks:i.attrMu
func (i *inode) getFUSEAttr() linux.FUSEAttr {
	ns := time.Second.Nanoseconds()
	return linux.FUSEAttr{
		Ino:       i.ino.Load(),
		UID:       i.uid.Load(),
		GID:       i.gid.Load(),
		Size:      i.size.Load(),
		Mode:      uint32(i.filemode()),
		BlkSize:   i.blockSize.Load(),
		Atime:     uint64(i.atime.Load() / ns),
		Mtime:     uint64(i.mtime.Load() / ns),
		Ctime:     uint64(i.ctime.Load() / ns),
		AtimeNsec: uint32(i.atime.Load() % ns),
		MtimeNsec: uint32(i.mtime.Load() % ns),
		CtimeNsec: uint32(i.ctime.Load() % ns),
		Nlink:     i.nlink.Load(),
	}
}

// statFromFUSEAttr makes attributes from linux.FUSEAttr to linux.Statx.
func statFromFUSEAttr(attr linux.FUSEAttr, mask, devMinor uint32) linux.Statx {
	var stat linux.Statx
	// Every basic field is filled from attr, so report all requested basic
	// fields as valid, as Linux's fuse_fillattr() does. VFS relies on this:
	// execve() requires STATX_TYPE in the result mask.
	stat.Mask = mask & linux.STATX_BASIC_STATS
	stat.Blksize = attr.BlkSize
	stat.DevMajor, stat.DevMinor = linux.UNNAMED_MAJOR, devMinor

	rdevMajor, rdevMinor := linux.DecodeDeviceID(attr.Rdev)
	stat.RdevMajor, stat.RdevMinor = uint32(rdevMajor), rdevMinor

	if mask&(linux.STATX_TYPE|linux.STATX_MODE) != 0 {
		stat.Mode = uint16(attr.Mode)
	}
	if mask&linux.STATX_NLINK != 0 {
		stat.Nlink = attr.Nlink
	}
	if mask&linux.STATX_UID != 0 {
		stat.UID = attr.UID
	}
	if mask&linux.STATX_GID != 0 {
		stat.GID = attr.GID
	}
	if mask&linux.STATX_ATIME != 0 {
		stat.Atime = linux.StatxTimestamp{
			Sec:  int64(attr.Atime),
			Nsec: attr.AtimeNsec,
		}
	}
	if mask&linux.STATX_MTIME != 0 {
		stat.Mtime = linux.StatxTimestamp{
			Sec:  int64(attr.Mtime),
			Nsec: attr.MtimeNsec,
		}
	}
	if mask&linux.STATX_CTIME != 0 {
		stat.Ctime = linux.StatxTimestamp{
			Sec:  int64(attr.Ctime),
			Nsec: attr.CtimeNsec,
		}
	}
	if mask&linux.STATX_INO != 0 {
		stat.Ino = attr.Ino
	}
	if mask&linux.STATX_SIZE != 0 {
		stat.Size = attr.Size
	}
	if mask&linux.STATX_BLOCKS != 0 {
		stat.Blocks = attr.Blocks
	}
	return stat
}

// getAttr gets the attribute of this inode by issuing a FUSE_GETATTR request.
// It updates the corresponding attributes if necessary.
//
// +checklocks:i.attrMu
func (i *inode) getAttr(ctx context.Context, creds *auth.Credentials, fs *vfs.Filesystem, opts vfs.StatOptions, flags uint32, fh uint64) (linux.FUSEAttr, error) {
	in := linux.FUSEGetAttrIn{
		GetAttrFlags: flags,
		Fh:           fh,
	}
	var out linux.FUSEAttrOut
	if err := i.call(ctx, linux.FUSE_GETATTR, &in, &out); err != nil {
		return linux.FUSEAttr{}, err
	}

	// Local version is newer, return the local one.
	i.fs.conn.mu.Lock()
	attributeVersion := i.fs.conn.attributeVersion.Load()
	if attributeVersion != 0 && i.attrVersion.Load() > attributeVersion {
		i.fs.conn.mu.Unlock()
		return i.getFUSEAttr(), nil
	}
	i.fs.conn.mu.Unlock()
	i.updateAttrs(out.Attr, int64(out.AttrValid), int64(out.AttrValidNsec))
	return out.Attr, nil
}

// reviseAttr attempts to update the attributes for internal purposes
// by calling getAttr with a pre-specified mask.
// Used by read, write, lseek.
//
// +checklocks:i.attrMu
func (i *inode) reviseAttr(ctx context.Context, flags uint32, fh uint64) error {
	// Never need atime for internal purposes.
	_, err := i.getAttr(ctx, auth.CredentialsFromContext(ctx), i.fs.VFSFilesystem(), vfs.StatOptions{
		Mask: linux.STATX_BASIC_STATS &^ linux.STATX_ATIME,
	}, flags, fh)
	return err
}

// fattrMaskFromStats converts vfs.SetStatOptions.Stat.Mask to linux stats mask
// aligned with the attribute mask defined in include/linux/fs.h.
func fattrMaskFromStats(mask uint32) uint32 {
	var fuseAttrMask uint32
	maskMap := map[uint32]uint32{
		linux.STATX_MODE:  linux.FATTR_MODE,
		linux.STATX_UID:   linux.FATTR_UID,
		linux.STATX_GID:   linux.FATTR_GID,
		linux.STATX_SIZE:  linux.FATTR_SIZE,
		linux.STATX_ATIME: linux.FATTR_ATIME,
		linux.STATX_MTIME: linux.FATTR_MTIME,
		linux.STATX_CTIME: linux.FATTR_CTIME,
	}
	for statxMask, fattrMask := range maskMap {
		if mask&statxMask != 0 {
			fuseAttrMask |= fattrMask
		}
	}
	return fuseAttrMask
}

type fhOptions struct {
	useFh bool
	fh    uint64
}

// +checklocks:i.attrMu
func (i *inode) setAttr(ctx context.Context, fs *vfs.Filesystem, creds *auth.Credentials, opts vfs.SetStatOptions, fhOpts fhOptions) error {
	// We should retain the original file type when assigning a new mode.
	fattrMask := fattrMaskFromStats(opts.Stat.Mask)
	if fhOpts.useFh {
		fattrMask |= linux.FATTR_FH
	}
	if opts.Stat.Mask&linux.STATX_ATIME != 0 && opts.Stat.Atime.Nsec == linux.UTIME_NOW {
		fattrMask |= linux.FATTR_ATIME_NOW
	}
	if opts.Stat.Mask&linux.STATX_MTIME != 0 && opts.Stat.Mtime.Nsec == linux.UTIME_NOW {
		fattrMask |= linux.FATTR_ATIME_NOW
	}
	in := linux.FUSESetAttrIn{
		Valid:     fattrMask,
		Fh:        fhOpts.fh,
		Size:      opts.Stat.Size,
		Atime:     uint64(opts.Stat.Atime.Sec),
		Mtime:     uint64(opts.Stat.Mtime.Sec),
		Ctime:     uint64(opts.Stat.Ctime.Sec),
		AtimeNsec: opts.Stat.Atime.Nsec,
		MtimeNsec: opts.Stat.Mtime.Nsec,
		CtimeNsec: opts.Stat.Ctime.Nsec,
		Mode:      uint32(uint16(i.filemode().FileType()) | opts.Stat.Mode),
		UID:       opts.Stat.UID,
		GID:       opts.Stat.GID,
	}
	var out linux.FUSEAttrOut
	if err := i.call(ctx, linux.FUSE_SETATTR, &in, &out); err != nil {
		return err
	}
	i.updateAttrs(out.Attr, int64(out.AttrValid), int64(out.AttrValidNsec))
	return nil
}

// +checklocks:i.attrMu
func (i *inode) updateAttrs(attr linux.FUSEAttr, validSec, validNSec int64) {
	i.fs.conn.mu.Lock()
	i.attrVersion.Store(i.fs.conn.attributeVersion.Add(1))
	i.fs.conn.mu.Unlock()
	i.attrTime = i.fs.clock.Now().AddTime(validTimeout(validSec, validNSec))

	i.ino.Store(attr.Ino)

	i.mode.Store((attr.Mode & 07777) | (i.mode.Load() & linux.S_IFMT))
	i.uid.Store(attr.UID)
	i.gid.Store(attr.GID)

	i.atime.Store(attr.ATimeNsec())
	i.mtime.Store(attr.MTimeNsec())
	i.ctime.Store(attr.CTimeNsec())

	oldSize := i.size.Load()
	i.size.Store(attr.Size)
	i.nlink.Store(attr.Nlink)
	i.truncateCache(oldSize, attr.Size)

	if !i.fs.opts.defaultPermissions {
		i.mode.Store(i.mode.Load() & ^uint32(linux.S_ISVTX))
	}
}

// CheckPermissions implements kernfs.Inode.CheckPermissions.
func (i *inode) CheckPermissions(ctx context.Context, creds *auth.Credentials, ats vfs.AccessTypes) error {
	if !i.allowCredentials(creds) {
		return linuxerr.EACCES
	}

	// By default, fusefs delegates all permission checks to the server.
	// However, standard unix permission checks can be enabled with the
	// default_permissions mount option.
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	refreshed := false
	opts := vfs.StatOptions{Mask: linux.STATX_MODE | linux.STATX_UID | linux.STATX_GID}
	if i.fs.opts.defaultPermissions || (ats.MayExec() && i.filemode().FileType() == linux.S_IFREG) {
		if i.fs.clock.Now().After(i.attrTime) {
			refreshed = true
			if _, err := i.getAttr(ctx, creds, i.fs.VFSFilesystem(), opts, 0, 0); err != nil {
				return err
			}
		}
	}

	if i.fs.opts.defaultPermissions || (ats.MayExec() && i.filemode().FileType() == linux.S_IFREG) {
		err := vfs.GenericCheckPermissions(creds, ats, linux.FileMode(i.mode.Load()), nil, auth.KUID(i.uid.Load()), auth.KGID(i.gid.Load()))
		if linuxerr.Equals(linuxerr.EACCES, err) && !refreshed {
			if _, err := i.getAttr(ctx, creds, i.fs.VFSFilesystem(), opts, 0, 0); err != nil {
				return err
			}
			return vfs.GenericCheckPermissions(creds, ats, linux.FileMode(i.mode.Load()), nil, auth.KUID(i.uid.Load()), auth.KGID(i.gid.Load()))
		}
		return err
	}

	if ats.MayRead() || ats.MayWrite() || ats.MayExec() {
		in := linux.FUSEAccessIn{Mask: uint32(ats)}
		return i.callNoReply(ctx, linux.FUSE_ACCESS, &in)
	}
	return nil
}

// Open implements kernfs.Inode.Open.
func (i *inode) Open(ctx context.Context, rp *vfs.ResolvingPath, d *kernfs.Dentry, opts vfs.OpenOptions) (*vfs.FileDescription, error) {
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	if opts.Flags&linux.O_LARGEFILE == 0 && i.size.Load() > linux.MAX_NON_LFS {
		return nil, linuxerr.EOVERFLOW
	}
	opts.Flags &= linux.O_ACCMODE | linux.O_CREAT | linux.O_EXCL | linux.O_TRUNC |
		linux.O_DIRECTORY | linux.O_NOFOLLOW | linux.O_NONBLOCK | linux.O_NOCTTY |
		linux.O_APPEND | linux.O_DIRECT

	var (
		fd     *fileDescription
		fdImpl vfs.FileDescriptionImpl
		opcode linux.FUSEOpcode
	)
	switch ft := i.filemode().FileType(); ft {
	case linux.S_IFREG:
		regularFD := &regularFileFD{}
		fd = &(regularFD.fileDescription)
		fdImpl = regularFD
		opcode = linux.FUSE_OPEN
	case linux.S_IFDIR:
		if opts.Flags&linux.O_CREAT != 0 {
			return nil, linuxerr.EISDIR
		}
		if ats := vfs.AccessTypesForOpenFlags(&opts); ats.MayWrite() {
			return nil, linuxerr.EISDIR
		}
		if opts.Flags&linux.O_DIRECT != 0 {
			return nil, linuxerr.EINVAL
		}
		directoryFD := &directoryFD{}
		fd = &(directoryFD.fileDescription)
		fdImpl = directoryFD
		opcode = linux.FUSE_OPENDIR
	case linux.S_IFLNK:
		return nil, linuxerr.ELOOP
	default:
		log.Warningf("Open on unknown file type: %v", ft)
		return nil, linuxerr.EINVAL
	}

	fd.LockFD.Init(&i.locks)
	isDir := i.filemode().IsDir()
	// The defaults when no open request is sent, as Linux's fuse_file_open():
	// keep the page cache and, for directories, cache the listing.
	fd.OpenFlag = linux.FOPEN_KEEP_CACHE
	if isDir {
		fd.OpenFlag |= linux.FOPEN_CACHE_DIR
	}

	// An open request is sent unless the file handle was already returned by
	// FUSE_CREATE, or the server does not support open (or opendir).
	willSendOpen := !i.fh.new && ((!isDir && !i.fs.conn.noOpen) || (isDir && !i.fs.conn.noOpenDir.Load()))
	// Without a server handle there is nothing to release.
	fd.noRelease = !i.fh.new && !willSendOpen

	truncateRegFile := opts.Flags&linux.O_TRUNC != 0 && i.filemode().FileType() == linux.S_IFREG
	if truncateRegFile && !(willSendOpen && i.fs.conn.atomicOTrunc) {
		// If the regular file needs to be truncated, but the connection doesn't
		// support O_TRUNC or if no Open RPC will be sent, then manually
		// truncate the file *before* Open. As per libfuse, "If [atomic O_TRUNC is]
		// disabled, and an application specifies O_TRUNC, fuse first calls
		// truncate() and then open() with O_TRUNC filtered out.".
		opts := vfs.SetStatOptions{Stat: linux.Statx{Size: 0, Mask: linux.STATX_SIZE}}
		if err := i.setAttr(ctx, i.fs.VFSFilesystem(), auth.CredentialsFromContext(ctx), opts, fhOptions{useFh: false}); err != nil {
			return nil, err
		}
	}

	if i.fh.new {
		fd.OpenFlag = i.fh.flags
		fd.Fh = i.fh.handle
		i.fh.new = false
	} else if willSendOpen {
		in := linux.FUSEOpenIn{Flags: opts.Flags & ^uint32(linux.O_CREAT|linux.O_EXCL|linux.O_NOCTTY)}
		// Clear O_TRUNC if the server doesn't support it.
		if !i.fs.conn.atomicOTrunc {
			in.Flags &= ^uint32(linux.O_TRUNC)
		}

		out := linux.FUSEOpenOut{}
		if err := i.call(ctx, opcode, &in, &out); err != nil {
			switch {
			case linuxerr.Equals(linuxerr.ENOSYS, err) && isDir:
				// As Linux, opendir of a server that doesn't implement it
				// succeeds and is never sent again (FUSE_NO_OPENDIR_SUPPORT).
				i.fs.conn.noOpenDir.Store(true)
				fd.noRelease = true
			case linuxerr.Equals(linuxerr.ENOSYS, err):
				i.fs.conn.noOpen = true
				fd.noRelease = true
				// The open that was to carry O_TRUNC was refused; truncate with SETATTR.
				if truncateRegFile && i.fs.conn.atomicOTrunc {
					opts := vfs.SetStatOptions{Stat: linux.Statx{Size: 0, Mask: linux.STATX_SIZE}}
					if err := i.setAttr(ctx, i.fs.VFSFilesystem(), auth.CredentialsFromContext(ctx), opts, fhOptions{useFh: false}); err != nil {
						return nil, err
					}
				}
			default:
				return nil, err
			}
		} else {
			fd.OpenFlag = out.OpenFlag
			fd.Fh = out.Fh
			// Open was successful. Update inode's size if atomicOTrunc && O_TRUNC.
			if truncateRegFile && i.fs.conn.atomicOTrunc {
				i.fs.conn.mu.Lock()
				i.attrVersion.Store(i.fs.conn.attributeVersion.Add(1))
				i.fs.conn.mu.Unlock()
				oldSize := i.size.Load()
				i.size.Store(0)
				i.truncateCache(oldSize, 0)
				i.touchCMtime()
			}
		}
	}
	if i.filemode().IsDir() {
		fd.OpenFlag &= ^uint32(linux.FOPEN_DIRECT_IO)
	}

	fd.DirectIO = fd.OpenFlag&linux.FOPEN_DIRECT_IO != 0
	fdOptions := &vfs.FileDescriptionOptions{}
	if fd.OpenFlag&linux.FOPEN_NONSEEKABLE != 0 {
		fdOptions.DenyPRead = true
		fdOptions.DenyPWrite = true
		fd.Nonseekable = true
	}

	if err := fd.vfsfd.Init(fdImpl, opts.Flags, rp.Credentials(), rp.Mount(), d.VFSDentry(), fdOptions); err != nil {
		return nil, err
	}
	if regularFD, ok := fdImpl.(*regularFileFD); ok && fd.OpenFlag&linux.FOPEN_KEEP_CACHE == 0 && i.fs.mf != nil {
		i.revalidateCache(ctx, regularFD)
	}
	return &fd.vfsfd, nil
}

func (i *inode) Valid(ctx context.Context, parent *kernfs.Dentry, name string) bool {
	now := i.fs.clock.Now()
	if entryTime := SeqAtomicLoadTime(&i.entryTimeSeq, &i.entryTime); entryTime.After(now) {
		return true
	}

	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	if i.entryTime.After(now) {
		return true
	}

	in := linux.FUSELookupIn{Name: linux.CString(name)}
	req := i.fs.conn.NewRequest(auth.CredentialsFromContext(ctx), pidFromContext(ctx), parent.Inode().(*inode).nodeID, linux.FUSE_LOOKUP, &in)
	res, err := i.fs.conn.Call(ctx, req)
	if err != nil {
		return false
	}
	if res.Error() != nil {
		return false
	}
	var out linux.FUSEEntryOut
	if res.UnmarshalPayload(&out) != nil {
		return false
	}
	if i.nodeID != out.NodeID {
		return false
	}
	// Don't enforce fuse_invalid_attr() => fuse_valid_type(),
	// fuse_valid_size() since inode.updateAttrs() and its callers
	// don't. But do enforce fuse_stale_inode():
	if i.generation != out.Generation {
		return false
	}
	if (i.mode.RacyLoad()^out.Attr.Mode)&linux.S_IFMT != 0 {
		return false
	}
	i.updateEntryTime(int64(out.EntryValid), int64(out.EntryValidNSec))
	return true
}

// Lookup implements kernfs.Inode.Lookup.
func (i *inode) Lookup(ctx context.Context, name string) (kernfs.Inode, error) {
	if i.knownAbsent(name) {
		return nil, linuxerr.ENOENT
	}
	in := linux.FUSELookupIn{Name: linux.CString(name)}
	var out linux.FUSEEntryOut
	if err := i.call(ctx, linux.FUSE_LOOKUP, &in, &out); err != nil {
		return nil, err
	}
	if out.NodeID == 0 {
		// A negative entry: as Linux's fuse_lookup_name(), the name doesn't
		// exist, and the server lets that be cached for entry_valid.
		i.addAbsent(name, validTimeout(int64(out.EntryValid), int64(out.EntryValidNSec)))
		return nil, linuxerr.ENOENT
	}
	return i.fs.newInode(ctx, out)
}

// maxNegativeEntries bounds the negative entries cached per filesystem.
// ponytail: past it, absent names just aren't cached; Linux instead lets the
// dcache shrinker reclaim negative dentries.
const maxNegativeEntries = 1 << 16

// knownAbsent reports whether name is cached as absent from directory i.
func (i *inode) knownAbsent(name string) bool {
	i.dirMu.Lock()
	defer i.dirMu.Unlock()
	expiry, ok := i.negative[name]
	if !ok {
		return false
	}
	if expiry.After(i.fs.clock.Now()) {
		return true
	}
	delete(i.negative, name)
	i.fs.negatives.Add(-1)
	return false
}

// addAbsent caches name as absent from directory i for timeout.
func (i *inode) addAbsent(name string, timeout ktime.Time) {
	if timeout == ktime.ZeroTime {
		return
	}
	i.dirMu.Lock()
	defer i.dirMu.Unlock()
	if _, ok := i.negative[name]; !ok {
		if i.fs.negatives.Add(1) > maxNegativeEntries {
			i.fs.negatives.Add(-1)
			return
		}
	}
	if i.negative == nil {
		i.negative = make(map[string]ktime.Time)
	}
	i.negative[name] = i.fs.clock.Now().AddTime(timeout)
}

// dropDirCaches forgets directory i's cached listing and negative entries.
func (i *inode) dropDirCaches() {
	i.dirMu.Lock()
	defer i.dirMu.Unlock()
	i.dirVersion++
	i.dirents, i.direntsOK = nil, false
	i.fs.negatives.Add(-int64(len(i.negative)))
	i.negative = nil
}

// Keep implements kernfs.Inode.Keep.
func (i *inode) Keep() bool {
	// Return true so that kernfs keeps the new dentry pointing to this
	// inode in the dentry tree. This is needed because inodes created via
	// Lookup are not temporary. They might refer to existing files on server
	// that can be Unlink'd/Rmdir'd.
	return true
}

// IterDirents implements kernfs.Inode.IterDirents.
func (*inode) IterDirents(ctx context.Context, mnt *vfs.Mount, callback vfs.IterDirentsCallback, offset, relOffset int64) (int64, error) {
	return offset, nil
}

// NewFile implements kernfs.Inode.NewFile.
func (i *inode) NewFile(ctx context.Context, name string, opts vfs.OpenOptions) (kernfs.Inode, error) {
	defer i.dirChanged(name)
	opts.Flags &= linux.O_ACCMODE | linux.O_CREAT | linux.O_EXCL | linux.O_TRUNC |
		linux.O_DIRECTORY | linux.O_NOFOLLOW | linux.O_NONBLOCK | linux.O_NOCTTY
	if !i.fs.conn.noCreate {
		in := linux.FUSECreateIn{
			CreateMeta: linux.FUSECreateMeta{
				Flags: opts.Flags,
				Mode:  uint32(opts.Mode) | linux.S_IFREG,
				Umask: umaskFromContext(ctx),
			},
			Name: linux.CString(name),
		}
		child, err := i.newEntry(ctx, name, linux.S_IFREG, linux.FUSE_CREATE, &in)
		if err == nil || !linuxerr.Equals(linuxerr.ENOSYS, err) {
			return child, err
		}
		i.fs.conn.noCreate = true
	}
	// The MKNOD reply carries no file handle, so Open() will issue a FUSE_OPEN.
	in := linux.FUSEMknodIn{
		MknodMeta: linux.FUSEMknodMeta{
			Mode:  uint32(opts.Mode) | linux.S_IFREG,
			Rdev:  0,
			Umask: umaskFromContext(ctx),
		},
		Name: linux.CString(name),
	}
	return i.newEntry(ctx, name, linux.S_IFREG, linux.FUSE_MKNOD, &in)
}

// NewNode implements kernfs.Inode.NewNode.
func (i *inode) NewNode(ctx context.Context, name string, opts vfs.MknodOptions) (kernfs.Inode, error) {
	defer i.dirChanged(name)
	in := linux.FUSEMknodIn{
		MknodMeta: linux.FUSEMknodMeta{
			Mode:  uint32(opts.Mode),
			Rdev:  linux.MakeDeviceID(uint16(opts.DevMajor), opts.DevMinor),
			Umask: umaskFromContext(ctx),
		},
		Name: linux.CString(name),
	}
	return i.newEntry(ctx, name, opts.Mode.FileType(), linux.FUSE_MKNOD, &in)
}

// NewSymlink implements kernfs.Inode.NewSymlink.
func (i *inode) NewSymlink(ctx context.Context, name, target string) (kernfs.Inode, error) {
	defer i.dirChanged(name)
	in := linux.FUSESymlinkIn{
		Name:   linux.CString(name),
		Target: linux.CString(target),
	}
	return i.newEntry(ctx, name, linux.S_IFLNK, linux.FUSE_SYMLINK, &in)
}

// NewLink implements kernfs.Inode.NewLink.
func (i *inode) NewLink(ctx context.Context, name string, target kernfs.Inode) (kernfs.Inode, error) {
	defer i.dirChanged(name)
	defer target.(*inode).invalidateAttrs() // nlink, ctime
	targetInode := target.(*inode)
	in := linux.FUSELinkIn{
		OldNodeID: primitive.Uint64(targetInode.nodeID),
		Name:      linux.CString(name),
	}
	return i.newEntry(ctx, name, targetInode.Mode().FileType(), linux.FUSE_LINK, &in)
}

// Unlink implements kernfs.Inode.Unlink.
func (i *inode) Unlink(ctx context.Context, name string, child kernfs.Inode) error {
	defer i.dirChanged(name)
	defer child.(*inode).invalidateAttrs() // nlink, ctime
	in := linux.FUSEUnlinkIn{Name: linux.CString(name)}
	return i.callNoReply(ctx, linux.FUSE_UNLINK, &in)
}

// NewDir implements kernfs.Inode.NewDir.
func (i *inode) NewDir(ctx context.Context, name string, opts vfs.MkdirOptions) (kernfs.Inode, error) {
	defer i.dirChanged(name)
	in := linux.FUSEMkdirIn{
		MkdirMeta: linux.FUSEMkdirMeta{
			Mode:  uint32(opts.Mode),
			Umask: umaskFromContext(ctx),
		},
		Name: linux.CString(name),
	}
	return i.newEntry(ctx, name, linux.S_IFDIR, linux.FUSE_MKDIR, &in)
}

// RmDir implements kernfs.Inode.RmDir.
func (i *inode) RmDir(ctx context.Context, name string, child kernfs.Inode) error {
	defer i.dirChanged(name)
	defer child.(*inode).invalidateAttrs() // nlink, ctime
	in := linux.FUSERmDirIn{Name: linux.CString(name)}
	return i.callNoReply(ctx, linux.FUSE_RMDIR, &in)
}

// Rename implements kernfs.Inode.Rename.
func (i *inode) Rename(ctx context.Context, oldname, newname string, child, dstDir kernfs.Inode) error {
	dstDirInode := dstDir.(*inode)
	defer i.dirChanged(oldname)
	defer dstDirInode.dirChanged(newname)
	defer child.(*inode).invalidateAttrs() // ctime
	in := linux.FUSERenameIn{
		Newdir:  primitive.Uint64(dstDirInode.nodeID),
		Oldname: linux.CString(oldname),
		Newname: linux.CString(newname),
	}
	return i.callNoReply(ctx, linux.FUSE_RENAME, &in)
}

// Getlink implements kernfs.Inode.Getlink.
func (i *inode) Getlink(ctx context.Context, mnt *vfs.Mount) (vfs.VirtualDentry, string, error) {
	path, err := i.Readlink(ctx, mnt)
	return vfs.VirtualDentry{}, path, err
}

// Readlink implements kernfs.Inode.Readlink.
func (i *inode) Readlink(ctx context.Context, mnt *vfs.Mount) (string, error) {
	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	if i.filemode().FileType()&linux.S_IFLNK == 0 {
		return "", linuxerr.EINVAL
	}
	if len(i.link) == 0 {
		req := i.fs.conn.NewRequest(auth.CredentialsFromContext(ctx), pidFromContext(ctx), i.nodeID, linux.FUSE_READLINK, &linux.FUSEEmptyIn{})
		res, err := i.fs.conn.Call(ctx, req)
		if err != nil {
			return "", err
		}
		if err := res.Error(); err != nil {
			return "", err
		}
		i.link = string(res.data[res.hdr.SizeBytes():])
		if !mnt.Options().ReadOnly {
			i.attrTime = ktime.ZeroTime
		}
	}
	return i.link, nil
}

// Stat implements kernfs.Inode.Stat.
func (i *inode) Stat(ctx context.Context, fs *vfs.Filesystem, opts vfs.StatOptions) (linux.Statx, error) {
	creds := auth.CredentialsFromContext(ctx)
	if !i.allowCredentials(creds) {
		if opts.Mask == 0 {
			return linux.Statx{
				DevMajor: linux.UNNAMED_MAJOR,
				DevMinor: i.fs.devMinor,
			}, nil
		}
		return linux.Statx{}, linuxerr.EACCES
	}

	// TODO: FUSE_STATX is currently unsupported, so btime is currently
	// unsupported.
	opts.Mask &= linux.STATX_BASIC_STATS

	// Report cached attributes without attrMu if no sync is needed: this is
	// reached with mm.MemoryManager.mappingMu locked (/proc/[pid]/maps =>
	// memmap.MappingIdentity.DeviceID/InodeID), while read(2) and write(2)
	// hold attrMu across copies to and from application memory, which lock
	// mappingMu. The attributes are atomics.
	if opts.Mask == 0 || opts.Sync == linux.AT_STATX_DONT_SYNC {
		return statFromFUSEAttr(i.getFUSEAttr(), opts.Mask, i.fs.devMinor), nil // +checklocksforce: see above.
	}

	i.attrMu.Lock()
	defer i.attrMu.Unlock()

	// TODO(gvisor.dev/issue/3679): support per-field cache validity
	sync := opts.Sync == linux.AT_STATX_FORCE_SYNC || i.attrTime.Before(i.fs.clock.Now())

	if sync {
		attr, err := i.getAttr(ctx, creds, fs, opts, 0, 0)
		if err != nil {
			return linux.Statx{}, err
		}
		return statFromFUSEAttr(attr, opts.Mask, i.fs.devMinor), nil
	}
	return statFromFUSEAttr(i.getFUSEAttr(), opts.Mask, i.fs.devMinor), nil
}

// StatFS implements kernfs.Inode.StatFS.
func (i *inode) StatFS(ctx context.Context, fs *vfs.Filesystem) (linux.Statfs, error) {
	if !i.allowCredentials(auth.CredentialsFromContext(ctx)) {
		return linux.Statfs{
			Type: linux.FUSE_SUPER_MAGIC,
		}, nil
	}

	var out linux.FUSEStatfsOut
	if err := i.call(ctx, linux.FUSE_STATFS, &linux.FUSEEmptyIn{}, &out); err != nil {
		return linux.Statfs{}, err
	}

	return linux.Statfs{
		Type:            linux.FUSE_SUPER_MAGIC,
		Blocks:          uint64(out.Blocks),
		BlocksFree:      out.BlocksFree,
		BlocksAvailable: out.BlocksAvailable,
		Files:           out.Files,
		FilesFree:       out.FilesFree,
		BlockSize:       int64(out.BlockSize),
		NameLength:      uint64(out.NameLength),
		FragmentSize:    int64(out.FragmentSize),
	}, nil
}

// SetStat implements kernfs.Inode.SetStat.
func (i *inode) SetStat(ctx context.Context, fs *vfs.Filesystem, creds *auth.Credentials, opts vfs.SetStatOptions) error {
	if !i.allowCredentials(creds) {
		return linuxerr.EACCES
	}

	i.attrMu.Lock()
	defer i.attrMu.Unlock()
	if err := vfs.CheckSetStat(ctx, creds, &opts, i.filemode(), nil, auth.KUID(i.uid.Load()), auth.KGID(i.gid.Load())); err != nil {
		return err
	}
	if opts.Stat.Mask == 0 {
		return nil
	}
	return i.setAttr(ctx, fs, creds, opts, fhOptions{useFh: false})
}

// GetXattr implements kernfs.InodeWithXattrs.GetXattr.
func (i *inode) GetXattr(ctx context.Context, opts vfs.GetXattrOptions) (string, error) {
	in := linux.FUSEGetXattrIn{
		Hdr: linux.FUSEGetXattrHdr{
			Size: uint32(opts.Size),
		},
		Name: linux.CString(opts.Name),
	}

	res, err := i.xattrCall(ctx, linux.FUSE_GETXATTR, &i.fs.conn.noGetXattr, &in)
	if err != nil {
		return "", err
	}

	if opts.Size == 0 {
		var out linux.FUSEGetXattrOut
		if err := res.UnmarshalPayload(&out); err != nil {
			return "", err
		}
		return string(make([]byte, out.Size)), nil
	}

	if len(res.data) <= res.hdr.SizeBytes() {
		return "", nil
	}

	return string(res.data[res.hdr.SizeBytes():]), nil
}

// SetXattr implements kernfs.InodeWithXattrs.SetXattr.
func (i *inode) SetXattr(ctx context.Context, opts vfs.SetXattrOptions) error {
	in := linux.FUSESetXattrIn{
		Hdr: linux.FUSESetXattrHdr{
			Size:  uint32(len(opts.Value)),
			Flags: opts.Flags,
		},
		Name:  linux.CString(opts.Name),
		Value: []byte(opts.Value),
	}

	_, err := i.xattrCall(ctx, linux.FUSE_SETXATTR, &i.fs.conn.noSetXattr, &in)
	return err
}

// ListXattr implements kernfs.InodeWithXattrs.ListXattr.
func (i *inode) ListXattr(ctx context.Context, size uint64) ([]string, error) {
	in := linux.FUSEGetXattrHdr{
		Size: uint32(size),
	}

	res, err := i.xattrCall(ctx, linux.FUSE_LISTXATTR, &i.fs.conn.noListXattr, &in)
	if err != nil {
		return nil, err
	}

	if size == 0 {
		var out linux.FUSEGetXattrOut
		if err := res.UnmarshalPayload(&out); err != nil {
			return nil, err
		}
		if out.Size == 0 {
			return nil, nil
		}
		if out.Size > linux.XATTR_LIST_MAX {
			return nil, linuxerr.E2BIG
		}
		return []string{string(make([]byte, out.Size-1))}, nil
	}

	if len(res.data) <= res.hdr.SizeBytes() {
		return nil, nil
	}

	payload := res.data[res.hdr.SizeBytes():]
	if len(payload) > linux.XATTR_LIST_MAX {
		return nil, linuxerr.E2BIG
	}

	var names []string
	start := 0
	for idx, b := range payload {
		if b == 0 {
			name := string(payload[start:idx])
			if len(name) > linux.XATTR_NAME_MAX {
				return nil, linuxerr.ERANGE
			}
			names = append(names, name)
			start = idx + 1
		}
	}
	return names, nil
}

// RemoveXattr implements kernfs.InodeWithXattrs.RemoveXattr.
func (i *inode) RemoveXattr(ctx context.Context, name string) error {
	in := linux.CString(name)
	_, err := i.xattrCall(ctx, linux.FUSE_REMOVEXATTR, &i.fs.conn.noRemoveXattr, &in)
	return err
}

// xattrCall sends an xattr request and returns its successful reply. As in
// Linux's fs/fuse/xattr.c, a server that answers ENOSYS doesn't support the
// request: it is reported as EOPNOTSUPP, which tools such as ls and libacl
// treat as "no xattrs", and latched in *unsupported so that it isn't sent
// again.
func (i *inode) xattrCall(ctx context.Context, opcode linux.FUSEOpcode, unsupported *atomicbitops.Bool, in marshal.Marshallable) (*Response, error) {
	if unsupported.Load() {
		return nil, linuxerr.EOPNOTSUPP
	}
	res, err := i.callRaw(ctx, opcode, in)
	if err != nil {
		return nil, err
	}
	if err := res.Error(); err != nil {
		if linuxerr.Equals(linuxerr.ENOSYS, err) {
			unsupported.Store(true)
			return nil, linuxerr.EOPNOTSUPP
		}
		return nil, err
	}
	return res, nil
}
