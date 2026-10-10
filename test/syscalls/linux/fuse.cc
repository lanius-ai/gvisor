// Copyright 2023 The gVisor Authors.
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

#include <dirent.h>
#include <fcntl.h>
#include <linux/capability.h>
#include <linux/fuse.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/uio.h>
#include <sys/wait.h>
#include <sys/xattr.h>
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <limits>
#include <memory>
#include <string>
#include <vector>

#include "gmock/gmock.h"
#include "gtest/gtest.h"
#include "absl/strings/str_format.h"
#include "absl/strings/string_view.h"
#include "absl/synchronization/notification.h"
#include "absl/time/clock.h"
#include "absl/time/time.h"
#include "test/util/cleanup.h"
#include "test/util/file_descriptor.h"
#include "test/util/fs_util.h"
#include "test/util/linux_capability_util.h"
#include "test/util/memory_util.h"
#include "test/util/mount_util.h"
#include "test/util/multiprocess_util.h"
#include "test/util/posix_error.h"
#include "test/util/save_util.h"
#include "test/util/signal_util.h"
#include "test/util/temp_path.h"
#include "test/util/test_util.h"
#include "test/util/thread_util.h"

using ::testing::AnyOf;
using ::testing::Ge;
using ::testing::UnorderedElementsAre;

namespace gvisor {
namespace testing {

namespace {

void FuseInit(int fd) {
  alignas(fuse_in_header) char req_buf[FUSE_MIN_READ_BUFFER];
  ASSERT_THAT(read(fd, req_buf, sizeof(req_buf)),
              SyscallSucceedsWithValue(Ge(sizeof(fuse_in_header))));

  fuse_in_header* in_hdr = reinterpret_cast<fuse_in_header*>(req_buf);
  ASSERT_EQ(in_hdr->opcode, FUSE_INIT);

  fuse_out_header out_hdr;
  out_hdr.error = 0;
  out_hdr.unique = in_hdr->unique;
  fuse_init_out out_payload = {};
  out_payload.major = FUSE_KERNEL_VERSION;
  out_payload.minor = FUSE_KERNEL_MINOR_VERSION;
  out_payload.max_readahead = 0;
  out_payload.flags = 0;
  out_payload.congestion_threshold = 0;

  struct iovec iov[] = {
      {.iov_base = &out_hdr, .iov_len = sizeof(out_hdr)},
      {.iov_base = &out_payload, .iov_len = sizeof(out_payload)},
  };
  out_hdr.len = sizeof(out_hdr) + sizeof(out_payload);

  ASSERT_THAT(writev(fd, iov, 2),
              SyscallSucceedsWithValue(sizeof(out_hdr) + sizeof(out_payload)));
}

TEST(FuseTest, RejectBadInit) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const FileDescriptor fd =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR, 0));

  auto mount_point = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mount_opts =
      absl::StrFormat("fd=%d,user_id=0,group_id=0,rootmode=40000", fd.get());
  auto mount = ASSERT_NO_ERRNO_AND_VALUE(
      Mount("fuse", mount_point.path(), "fuse", MS_NODEV | MS_NOSUID,
            mount_opts, 0 /* umountflags */));

  // Read the init request so that we have the correct unique ID.
  alignas(fuse_in_header) char req_buf[FUSE_MIN_READ_BUFFER];
  ASSERT_THAT(read(fd.get(), req_buf, sizeof(req_buf)),
              SyscallSucceedsWithValue(Ge(sizeof(fuse_in_header))));

  fuse_out_header resp;
  resp.len = sizeof(resp) - 1;
  resp.error = 0;
  resp.unique = reinterpret_cast<fuse_in_header*>(req_buf)->unique;

  ASSERT_THAT(write(fd.get(), reinterpret_cast<char*>(&resp), sizeof(resp)),
              SyscallFailsWithErrno(EINVAL));
}

