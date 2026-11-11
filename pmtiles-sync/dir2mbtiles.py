#!/usr/bin/env python3
"""dir2mbtiles.py <瓦片目录> <mbtiles> <格式>：把 z/x/y.<格式> 目录转成 mbtiles，替代 mb-util。

瓦片目录在 NFS 上，打开读取一张文件要和服务端来回好几趟（实测约 100ms 一张），
mb-util 单线程逐张读，二十多万张要六七个小时。瓶颈是等待不是吞吐，所以这里多线程并发读。
行为与原来的 mb-util --scheme=xyz + drop-empty-tiles.sh 一致：
- 文件按 XYZ 命名，写入时 tile_row 翻成 TMS；
- 0 字节文件是 tiler 写的 404 标记，不写入；
- 目录里有 metadata.json 时导入 metadata 表。
"""
import json
import os
import sqlite3
import sys
import time
from collections import deque
from concurrent.futures import ThreadPoolExecutor

WORKERS = int(os.environ.get("PACK_WORKERS", "32"))


def list_tiles(root, ext):
    """列出 (z, x, y, path)；只读目录，不逐个 stat。"""
    for zd in os.scandir(root):
        if not (zd.is_dir() and zd.name.isdigit()):
            continue
        z = int(zd.name)
        for xd in os.scandir(zd.path):
            if not (xd.is_dir() and xd.name.isdigit()):
                continue
            x = int(xd.name)
            for f in os.scandir(xd.path):
                name, dot, e = f.name.rpartition(".")
                if dot and e == ext and name.isdigit():
                    yield z, x, int(name), f.path


def read(t):
    z, x, y, path = t
    with open(path, "rb") as fh:
        return z, x, y, fh.read()


def main():
    root, out, ext = sys.argv[1], sys.argv[2], sys.argv[3]
    if os.path.exists(out):
        os.remove(out)
    db = sqlite3.connect(out)
    db.execute("pragma journal_mode = off")
    db.execute("pragma synchronous = off")
    db.execute("create table metadata (name text, value text)")
    db.execute("create table tiles (zoom_level integer, tile_column integer, tile_row integer, tile_data blob)")

    meta_file = os.path.join(root, "metadata.json")
    if os.path.isfile(meta_file):
        with open(meta_file, encoding="utf-8") as fh:
            meta = json.load(fh)
        db.executemany("insert into metadata (name, value) values (?, ?)",
                       [(k, v if isinstance(v, str) else json.dumps(v)) for k, v in meta.items()])

    start = time.monotonic()
    written = empty = 0
    def results(pool):
        # 有界并发：最多 WORKERS*4 张在读，按提交顺序取结果，内存不随瓦片总数增长
        pending = deque()
        for t in list_tiles(root, ext):
            pending.append(pool.submit(read, t))
            if len(pending) >= WORKERS * 4:
                yield pending.popleft().result()
        while pending:
            yield pending.popleft().result()

    with ThreadPoolExecutor(WORKERS) as pool:
        for z, x, y, data in results(pool):
            if not data:
                empty += 1
                continue
            db.execute("insert into tiles values (?, ?, ?, ?)", (z, x, (1 << z) - 1 - y, data))
            written += 1
            if written % 10000 == 0:
                rate = written / max(time.monotonic() - start, 1e-6)
                print(f"    {written} tiles, {rate:.0f}/s", flush=True)

    db.execute("create unique index tile_index on tiles (zoom_level, tile_column, tile_row)")
    db.commit()
    db.close()
    secs = time.monotonic() - start
    print(f"    Wrote {written} tiles, skipped {empty} empty (404 markers) in {secs:.0f}s", flush=True)


if __name__ == "__main__":
    main()
