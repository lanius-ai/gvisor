"""Hold FUSE mappings across checkpoint/restore.

Maps /w/cr.bin MAP_SHARED and dirties a page without msync, maps an ELF on
the mount (exec'd helper), then waits for /tmp/cr.go (created after restore),
checks the mapping, writes more, msyncs and verifies via read(2).
"""
import mmap
import os
import sys
import time

PS = 4096
p = "/w/cr.bin"
fd = os.open(p, os.O_RDWR | os.O_CREAT | os.O_TRUNC, 0o644)
os.write(fd, b"\0" * 8 * PS)
m = mmap.mmap(fd, 8 * PS, mmap.MAP_SHARED, mmap.PROT_READ | mmap.PROT_WRITE)
m[3 * PS : 3 * PS + 11] = b"before-ckpt"  # dirty, not synced
open("/tmp/cr.ready", "w").write(str(os.getpid()))
while not os.path.exists("/tmp/cr.go"):
    time.sleep(0.2)
out = []
out.append(("mapping_survived", m[3 * PS : 3 * PS + 11] == b"before-ckpt"))
out.append(("dirty_visible_to_read", os.pread(fd, 11, 3 * PS) == b"before-ckpt"))
m[5 * PS : 5 * PS + 13] = b"after-restore"
m.flush()
out.append(("msync_after_restore", os.pread(fd, 13, 5 * PS) == b"after-restore"))
m.close()
os.close(fd)
fd2 = os.open(p, os.O_RDONLY)
out.append(("new_fd_reads_both", os.pread(fd2, 11, 3 * PS) == b"before-ckpt" and os.pread(fd2, 13, 5 * PS) == b"after-restore"))
with open("/tmp/cr.result", "w") as f:
    for name, ok in out:
        f.write(f"RESULT cr.{name} {'PASS' if ok else 'FAIL'}\n")
sys.exit(0)