TEST(FuseTest, CloneDevice) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  SKIP_IF(IsRunningWithSaveRestore());

  const FileDescriptor fd1 =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));

  auto mount_point = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mount_opts =
      absl::StrFormat("fd=%d,user_id=0,group_id=0,rootmode=40000", fd1.get());
  auto mount = ASSERT_NO_ERRNO_AND_VALUE(
      Mount("fuse", mount_point.path(), "fuse", MS_NODEV | MS_NOSUID,
            mount_opts, 0 /* umountflags */));
  FuseInit(fd1.get());

  const FileDescriptor fd2 =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));
  int fd1_num = fd1.get();
  ASSERT_THAT(ioctl(fd2.get(), FUSE_DEV_IOC_CLONE, &fd1_num),
              SyscallSucceeds());

  ScopedThread fuse_server = ScopedThread([&] {
    // Send stat reply from both FUSE servers.
    for (int fd : {fd1.get(), fd2.get()}) {
      // Read the stat request.
      alignas(fuse_in_header) char req_buf[4096 * 4];
      ASSERT_THAT(read(fd, req_buf, sizeof(req_buf)),
                  SyscallSucceedsWithValue(Ge(sizeof(fuse_in_header))));

      fuse_in_header* in_hdr = reinterpret_cast<fuse_in_header*>(req_buf);
      ASSERT_EQ(in_hdr->opcode, FUSE_GETATTR);

      // Send stat reply.
      fuse_out_header out_hdr;
      out_hdr.error = 0;
      out_hdr.unique = in_hdr->unique;
      fuse_attr_out out_payload = {};
      out_payload.attr.mode = S_IFDIR | 0755;
      out_payload.attr.nlink = 1;
      out_payload.attr.uid = 0;
      out_payload.attr.gid = 0;
      out_payload.attr.size = fd;
      out_payload.attr.atime = 0;
      out_payload.attr.mtime = 0;
      out_payload.attr.ctime = 0;

      struct iovec iov[] = {
          {.iov_base = &out_hdr, .iov_len = sizeof(out_hdr)},
          {.iov_base = &out_payload, .iov_len = sizeof(out_payload)},
      };
      out_hdr.len = sizeof(out_hdr) + sizeof(out_payload);

      ASSERT_THAT(
          writev(fd, iov, 2),
          SyscallSucceedsWithValue(sizeof(out_hdr) + sizeof(out_payload)));
    }
  });

  // Check if filesystem is responsive by stat'ing root. Both FUSE servers
  // should be able to respond.
  struct stat st;
  EXPECT_THAT(stat(mount_point.path().c_str(), &st), SyscallSucceeds());
  EXPECT_EQ(st.st_size, fd1.get());
  EXPECT_THAT(stat(mount_point.path().c_str(), &st), SyscallSucceeds());
  EXPECT_EQ(st.st_size, fd2.get());

  fuse_server.Join();
}

TEST(FuseTest, CloneToConnectedDeviceFails) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const FileDescriptor fd1 =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));

  auto mount_point = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mount_opts =
      absl::StrFormat("fd=%d,user_id=0,group_id=0,rootmode=40000", fd1.get());
  auto mount = ASSERT_NO_ERRNO_AND_VALUE(
      Mount("fuse", mount_point.path(), "fuse", MS_NODEV | MS_NOSUID,
            mount_opts, 0 /* umountflags */));
  FuseInit(fd1.get());

  int fd1_num = fd1.get();
  EXPECT_THAT(ioctl(fd1.get(), FUSE_DEV_IOC_CLONE, &fd1_num),
              SyscallFailsWithErrno(EINVAL));
}

TEST(FuseTest, CloneFromUnconnectedDeviceFails) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const FileDescriptor fd1 =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));

  const FileDescriptor fd2 =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));

  int fd1_num = fd1.get();
  EXPECT_THAT(ioctl(fd2.get(), FUSE_DEV_IOC_CLONE, &fd1_num),
              SyscallFailsWithErrno(EINVAL));
}

// kNoReply tells FuseServer not to answer a request (FORGET, INTERRUPT).
constexpr ssize_t kNoReply = std::numeric_limits<ssize_t>::min();

// A FUSE filesystem served by the test, which can count the requests the
// kernel sends. Every request but FUSE_INIT goes to handle, which writes the
// reply's payload to out and returns its length, a negative errno, or
// kNoReply. Destruction unmounts it and stops the server.
class FuseServer {
 public:
  using Handler = std::function<ssize_t(const fuse_in_header& in,
                                        const char* in_payload, char* out)>;

  static PosixErrorOr<std::unique_ptr<FuseServer>> Mount(Handler handle) {
    auto s = std::unique_ptr<FuseServer>(new FuseServer());
    s->handle_ = std::move(handle);
    ASSIGN_OR_RETURN_ERRNO(s->fd_, Open("/dev/fuse", O_RDWR));
    ASSIGN_OR_RETURN_ERRNO(s->mount_point_, TempPath::CreateDir());
    const std::string opts = absl::StrFormat(
        "fd=%d,user_id=0,group_id=0,rootmode=40755", s->fd_.get());
    RETURN_ERROR_IF_SYSCALL_FAIL(mount("fuse", s->mount_point_.path().c_str(),
                                       "fuse", MS_NODEV | MS_NOSUID,
                                       opts.c_str()));
    s->mounted_ = true;
    FuseServer* raw = s.get();
    s->thread_ = std::make_unique<ScopedThread>([raw] { raw->Serve(); });
    return s;
  }

  ~FuseServer() {
    if (mounted_) {
      umount(mount_point_.path().c_str());  // The server reads ENODEV.
    }
    thread_.reset();
  }

