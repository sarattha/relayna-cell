#!/usr/bin/env python3
"""Export committed audit rows for Alloy replay. Run inside the VM as root.

Pause Cell work before replaying large histories. Duplicate deliveries retain
the original event_id; Loki is a searchable projection of the SQLite journal.
"""
import argparse
import os
import pathlib
import sqlite3
import subprocess
import uuid

p = argparse.ArgumentParser()
p.add_argument('--after', type=int, default=0)
args = p.parse_args()
if subprocess.run(['systemctl', 'is-active', '--quiet', 'cell-server']).returncode == 0:
    raise SystemExit('Stop cell-server before replaying so the journal budget and export are consistent.')
root = pathlib.Path('/var/lib/relayna-cell')
db = sqlite3.connect(f'file:{root}/cell.db?mode=ro', uri=True)
size = db.execute('SELECT COALESCE(SUM(length(body)+1),0) FROM events WHERE seq>?', (args.after,)).fetchone()[0]
used = sum(f.stat().st_size for f in (root/'logs').iterdir())
if used + size > 256*1024*1024:
    raise SystemExit('Replay would exceed the journal budget; archive old export segments first.')
target = root/'logs'/f'replay-{uuid.uuid4().hex}.ndjson'
temp = target.with_suffix('.tmp')
try:
    with temp.open('xb') as f:
        for (body,) in db.execute('SELECT body FROM events WHERE seq>? ORDER BY seq', (args.after,)):
            f.write(body if isinstance(body, bytes) else body.encode())
            f.write(b'\n')
        f.flush()
        os.fsync(f.fileno())
    temp.chmod(0o644)
    temp.rename(target)
finally:
    temp.unlink(missing_ok=True)
print(target)
