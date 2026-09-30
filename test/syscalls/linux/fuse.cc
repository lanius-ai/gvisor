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
#include <unistd.h>

#include <algorithm>
#include <atomic>
#include <cerrno>
#include <cstdint>
#include <cstdlib>
#include <cstring>
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

using ::testing::Ge;

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

}  // namespace
}  // namespace testing
}  // namespace gvisor