  std::string Path(absl::string_view name) const {
    return JoinPath(mount_point_.path(), name);
  }

 private:
  FuseServer() = default;

  void Serve() {
    std::vector<char> req(FUSE_MIN_READ_BUFFER);
    std::vector<char> resp(sizeof(fuse_out_header) + (1 << 20));
    for (;;) {
      ssize_t n = read(fd_.get(), req.data(), req.size());
      if (n < 0 && errno == EINTR) {
        continue;
      }
      if (n < 0) {
        return;  // ENODEV after umount.
      }
      auto* in = reinterpret_cast<fuse_in_header*>(req.data());
      auto* out = reinterpret_cast<fuse_out_header*>(resp.data());
      char* payload = resp.data() + sizeof(*out);
      memset(resp.data(), 0, resp.size());
      ssize_t len;
      if (in->opcode == FUSE_INIT) {
        auto* init = reinterpret_cast<fuse_init_out*>(payload);
        init->major = FUSE_KERNEL_VERSION;
        init->minor = FUSE_KERNEL_MINOR_VERSION;
        len = sizeof(*init);
      } else {
        len = handle_(*in, req.data() + sizeof(*in), payload);
      }
      if (len == kNoReply) {
        continue;
      }
      out->unique = in->unique;
      if (len < 0) {
        out->error = len;
        len = 0;
      }
      out->len = sizeof(*out) + len;
      write(fd_.get(), resp.data(), out->len);
    }
  }

  FileDescriptor fd_;
  TempPath mount_point_;
  bool mounted_ = false;
  Handler handle_;
  std::unique_ptr<ScopedThread> thread_;
};

void FillAttr(fuse_attr* attr, uint64_t ino, mode_t mode, uint64_t size,
              uint32_t nlink) {
  attr->ino = ino;
  attr->mode = mode;
  attr->size = size;
  attr->nlink = nlink;
}

ssize_t ReplyEntry(char* out, uint64_t nodeid, mode_t mode, uint64_t size) {
  auto* entry = reinterpret_cast<fuse_entry_out*>(out);
  entry->nodeid = nodeid;
  entry->entry_valid = entry->attr_valid = 3600;
  FillAttr(&entry->attr, nodeid, mode, size, S_ISDIR(mode) ? 2 : 1);
  return sizeof(*entry);
}

ssize_t ReplyAttr(char* out, uint64_t nodeid, mode_t mode, uint64_t size,
                  uint32_t nlink) {
  auto* attr = reinterpret_cast<fuse_attr_out*>(out);
  attr->attr_valid = 3600;
  FillAttr(&attr->attr, nodeid, mode, size, nlink);
  return sizeof(*attr);
}

TEST(FuseTest, LookupUpdatesInode) {
  SKIP_IF(absl::NullSafeStringView(getenv("GVISOR_FUSE_TEST")) != "TRUE");
  const std::string kFileData = "May thy knife chip and shatter.\n";
  TempPath path = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateFileWith(
      GetAbsoluteTestTmpdir(), kFileData, TempPath::kDefaultFileMode));

  FileDescriptor fd = ASSERT_NO_ERRNO_AND_VALUE(Open(path.path(), O_RDONLY));
  std::vector<char> buf(kFileData.size());
  ASSERT_THAT(ReadFd(fd.get(), buf.data(), kFileData.size()),
              SyscallSucceedsWithValue(kFileData.size()));

  ASSERT_THAT(unlink(JoinPath("/fuse", Basename(path.path())).c_str()),
              SyscallSucceeds());

  EXPECT_THAT(access(path.path().c_str(), O_RDONLY),
              SyscallFailsWithErrno(ENOENT));
}

// Unmounting aborts the connection, so that the server learns that the
// filesystem is gone (Linux: fuse_sb_destroy() => fuse_abort_conn()).
TEST(FuseTest, UnmountAbortsConnection) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const FileDescriptor fd =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));

  auto mount_point = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mount_opts =
      absl::StrFormat("fd=%d,user_id=0,group_id=0,rootmode=40000", fd.get());
  ASSERT_THAT(mount("fuse", mount_point.path().c_str(), "fuse",
                    MS_NODEV | MS_NOSUID, mount_opts.c_str()),
              SyscallSucceeds());
  FuseInit(fd.get());
  ASSERT_THAT(umount(mount_point.path().c_str()), SyscallSucceeds());

  struct pollfd pfd = {.fd = fd.get(), .events = POLLIN};
  ASSERT_THAT(RetryEINTR(poll)(&pfd, 1, 10000), SyscallSucceedsWithValue(1));
  alignas(fuse_in_header) char req_buf[FUSE_MIN_READ_BUFFER];
  EXPECT_THAT(read(fd.get(), req_buf, sizeof(req_buf)),
              SyscallFailsWithErrno(ENODEV));
}

