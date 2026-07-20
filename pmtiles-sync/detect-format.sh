#!/bin/bash
set -euo pipefail

tiledir="${1:?tile directory is required}"
metadata="$tiledir/metadata.json"
format=""

if [ -f "$metadata" ]; then
    format=$(python3 -c 'import json, sys; print(json.load(open(sys.argv[1], encoding="utf-8")).get("format", ""))' "$metadata")
fi

if [ -z "$format" ]; then
    first_tile=$(find "$tiledir" -mindepth 3 -type f -name '*.*' -print -quit)
    if [ -n "$first_tile" ]; then
        format="${first_tile##*.}"
    fi
fi

printf '%s\n' "$format"
