// mmap/FUSE coherence probe. Usage: mmaptest <dir-on-mount> [backing-dir]
// Prints one "PASS name" / "FAIL name: why" line per check.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <setjmp.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

static const char *dir, *backing;
static int fails;
#define PS 4096

static void result(const char *name, int ok, const char *why) {
  if (ok) printf("PASS %s\n", name);
  else { printf("FAIL %s: %s (errno %d %s)\n", name, why, errno, strerror(errno)); fails++; }
  fflush(stdout);
}

static char *path(const char *base, const char *name) {
  static char buf[2][4096]; static int k; k ^= 1;
  snprintf(buf[k], sizeof buf[k], "%s/%s", base, name);
  return buf[k];
}

static int mkfile(const char *name, size_t size, char fill) {
  int fd = open(path(dir, name), O_RDWR | O_CREAT | O_TRUNC, 0644);
  if (fd < 0) return -1;
  char *b = malloc(size); memset(b, fill, size);
  if (write(fd, b, size) != (ssize_t)size) { free(b); close(fd); return -1; }
  free(b);
  return fd;
}

static sigjmp_buf jb;
static void onbus(int s) { (void)s; siglongjmp(jb, 1); }

int main(int argc, char **argv) {
  if (argc < 2) return 2;
  dir = argv[1]; backing = argc > 2 ? argv[2] : NULL;
  signal(SIGBUS, onbus);

  { // write() then mmap read
    int fd = mkfile("a", 3 * PS, 'a');
    char *m = mmap(NULL, 3 * PS, PROT_READ, MAP_SHARED, fd, 0);
    int ok = m != MAP_FAILED && m[0] == 'a' && m[3 * PS - 1] == 'a';
    result("write_then_mmap_read", ok, "content");
    if (ok) {
      pwrite(fd, "XYZ", 3, PS + 10);
      result("pwrite_visible_in_existing_mapping", memcmp(m + PS + 10, "XYZ", 3) == 0, "stale mapping");
      munmap(m, 3 * PS);
    }
    close(fd);
  }
  { // mmap shared write then read()
    int fd = mkfile("b", 2 * PS, 'b');
    char *m = mmap(NULL, 2 * PS, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    int ok = m != MAP_FAILED;
    result("mmap_shared_rw", ok, "mmap");
    if (ok) {
      memcpy(m + 100, "hello", 5);
      char buf[5] = {0};
      pread(fd, buf, 5, 100);
      result("mmap_write_visible_to_read", memcmp(buf, "hello", 5) == 0, "read saw old data");
      memcpy(m + PS + 1, "msync", 5);
      result("msync", msync(m, 2 * PS, MS_SYNC) == 0, "msync");
      if (backing) {
        int bfd = open(path(backing, "b"), O_RDONLY);
        char bb[5] = {0}; pread(bfd, bb, 5, PS + 1); close(bfd);
        result("msync_reaches_server", memcmp(bb, "msync", 5) == 0, "server lacks data after msync");
      }
      memcpy(m + 200, "unmap", 5);
      munmap(m, 2 * PS);
      if (backing) {
        int bfd = open(path(backing, "b"), O_RDONLY);
        char bb[5] = {0}; pread(bfd, bb, 5, 200); close(bfd);
        result("munmap_writes_back", memcmp(bb, "unmap", 5) == 0, "server lacks data after munmap");
      }
    }
    close(fd);
  }
  { // write, close without msync/munmap order: dirty then close then reopen
    int fd = mkfile("c", PS, 'c');
    char *m = mmap(NULL, PS, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    memcpy(m, "close", 5);
    close(fd); // mapping keeps file alive
    int fd2 = open(path(dir, "c"), O_RDONLY);
    char buf[5] = {0}; pread(fd2, buf, 5, 0); close(fd2);
    result("dirty_visible_after_close_via_new_fd", memcmp(buf, "close", 5) == 0, "lost");
    munmap(m, PS);
  }
  { // fork + MAP_SHARED
    int fd = mkfile("d", PS, 'd');
    char *m = mmap(NULL, PS, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
    pid_t p = fork();
    if (p == 0) { memcpy(m, "child", 5); _exit(0); }
    waitpid(p, NULL, 0);
    result("fork_shared_child_write_visible", memcmp(m, "child", 5) == 0, "parent mapping stale");
    munmap(m, PS); close(fd);
  }
  { // two independent processes (separate opens) share pages
    int fd = mkfile("e", PS, 'e'); close(fd);
    pid_t p = fork();
    if (p == 0) {
      int f = open(path(dir, "e"), O_RDWR);
      char *m = mmap(NULL, PS, PROT_READ | PROT_WRITE, MAP_SHARED, f, 0);
      memcpy(m, "other", 5); // no msync
      sleep(1);
      _exit(0);
    }
    usleep(300000);
    int f = open(path(dir, "e"), O_RDONLY);
    char *m = mmap(NULL, PS, PROT_READ, MAP_SHARED, f, 0);
    result("cross_process_shared_coherent", memcmp(m, "other", 5) == 0, "not coherent");
    waitpid(p, NULL, 0);
    munmap(m, PS); close(f);
  }
  { // MAP_PRIVATE writes stay private
    int fd = mkfile("f", PS, 'f');
    char *m = mmap(NULL, PS, PROT_READ | PROT_WRITE, MAP_PRIVATE, fd, 0);
    m[0] = 'P';
    char c; pread(fd, &c, 1, 0);
    result("private_write_not_visible", c == 'f', "private leaked");
    munmap(m, PS); close(fd);
  }
  { // truncate shrinks mapping: SIGBUS past EOF
    int fd = mkfile("g", 3 * PS, 'g');
    char *m = mmap(NULL, 3 * PS, PROT_READ, MAP_SHARED, fd, 0);
    volatile char c = m[2 * PS];
    ftruncate(fd, PS);
    int bus = 0;
    if (sigsetjmp(jb, 1) == 0) { c = m[2 * PS]; } else bus = 1;
    (void)c;
    result("truncate_sigbus_past_eof", bus, "no SIGBUS");
    result("truncate_keeps_head", m[0] == 'g', "head lost");
    ftruncate(fd, 3 * PS);
    result("regrow_reads_zero", m[2 * PS] == 0, "stale data after regrow");
    munmap(m, 3 * PS); close(fd);
  }
  { // extend with write, mapping of new page sees it
    int fd = mkfile("h", 10, 'h');
    char *m = mmap(NULL, 2 * PS, PROT_READ, MAP_SHARED, fd, 0);
    pwrite(fd, "tail", 4, PS + 5);
    int bus = 0; char got[4] = {0};
    if (sigsetjmp(jb, 1) == 0) memcpy(got, m + PS + 5, 4); else bus = 1;
    result("extend_visible_in_mapping", !bus && memcmp(got, "tail", 4) == 0, bus ? "SIGBUS" : "stale");
    result("hole_zero", m[100] == 0, "hole not zero");
    munmap(m, 2 * PS); close(fd);
  }
  { // PROT_EXEC mapping (dlopen-style)
    int fd = mkfile("x", PS, 0xc3);
    void *m = mmap(NULL, PS, PROT_READ | PROT_EXEC, MAP_PRIVATE, fd, 0);
    result("prot_exec_private", m != MAP_FAILED, "mmap exec");
    if (m != MAP_FAILED) munmap(m, PS);
    close(fd);
  }
  { // read of an unlinked-but-open file
    int fd = mkfile("u", 6, 'u');
    unlink(path(dir, "u"));
    char buf[4096]; ssize_t n = pread(fd, buf, sizeof buf, 0);
    result("read_unlinked_open", n == 6 && buf[0] == 'u', "read failed");
    char *m = mmap(NULL, PS, PROT_READ, MAP_SHARED, fd, 0);
    int ok = 0;
    if (m != MAP_FAILED) { if (sigsetjmp(jb, 1) == 0) ok = m[0] == 'u'; else errno = EFAULT; }
    result("mmap_unlinked_open", ok, "mmap failed or SIGBUS");
    close(fd);
  }
  { // statx mask
    struct statx sx;
    int r = statx(AT_FDCWD, path(dir, "a"), 0, STATX_BASIC_STATS, &sx);
    result("statx_mask_basic", r == 0 && (sx.stx_mask & STATX_BASIC_STATS) == STATX_BASIC_STATS && S_ISREG(sx.stx_mode), "mask");
  }
  { // rename a directory into its own subdirectory
    mkdir(path(dir, "r1"), 0755);
    char sub[4096]; snprintf(sub, sizeof sub, "%s/r1/r2", dir); mkdir(sub, 0755);
    char dst[4096]; snprintf(dst, sizeof dst, "%s/r1/r2/r3", dir);
    int r = rename(path(dir, "r1"), dst);
    result("rename_into_own_subdir_einval", r < 0 && errno == EINVAL, "rename allowed or wrong errno");
  }
  printf("%s %d failures\n", fails ? "SOME-FAILED" : "ALL-PASSED", fails);
  return fails != 0;
}