// execve() of a script requires the file type in the statx mask.
TEST(FuseTest, StatxAndExecScript) {
  TempPath script = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateFileWith(
      GetAbsoluteTestTmpdir(), "#!/bin/sh\nexit 7\n", 0755));

  struct statx stx;
  ASSERT_THAT(syscall(SYS_statx, AT_FDCWD, script.path().c_str(), 0,
                      STATX_BASIC_STATS, &stx),
              SyscallSucceeds());
  EXPECT_EQ(stx.stx_mask & STATX_BASIC_STATS, STATX_BASIC_STATS);
  EXPECT_TRUE(S_ISREG(stx.stx_mode));

  pid_t child;
  int execve_errno;
  auto kill = ASSERT_NO_ERRNO_AND_VALUE(
      ForkAndExec(script.path(), {script.path()}, {}, &child, &execve_errno));
  ASSERT_EQ(execve_errno, 0);
  int status;
  ASSERT_THAT(RetryEINTR(waitpid)(child, &status, 0),
              SyscallSucceedsWithValue(child));
  kill.Release();
  EXPECT_TRUE(WIFEXITED(status) && WEXITSTATUS(status) == 7) << status;
}

// Pages of a large shared mapping are read on fault, including pages far
// apart and holes.
TEST(FuseTest, MmapLargeSparseFile) {
  constexpr size_t kSize = 256 << 20;
  constexpr size_t kStride = 1 << 20;
  TempPath path =
      ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateFileIn(GetAbsoluteTestTmpdir()));
  FileDescriptor fd = ASSERT_NO_ERRNO_AND_VALUE(Open(path.path(), O_RDWR));
  ASSERT_THAT(ftruncate(fd.get(), kSize), SyscallSucceeds());
  for (size_t off = 0; off < kSize; off += kStride) {
    const char c = 'a' + (off / kStride) % 26;
    ASSERT_THAT(pwrite(fd.get(), &c, 1, off + kStride / 2),
                SyscallSucceedsWithValue(1));
  }

  Mapping m = ASSERT_NO_ERRNO_AND_VALUE(
      Mmap(nullptr, kSize, PROT_READ, MAP_SHARED, fd.get(), 0));
  const char* p = reinterpret_cast<const char*>(m.ptr());
  for (size_t off = 0; off < kSize; off += kStride) {
    EXPECT_EQ(p[off + kStride / 2], 'a' + (off / kStride) % 26) << off;
    EXPECT_EQ(p[off + kStride / 2 + 1], 0) << off;
    EXPECT_EQ(p[off], 0) << off;
  }

  // A later write(2) is visible through the mapping.
  const char c = 'Z';
  ASSERT_THAT(pwrite(fd.get(), &c, 1, kSize - 1), SyscallSucceedsWithValue(1));
  EXPECT_EQ(p[kSize - 1], 'Z');
}

