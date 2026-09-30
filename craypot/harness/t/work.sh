#!/bin/bash
# In-sandbox FUSE workload. Usage: work.sh <mode> where mode is
#   bindfs     bindfs /srv/b -> /w (libfuse, no kernel_cache => no FOPEN_KEEP_CACHE)
#   bindfs-kc  same with -o kernel_cache (FOPEN_KEEP_CACHE)
#   agentfs    AgentFS mount at /w
#   rootfs     plain directory /w (baseline)
# Prints "RESULT <name> PASS|FAIL [detail]" lines.
MODE=$1; W=/w; B=
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin HOME=/root
res() { echo "RESULT $1 $2 ${3:-}"; }
chk() { local n=$1; shift; if out=$("$@" 2>&1); then res "$n" PASS; else res "$n" FAIL "$(echo "$out" | tail -2 | tr '\n' ' ')"; fi; }
umount -l $W 2>/dev/null; pkill -x bindfs; pkill -x agentfs; sleep 0.2
rm -rf $W /srv/b /var/lib/agentfs; mkdir -p $W /srv/b
case $MODE in
bindfs) bindfs /srv/b $W; B=/srv/b ;;
bindfs-kc) bindfs -o kernel_cache /srv/b $W; B=/srv/b ;;
agentfs) mkdir -p /var/lib/agentfs && cd /var/lib/agentfs && agentfs init ws >/dev/null && cd / \
  && (agentfs mount /var/lib/agentfs/.agentfs/ws.db $W --foreground --system >/var/log/agentfs.log 2>&1 &) \
  && for i in $(seq 50); do mountpoint -q $W && break; sleep 0.1; done ;;
rootfs) ;;
esac
[ "$MODE" = rootfs ] || { mountpoint -q $W && res mount PASS "$(grep " $W " /proc/mounts)" || { res mount FAIL; exit 1; }; }
cd $W

# exec
printf '#!/bin/sh\necho ok\n' > s.sh && chmod 755 s.sh; chk exec_script ./s.sh
cp /bin/true t && chk exec_elf ./t

# mmap coherence probe (compiled on rootfs, runs against the mount)
gcc -O1 -o /tmp/mmaptest /opt/t/mmaptest.c; mkdir -p mm
/tmp/mmaptest $W/mm $B${B:+/mm} > /tmp/mm.out 2>&1
while read -r st name rest; do [ "$st" = PASS ] || [ "$st" = FAIL ] && res "mm.$name" "$st" "$rest"; done < /tmp/mm.out

# stock git
chk git_clone git clone -q /opt/click $W/click
cd $W/click 2>/dev/null && {
  chk git_status git status --short
  echo "# change" >> README.md; chk git_add git add -A; chk git_commit git commit -qm change
  chk git_log git log --oneline -3; chk git_gc git gc -q; chk git_fsck git fsck --no-progress
  chk git_checkout git checkout -q HEAD~1; chk git_status2 git status --short; cd $W; }

# python venv + C extension (dlopen) + console script exec
chk venv python3 -m venv $W/venv
mkdir -p $W/ext && cat > $W/ext/hello.c <<'EOF'
#include <Python.h>
static PyObject *hi(PyObject *s, PyObject *a) { return PyUnicode_FromString("c-ext-ok"); }
static PyMethodDef M[] = {{"hi", hi, METH_NOARGS, 0}, {0}};
static struct PyModuleDef D = {PyModuleDef_HEAD_INIT, "hello", 0, -1, M};
PyMODINIT_FUNC PyInit_hello(void) { return PyModule_Create(&D); }
EOF
chk cext_build gcc -shared -fPIC $(python3-config --includes) -o $W/ext/hello$(python3-config --extension-suffix) $W/ext/hello.c
chk cext_import sh -c "cd $W/ext && $W/venv/bin/python -c 'import hello; assert hello.hi()==\"c-ext-ok\"'"
chk venv_console_script $W/venv/bin/pip --version

# sqlite WAL: writer + concurrent reader in separate processes, checkpoint
chk sqlite_wal python3 /opt/t/wal.py $W/wal.db

# gcc build with outputs on the mount
mkdir -p $W/cbuild && printf 'int f(void){return 41;}\n' > $W/cbuild/f.c && printf 'int f(void);\nint main(void){return f()==41?0:1;}\n' > $W/cbuild/m.c
chk gcc_build sh -c "cd $W/cbuild && gcc -c f.c && gcc -c m.c && ar rcs libf.a f.o && gcc -o m m.o -L. -lf && ./m"

# cargo build with target dir on the mount
chk cargo_build sh -c "cd $W && cargo new -q --vcs none hellors && cd hellors && cargo build -q --offline && ./target/debug/hellors | grep -q Hello"

# unmount: server should exit
cd /
if [ "$MODE" != rootfs ]; then
  umount $W; r=$?; sleep 1
  if ps -eo stat=,comm= | awk '($2=="bindfs"||$2=="agentfs") && $1 !~ /^Z/' | grep -q .; then res umount_server_exits FAIL "umount rc=$r, server alive"; else res umount_server_exits PASS "umount rc=$r"; fi
fi
