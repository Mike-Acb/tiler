#!/bin/bash
set -euo pipefail

INPUT_DIR="${INPUT_DIR:-/data/output}"
MINIO_ENDPOINT="${MINIO_ENDPOINT:?MINIO_ENDPOINT is required}"
MINIO_ACCESS_KEY="${MINIO_ACCESS_KEY:?MINIO_ACCESS_KEY is required}"
MINIO_SECRET_KEY="${MINIO_SECRET_KEY:?MINIO_SECRET_KEY is required}"
MINIO_BUCKET="${MINIO_BUCKET:-open}"
WORK_DIR="/tmp/pmtiles-work"

echo "==> Configuring MinIO client..."
mc alias set store "$MINIO_ENDPOINT" "$MINIO_ACCESS_KEY" "$MINIO_SECRET_KEY"
mc mb --ignore-existing "store/${MINIO_BUCKET}"

mkdir -p "$WORK_DIR"

shopt -s nullglob
dirs=("$INPUT_DIR"/*/)
if [ ${#dirs[@]} -eq 0 ]; then
    echo "No tile directories found in $INPUT_DIR"
    exit 0
fi

for tiledir in "${dirs[@]}"; do
    name=$(basename "$tiledir")
    safe_name=$(echo "$name" | tr ' ' '_')
    mbtiles_file="$WORK_DIR/${safe_name}.mbtiles"
    pmtiles_file="$WORK_DIR/${safe_name}.pmtiles"

    echo "==> Processing: $name"

    format=$(find "$tiledir" -type f -name '*.*' | head -1 | sed 's/.*\.//')
    if [ -z "$format" ]; then
        echo "    Skipping: no tile files found"
        continue
    fi

    echo "    Converting to MBTiles (format: $format)..."
    mb-util --image_format="$format" --scheme=xyz "$tiledir" "$mbtiles_file"

    echo "    Converting to PMTiles..."
    pmtiles convert "$mbtiles_file" "$pmtiles_file"

    echo "    Uploading to MinIO..."
    mc cp "$pmtiles_file" "store/${MINIO_BUCKET}/${safe_name}.pmtiles"

    rm -f "$mbtiles_file" "$pmtiles_file"
    echo "    Done: ${safe_name}.pmtiles -> ${MINIO_BUCKET}"
done

echo "==> All tilesets synced successfully"