// A signal that interrupts a page fault waiting for the server is handled and
// the fault is retried, instead of raising SIGBUS.
TEST(FuseTest, MmapFaultInterruptedBySignal) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  // The server thread must not be paused by a save.
  const DisableSave ds;
  const FileDescriptor fd =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));
  auto mount_point = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mount_opts =
      absl::StrFormat("fd=%d,user_id=0,group_id=0,rootmode=40755", fd.get());
  ASSERT_THAT(mount("fuse", mount_point.path().c_str(), "fuse",
                    MS_NODEV | MS_NOSUID, mount_opts.c_str()),
              SyscallSucceeds());
  FuseInit(fd.get());

  constexpr uint64_t kFileNode = 2;
  const size_t kFileSize = kPageSize;
  absl::Notification first_read, release_first_read;
  ScopedThread server([&] {
    auto fill_attr = [&](uint64_t nodeid, fuse_attr* attr) {
      attr->ino = nodeid;
      attr->nlink = 1;
      if (nodeid == FUSE_ROOT_ID) {
        attr->mode = S_IFDIR | 0755;
      } else {
        attr->mode = S_IFREG | 0644;
        attr->size = kFileSize;
      }
    };
    int reads = 0;
    std::vector<char> req(FUSE_MIN_READ_BUFFER + kPageSize);
    std::vector<char> resp(sizeof(fuse_out_header) + kPageSize);
    for (;;) {
      ssize_t n = read(fd.get(), req.data(), req.size());
      if (n < 0 && errno == EINTR) {
        continue;
      }
      if (n < 0) {
        return;  // ENODEV after umount.
      }
      auto* in = reinterpret_cast<fuse_in_header*>(req.data());
      void* in_payload = in + 1;
      auto* out = reinterpret_cast<fuse_out_header*>(resp.data());
      void* out_payload = out + 1;
      memset(resp.data(), 0, resp.size());
      out->unique = in->unique;
      size_t len = 0;
      switch (in->opcode) {
        case FUSE_LOOKUP: {
          auto* entry = reinterpret_cast<fuse_entry_out*>(out_payload);
          entry->nodeid = kFileNode;
          entry->entry_valid = entry->attr_valid = 3600;
          fill_attr(kFileNode, &entry->attr);
          len = sizeof(*entry);
          break;
        }
        case FUSE_GETATTR: {
          auto* attr = reinterpret_cast<fuse_attr_out*>(out_payload);
          attr->attr_valid = 3600;
          fill_attr(in->nodeid, &attr->attr);
          len = sizeof(*attr);
          break;
        }
        case FUSE_OPEN: {
          auto* open = reinterpret_cast<fuse_open_out*>(out_payload);
          open->fh = 1;
          open->open_flags = FOPEN_KEEP_CACHE;
          len = sizeof(*open);
          break;
        }
        case FUSE_READ: {
          if (reads++ == 0) {
            first_read.Notify();
            release_first_read.WaitForNotification();
          }
          auto* read_in = reinterpret_cast<fuse_read_in*>(in_payload);
          len = read_in->offset < kFileSize
                    ? std::min<size_t>(read_in->size,
                                       kFileSize - read_in->offset)
                    : 0;
          memset(out_payload, 'x', len);
          break;
        }
        case FUSE_ACCESS:
        case FUSE_FLUSH:
        case FUSE_RELEASE:
          break;
        case FUSE_INTERRUPT:
        case FUSE_FORGET:
        case FUSE_BATCH_FORGET:
          continue;  // No reply.
        default:
          out->error = -ENOSYS;
      }
      out->len = sizeof(*out) + len;
      // The reply to an interrupted request may be refused.
      write(fd.get(), resp.data(), out->len);
    }
  });
  // Stops the server, also if an assertion fails.
  Cleanup unmount([&] { umount(mount_point.path().c_str()); });

  static std::atomic<int> signals;
  signals = 0;
  struct sigaction sa = {};
  sa.sa_handler = +[](int) { signals++; };
  auto cleanup_sigaction =
      ASSERT_NO_ERRNO_AND_VALUE(ScopedSigaction(SIGUSR1, sa));
  {
    const FileDescriptor file = ASSERT_NO_ERRNO_AND_VALUE(
        Open(JoinPath(mount_point.path(), "file"), O_RDONLY));
    Mapping m = ASSERT_NO_ERRNO_AND_VALUE(
        Mmap(nullptr, kFileSize, PROT_READ, MAP_SHARED, file.get(), 0));
    std::atomic<pid_t> tid{0};
    std::atomic<char> got{0};
    ScopedThread faulter([&] {
      tid = syscall(SYS_gettid);
      got = *reinterpret_cast<volatile char*>(m.ptr());
    });
    first_read.WaitForNotification();
    ASSERT_THAT(syscall(SYS_tgkill, getpid(), tid.load(), SIGUSR1),
                SyscallSucceeds());
    // Give the signal time to arrive while the read is pending.
    absl::SleepFor(absl::Milliseconds(100));
    release_first_read.Notify();
    faulter.Join();
    EXPECT_EQ(got, 'x');
    EXPECT_EQ(signals, 1);
  }
}

