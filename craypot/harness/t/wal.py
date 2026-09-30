import os
import sqlite3
import subprocess
import sys

db = sys.argv[1]
c = sqlite3.connect(db, timeout=10)
c.execute("PRAGMA journal_mode=WAL")
c.execute("CREATE TABLE t(x)")
c.executemany("INSERT INTO t VALUES(?)", [(i,) for i in range(5000)])
c.commit()
assert os.path.exists(db + "-shm"), "no -shm"
reader = (
    "import sqlite3,sys,time;c=sqlite3.connect(sys.argv[1]);c.execute('BEGIN');"
    "n=c.execute('SELECT count(*) FROM t').fetchone()[0];print(n,flush=True);time.sleep(1);"
    "assert c.execute('SELECT count(*) FROM t').fetchone()[0]==n;c.commit()"
)
r = subprocess.Popen([sys.executable, "-c", reader, db], stdout=subprocess.PIPE, text=True)
assert r.stdout.readline().strip() == "5000"
c.execute("INSERT INTO t VALUES(-1)")  # writer proceeds while the reader holds a snapshot
c.commit()
assert r.wait() == 0, "reader snapshot"
c.execute("PRAGMA wal_checkpoint(TRUNCATE)")
assert c.execute("SELECT count(*) FROM t").fetchone()[0] == 5001
assert c.execute("PRAGMA integrity_check").fetchone()[0] == "ok"
print("wal ok")
