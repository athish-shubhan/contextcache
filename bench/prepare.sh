#!/usr/bin/env bash
# Fetches the Berkeley Function Calling Leaderboard data (Apache-2.0) with a
# sparse checkout and builds data/catalog.json. Needs network once.
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p data
if [ ! -d data/gorilla ]; then
  git clone -q --depth 1 --filter=blob:none --sparse https://github.com/ShishirPatil/gorilla data/gorilla
  git -C data/gorilla sparse-checkout set berkeley-function-call-leaderboard/bfcl_eval/data
fi
D=$(dirname "$(find data/gorilla -name 'BFCL_v*_live_simple.json' -not -path '*possible_answer*' | head -1)")
echo "BFCL data: $D ($(git -C data/gorilla rev-parse --short HEAD))"
ln -sfn "$(realpath "$D")" data/bfcl
go build -o bin/ccbench ./cmd/ccbench
./bin/ccbench prepare -bfcl data/bfcl -out data/catalog.json