// A server without xattr support answers xattr requests with ENOSYS. Like
// Linux's fs/fuse/xattr.c, that is reported as EOPNOTSUPP (which ls and
// libacl treat as "no xattrs") and latched per request type, so later calls
// send no request.
TEST(FuseTest, XattrENOSYSIsEOPNOTSUPPAndLatched) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const DisableSave ds;  // The server thread must not be paused by a save.
  const FileDescriptor fd =
      ASSERT_NO_ERRNO_AND_VALUE(Open("/dev/fuse", O_RDWR));
  auto mount_point = ASSERT_NO_ERRNO_AND_VALUE(TempPath::CreateDir());
  auto mount_opts =
      absl::StrFormat("fd=%d,user_id=0,group_id=0,rootmode=40755", fd.get());
  ASSERT_THAT(mount("fuse", mount_point.path().c_str(), "fuse",
                    MS_NODEV | MS_NOSUID, mount_opts.c_str()),
              SyscallSucceeds());
  FuseInit(fd.get());

  std::atomic<int> getxattr_reqs{0}, listxattr_reqs{0}, setxattr_reqs{0},
      removexattr_reqs{0};
  ScopedThread server([&] {
    std::vector<char> req(FUSE_MIN_READ_BUFFER);
    for (;;) {
      ssize_t n = read(fd.get(), req.data(), req.size());
      if (n < 0 && errno == EINTR) {
        continue;
      }
      if (n < 0) {
        return;  // ENODEV after umount.
      }
      auto* in = reinterpret_cast<fuse_in_header*>(req.data());
      struct {
        fuse_out_header hdr;
        fuse_attr_out attr;
      } resp = {};
      resp.hdr.unique = in->unique;
      resp.hdr.len = sizeof(resp.hdr);
      switch (in->opcode) {
        case FUSE_GETATTR:
          resp.attr.attr_valid = 3600;
          resp.attr.attr.ino = in->nodeid;
          resp.attr.attr.mode = S_IFDIR | 0755;
          resp.attr.attr.nlink = 2;
          resp.hdr.len = sizeof(resp);
          break;
        case FUSE_ACCESS:
          break;
        case FUSE_GETXATTR:
          getxattr_reqs++;
          resp.hdr.error = -ENOSYS;
          break;
        case FUSE_LISTXATTR:
          listxattr_reqs++;
          resp.hdr.error = -ENOSYS;
          break;
        case FUSE_SETXATTR:
          setxattr_reqs++;
          resp.hdr.error = -ENOSYS;
          break;
        case FUSE_REMOVEXATTR:
          removexattr_reqs++;
          resp.hdr.error = -ENOSYS;
          break;
        case FUSE_FORGET:
        case FUSE_BATCH_FORGET:
        case FUSE_INTERRUPT:
          continue;  // No reply.
        default:
          resp.hdr.error = -ENOSYS;
      }
      write(fd.get(), &resp, resp.hdr.len);
    }
  });
  // Stops the server, also if an assertion fails.
  Cleanup unmount([&] { umount(mount_point.path().c_str()); });

  const char* path = mount_point.path().c_str();
  char buf[64];
  for (int i = 0; i < 2; i++) {
    EXPECT_THAT(getxattr(path, "user.a", buf, sizeof(buf)),
                SyscallFailsWithErrno(EOPNOTSUPP));
    // gVisor's VFS, unlike Linux's fuse_listxattr(), reports EOPNOTSUPP from
    // listxattr as an empty list.
    EXPECT_THAT(listxattr(path, buf, sizeof(buf)),
                AnyOf(SyscallSucceedsWithValue(0),
                      SyscallFailsWithErrno(EOPNOTSUPP)));
    EXPECT_THAT(setxattr(path, "user.a", "v", 1, 0),
                SyscallFailsWithErrno(EOPNOTSUPP));
    EXPECT_THAT(removexattr(path, "user.a"),
                SyscallFailsWithErrno(EOPNOTSUPP));
  }
  EXPECT_EQ(getxattr_reqs, 1);
  EXPECT_EQ(listxattr_reqs, 1);
  EXPECT_EQ(setxattr_reqs, 1);
  EXPECT_EQ(removexattr_reqs, 1);
}

// A server that answers FUSE_OPENDIR with ENOSYS (FUSE_NO_OPENDIR_SUPPORT) is
// sent no more OPENDIR or RELEASEDIR. As on Linux, such directories default
// to FOPEN_CACHE_DIR: a listing is read from the server once and kept until a
// change made through the mount.
TEST(FuseTest, NoOpendirCachesListing) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const DisableSave ds;  // The server thread must not be paused by a save.
  std::atomic<int> opendirs{0}, releasedirs{0}, readdirs{0};
  std::atomic<bool> made_b{false};
  auto server = ASSERT_NO_ERRNO_AND_VALUE(FuseServer::Mount(
      [&](const fuse_in_header& in, const char* in_payload,
          char* out) -> ssize_t {
        switch (in.opcode) {
          case FUSE_GETATTR:
            return ReplyAttr(out, in.nodeid, S_IFDIR | 0755, 0, 2);
          case FUSE_LOOKUP:
            return -ENOENT;
          case FUSE_MKDIR:
            made_b = true;
            return ReplyEntry(out, 3, S_IFDIR | 0755, 0);
          case FUSE_OPENDIR:
            opendirs++;
            return -ENOSYS;
          case FUSE_RELEASEDIR:
            releasedirs++;
            return 0;
          case FUSE_READDIR: {
            readdirs++;
            std::vector<std::string> names = {".", "..", "a"};
            if (made_b) {
              names.push_back("b");
            }
            auto* read_in = reinterpret_cast<const fuse_read_in*>(in_payload);
            size_t len = 0;
            for (size_t i = read_in->offset; i < names.size(); i++) {
              auto* d = reinterpret_cast<fuse_dirent*>(out + len);
              d->ino = i + 1;
              d->off = i + 1;
              d->namelen = names[i].size();
              d->type = names[i] == "a" ? DT_REG : DT_DIR;
              memcpy(d->name, names[i].data(), names[i].size());
              len += FUSE_DIRENT_SIZE(d);
            }
            return len;
          }
          case FUSE_ACCESS:
            return 0;
          case FUSE_FORGET:
          case FUSE_BATCH_FORGET:
          case FUSE_INTERRUPT:
            return kNoReply;
          default:
            return -ENOSYS;
        }
      }));

  for (int i = 0; i < 2; i++) {
    EXPECT_THAT(ASSERT_NO_ERRNO_AND_VALUE(ListDir(server->Path(""), false)),
                UnorderedElementsAre(".", "..", "a"));
  }
  EXPECT_EQ(opendirs, 1);
  EXPECT_EQ(releasedirs, 0);
  EXPECT_EQ(readdirs, 2);  // The entries, then the end of the directory.

  ASSERT_THAT(mkdir(server->Path("b").c_str(), 0755), SyscallSucceeds());
  EXPECT_THAT(ASSERT_NO_ERRNO_AND_VALUE(ListDir(server->Path(""), false)),
              UnorderedElementsAre(".", "..", "a", "b"));
  EXPECT_EQ(readdirs, 4);
}

