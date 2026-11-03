#!/bin/sh
# Deploy the committed files to Vercel production without git metadata, so
# Vercel does not try to match the commit email to a GitHub account.
set -eu

SRC="$(cd "$(dirname "$0")" && pwd)"
OUT="${TMPDIR:-/tmp}/page_post_deploy"

rm -rf "$OUT"
mkdir -p "$OUT"
git -C "$SRC" archive HEAD | tar -x -C "$OUT"
cp -R "$SRC/.vercel" "$OUT/"
rm -f "$OUT/deploy.sh"

cd "$OUT"
vercel --prod
