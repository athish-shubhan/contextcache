#!/usr/bin/env bash
# Reproduces results/. Needs network once (bench/prepare.sh), Postgres for
# the SQL store (DSN below), and `pip install mcp` for the conformance check.
# About 15 minutes on 2 vCPUs.
set -euo pipefail
cd "$(dirname "$0")/.."
DSN=${DSN:-postgres://postgres:postgres@127.0.0.1:5432/contextcache?sslmode=disable}
mkdir -p bin results
[ -f data/catalog.json ] || bench/prepare.sh
go build -o bin/ccbench ./cmd/ccbench
go build -o bin/ccgw ./cmd/ccgw
go build -o bin/toolfarm ./cmd/toolfarm
go build -o bin/memrevise ./cmd/memrevise
GLOVE=${GLOVE:-${HOME}/data/glove/glove100.gz}   # gensim-data release glove-wiki-gigaword-100 (GitHub)
[ -f "$GLOVE" ] || { mkdir -p "$(dirname "$GLOVE")"; curl -sSL -o "$GLOVE" https://github.com/RaRe-Technologies/gensim-data/releases/download/glove-wiki-gigaword-100/glove-wiki-gigaword-100.gz; }
./bin/ccbench overload -catalog data/catalog.json -glove "$GLOVE" -out results/overload.json 2> results/overload.log
./bin/ccbench pgvector -catalog data/catalog.json -glove "$GLOVE" -dsn "$DSN" -out results/pgvector.json 2> results/pgvector.log
./bin/ccbench skills   -catalog data/catalog.json -out results/skills.json 2> results/skills.log
./bin/ccbench cache    -catalog data/catalog.json -out results/cache.json 2> results/cache.log
./bin/ccbench latency  -catalog data/catalog.json -out results/latency.json 2> results/latency.log
./bin/memrevise -out results/memrevise.json -data data/memrevise -dsn "$DSN" -glove "$GLOVE" 2> results/memrevise.log
python3 bench/conformance.py data/catalog.json > results/conformance.json || true
python3 bench/plot.py results