// A FUSE_LOOKUP reply with nodeid 0 is a negative entry: as on Linux, the name
// doesn't exist (ENOENT) and the kernel caches that for entry_valid, until the
// name is created through the mount. An ENOENT reply is not cached.
TEST(FuseTest, NegativeEntryCached) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const DisableSave ds;  // The server thread must not be paused by a save.
  std::atomic<int> lookups_x{0}, lookups_y{0};
  auto server = ASSERT_NO_ERRNO_AND_VALUE(FuseServer::Mount(
      [&](const fuse_in_header& in, const char* in_payload,
          char* out) -> ssize_t {
        switch (in.opcode) {
          case FUSE_GETATTR:
            return ReplyAttr(out, in.nodeid, S_IFDIR | 0755, 0, 2);
          case FUSE_LOOKUP:
            if (std::string(in_payload) == "x") {
              lookups_x++;
              auto* entry = reinterpret_cast<fuse_entry_out*>(out);
              entry->entry_valid = 3600;  // nodeid 0
              return sizeof(*entry);
            }
            lookups_y++;
            return -ENOENT;
          case FUSE_MKDIR:
            return ReplyEntry(out, 3, S_IFDIR | 0755, 0);
          case FUSE_ACCESS:
            return 0;
          case FUSE_FORGET:
          case FUSE_BATCH_FORGET:
          case FUSE_INTERRUPT:
            return kNoReply;
          default:
            return -ENOSYS;
        }
      }));

  struct stat st;
  for (int i = 0; i < 2; i++) {
    EXPECT_THAT(stat(server->Path("x").c_str(), &st),
                SyscallFailsWithErrno(ENOENT));
    EXPECT_THAT(stat(server->Path("y").c_str(), &st),
                SyscallFailsWithErrno(ENOENT));
  }
  EXPECT_EQ(lookups_x, 1);
  EXPECT_EQ(lookups_y, 2);

  ASSERT_THAT(mkdir(server->Path("x").c_str(), 0755), SyscallSucceeds());
  ASSERT_THAT(stat(server->Path("x").c_str(), &st), SyscallSucceeds());
  EXPECT_TRUE(S_ISDIR(st.st_mode));
  EXPECT_EQ(lookups_x, 1);
}

// A directory's attributes are fetched again after a change made through the
// mount, as Linux's fuse_dir_changed() does, although attr_valid has not run
// out.
TEST(FuseTest, DirAttributesRefetchedAfterChange) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const DisableSave ds;  // The server thread must not be paused by a save.
  std::atomic<int> getattrs{0};
  std::atomic<bool> made_d{false};
  auto server = ASSERT_NO_ERRNO_AND_VALUE(FuseServer::Mount(
      [&](const fuse_in_header& in, const char* in_payload,
          char* out) -> ssize_t {
        switch (in.opcode) {
          case FUSE_GETATTR:
            getattrs++;
            return ReplyAttr(out, in.nodeid, S_IFDIR | 0755, 0,
                             in.nodeid == FUSE_ROOT_ID && made_d ? 3 : 2);
          case FUSE_LOOKUP:
            return -ENOENT;
          case FUSE_MKDIR:
            made_d = true;
            return ReplyEntry(out, 3, S_IFDIR | 0755, 0);
          case FUSE_ACCESS:
            return 0;
          case FUSE_FORGET:
          case FUSE_BATCH_FORGET:
          case FUSE_INTERRUPT:
            return kNoReply;
          default:
            return -ENOSYS;
        }
      }));

  const std::string root = server->Path("");
  EXPECT_EQ(ASSERT_NO_ERRNO_AND_VALUE(Stat(root)).st_nlink, 2);
  const int before = getattrs;
  EXPECT_EQ(ASSERT_NO_ERRNO_AND_VALUE(Stat(root)).st_nlink, 2);
  EXPECT_EQ(getattrs, before);  // attr_valid holds

  ASSERT_THAT(mkdir(server->Path("d").c_str(), 0755), SyscallSucceeds());
  EXPECT_EQ(ASSERT_NO_ERRNO_AND_VALUE(Stat(root)).st_nlink, 3);
}

