#!/bin/bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
INPUT_DIR="${INPUT_DIR:-/data/output}"

# 与下载容器并行启动时靠 .done 判断下载结束；单独打包（如 tiler-console 的打包 Job）设 WAIT_FOR_DONE=0 跳过
if [ "${WAIT_FOR_DONE:-1}" = "1" ]; then
    echo "==> Waiting for download to finish..."
    while [ ! -f "$INPUT_DIR/.done" ]; do
        sleep 10
    done
    echo "==> Download complete, starting sync"
else
    echo "==> WAIT_FOR_DONE=0, starting sync directly"
fi
MINIO_ENDPOINT="${MINIO_ENDPOINT:?MINIO_ENDPOINT is required}"
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:?MINIO_ACCESS_KEY is required}"
MINIO_SECRET_KEY="${MINIO_SECRET_KEY:?MINIO_SECRET_KEY is required}"
MINIO_BUCKET="${MINIO_BUCKET:-open}"
MINIO_PATH="${MINIO_PATH:-}"
TILE_NAME="${TILE_NAME:-}"
WORK_DIR="/tmp/pmtiles-work"

echo "==> Configuring MinIO client..."
mc alias set store "$MINIO_ENDPOINT" "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY"
mc mb --ignore-existing "store/${MINIO_BUCKET}"

mkdir -p "$WORK_DIR"

process_dir() {
    local tiledir="$1"
    local name
    name=$(basename "$tiledir")
    local safe_name
    safe_name=$(echo "$name" | tr ' ' '_')
    local mbtiles_file="$WORK_DIR/${safe_name}.mbtiles"
    local pmtiles_file="$WORK_DIR/${safe_name}.pmtiles"

    echo "==> Processing: $name"

    local format
    format=$("$SCRIPT_DIR/detect-format.sh" "$tiledir")
    if [ -z "$format" ]; then
        echo "    Skipping: no tile files found"
        return
    fi

    echo "    Converting to MBTiles (format: $format)..."
    # 并发读取瓦片（目录多在 NFS 上，逐张读太慢）；0 字节的 404 标记不写入，metadata.json 一并导入
    "$SCRIPT_DIR/dir2mbtiles.py" "$tiledir" "$mbtiles_file" "$format"

    # 目录里有 metadata.json 时已导入；没有（旧版 tiler 下载的）就用控制台传来的元数据补上
    if [ ! -f "$tiledir/metadata.json" ] && [ -n "${TILE_METADATA:-}" ]; then
        echo "    No metadata.json, applying TILE_METADATA..."
        "$SCRIPT_DIR/apply-metadata.sh" "$mbtiles_file"
    fi

    echo "    Converting to PMTiles..."
    pmtiles convert "$mbtiles_file" "$pmtiles_file"

    echo "    Uploading to MinIO..."
    local dest
    if [ -n "$MINIO_PATH" ]; then
        dest="store/${MINIO_BUCKET}/${MINIO_PATH}/${safe_name}.pmtiles"
    else
        dest="store/${MINIO_BUCKET}/${safe_name}.pmtiles"
    fi
    mc cp "$pmtiles_file" "$dest"

    rm -f "$mbtiles_file" "$pmtiles_file"
    echo "    Done: ${safe_name}.pmtiles -> $dest"
}

if [ -n "$TILE_NAME" ]; then
    tiledir="$INPUT_DIR/$TILE_NAME"
    if [ ! -d "$tiledir" ]; then
        echo "Directory not found: $tiledir"
        exit 1
    fi
    process_dir "$tiledir"
else
    shopt -s nullglob
    dirs=("$INPUT_DIR"/*/)
    if [ ${#dirs[@]} -eq 0 ]; then
        echo "No tile directories found in $INPUT_DIR"
        exit 0
    fi
    for tiledir in "${dirs[@]}"; do
        process_dir "$tiledir"
    done
fi

echo "==> All done"
