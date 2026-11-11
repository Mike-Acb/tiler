#!/bin/bash
set -euo pipefail

mbtiles="${1:?mbtiles file is required}"

python3 - "$mbtiles" <<'PY'
import sqlite3
import sys

db = sqlite3.connect(sys.argv[1])
tables = {row[0] for row in db.execute("select name from sqlite_master where type = 'table'")}
if "tiles" not in tables:
    sys.exit(f"tiles table not found in {sys.argv[1]}")
removed = db.execute("delete from tiles where tile_data is null or length(tile_data) = 0").rowcount
db.commit()
db.close()
print(f"    Removed {removed} empty tiles")
PY
