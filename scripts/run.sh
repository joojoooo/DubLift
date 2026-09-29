#!/usr/bin/env sh
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
command -v ffmpeg >/dev/null || { echo 'Install FFmpeg first.' >&2; exit 1; }
command -v ffprobe >/dev/null || { echo 'Install ffprobe first (included with FFmpeg).' >&2; exit 1; }
go build -trimpath -o bin/dublift ./cmd/dublift
exec ./bin/dublift "$@"