// st_ino is the inode number in the server's attributes, as on Linux, also
// when it differs from the nodeid and the attributes come from the cache.
TEST(FuseTest, StatReportsServerInodeNumber) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const DisableSave ds;  // The server thread must not be paused by a save.
  constexpr uint64_t kIno = 77;
  auto server = ASSERT_NO_ERRNO_AND_VALUE(FuseServer::Mount(
      [&](const fuse_in_header& in, const char* in_payload,
          char* out) -> ssize_t {
        switch (in.opcode) {
          case FUSE_LOOKUP: {
            ssize_t len = ReplyEntry(out, 2, S_IFREG | 0644, 0);
            reinterpret_cast<fuse_entry_out*>(out)->attr.ino = kIno;
            return len;
          }
          case FUSE_GETATTR: {
            ssize_t len = ReplyAttr(out, in.nodeid, S_IFDIR | 0755, 0, 2);
            if (in.nodeid != FUSE_ROOT_ID) {
              len = ReplyAttr(out, kIno, S_IFREG | 0644, 0, 1);
            }
            return len;
          }
          case FUSE_ACCESS:
            return 0;
          case FUSE_FORGET:
          case FUSE_BATCH_FORGET:
          case FUSE_INTERRUPT:
            return kNoReply;
          default:
            return -ENOSYS;
        }
      }));
  for (int i = 0; i < 2; i++) {
    EXPECT_EQ(ASSERT_NO_ERRNO_AND_VALUE(Stat(server->Path("f"))).st_ino, kIno);
  }
}

// Renaming a file over another drops the replaced file's cached attributes,
// as Linux's fuse_rename_common() => fuse_entry_unlinked() does: a file
// descriptor on the replaced file reports nlink 0 although attr_valid has not
// run out.
TEST(FuseTest, RenameOverRefetchesReplacedAttributes) {
  SKIP_IF(!ASSERT_NO_ERRNO_AND_VALUE(HaveCapability(CAP_SYS_ADMIN)));
  const DisableSave ds;  // The server thread must not be paused by a save.
  constexpr uint64_t kSrcNode = 2, kDstNode = 3;
  std::atomic<bool> renamed{false};
  auto server = ASSERT_NO_ERRNO_AND_VALUE(FuseServer::Mount(
      [&](const fuse_in_header& in, const char* in_payload,
          char* out) -> ssize_t {
        switch (in.opcode) {
          case FUSE_LOOKUP:
            if (std::string(in_payload) == "src") {
              return ReplyEntry(out, kSrcNode, S_IFREG | 0644, 0);
            }
            if (std::string(in_payload) == "dst") {
              return ReplyEntry(out, kDstNode, S_IFREG | 0644, 0);
            }
            return -ENOENT;
          case FUSE_GETATTR:
            if (in.nodeid == FUSE_ROOT_ID) {
              return ReplyAttr(out, in.nodeid, S_IFDIR | 0755, 0, 2);
            }
            return ReplyAttr(out, in.nodeid, S_IFREG | 0644, 0,
                             in.nodeid == kDstNode && renamed ? 0 : 1);
          case FUSE_OPEN: {
            auto* open = reinterpret_cast<fuse_open_out*>(out);
            open->fh = 1;
            return sizeof(*open);
          }
          case FUSE_RENAME:
            renamed = true;
            return 0;
          case FUSE_ACCESS:
          case FUSE_FLUSH:
          case FUSE_RELEASE:
            return 0;
          case FUSE_FORGET:
          case FUSE_BATCH_FORGET:
          case FUSE_INTERRUPT:
            return kNoReply;
          default:
            return -ENOSYS;
        }
      }));

  const FileDescriptor dst =
      ASSERT_NO_ERRNO_AND_VALUE(Open(server->Path("dst"), O_RDONLY));
  EXPECT_EQ(ASSERT_NO_ERRNO_AND_VALUE(Fstat(dst.get())).st_nlink, 1);
  ASSERT_THAT(rename(server->Path("src").c_str(), server->Path("dst").c_str()),
              SyscallSucceeds());
  EXPECT_EQ(ASSERT_NO_ERRNO_AND_VALUE(Fstat(dst.get())).st_nlink, 0);
}

}  // namespace
}  // namespace testing
}  // namespace gvisor
