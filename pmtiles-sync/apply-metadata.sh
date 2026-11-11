#!/bin/bash
set -euo pipefail

# apply-metadata.sh <mbtiles>：把环境变量 TILE_METADATA（JSON 对象）写进 mbtiles 的 metadata 表。
# 旧版 tiler 下载的目录没有 metadata.json，mb-util 导出的 mbtiles 就没有名称、格式和范围，
# pmtiles convert 打出来的文件也跟着缺；控制台的打包 Job 会按图层配置生成这份元数据传进来。
mbtiles="${1:?mbtiles file is required}"

python3 - "$mbtiles" <<'PY'
import json
import os
import sqlite3
import sys

meta = json.loads(os.environ["TILE_METADATA"])
if not isinstance(meta, dict):
    sys.exit("TILE_METADATA must be a JSON object")
db = sqlite3.connect(sys.argv[1])
db.execute("create table if not exists metadata (name text, value text)")
for k, v in meta.items():
    db.execute("delete from metadata where name = ?", (k,))
    db.execute("insert into metadata (name, value) values (?, ?)", (k, str(v)))
db.commit()
db.close()
print("    Applied metadata: " + ", ".join(f"{k}={v}" for k, v in sorted(meta.items())))
PY
