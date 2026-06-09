#!/bin/bash
set -euo pipefail

INPUT_DIR="${INPUT_DIR:-/data/output}"

echo "==> Waiting for download to finish..."
while [ ! -f "$INPUT_DIR/.done" ]; do
    sleep 10
done
echo "==> Download complete, starting sync"
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
    format=$(find "$tiledir" -type f -name '*.*' -print -quit | sed 's/.*\.//')
    if [ -z "$format" ]; then
        echo "    Skipping: no tile files found"
        return
    fi

    echo "    Converting to MBTiles (format: $format)..."
    mb-util --image_format="$format" --scheme=xyz "$tiledir" "$mbtiles_file"

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
