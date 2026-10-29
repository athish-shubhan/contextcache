// Command ccbench runs ContextCache's benchmarks.
//
//	ccbench prepare  -bfcl data/bfcl -out data/catalog.json
//	ccbench overload -catalog data/catalog.json -out results/overload.json
//	ccbench cache    -catalog data/catalog.json -out results/cache.json
//	ccbench latency  -out results/latency.json
package main

import (
	"fmt"
	"log"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ccbench prepare|overload|cache|latency|skills|pgvector [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "prepare":
		err = prepare(os.Args[2:])
	case "overload":
		err = overload(os.Args[2:])
	case "cache":
		err = cacheBench(os.Args[2:])
	case "latency":
		err = latency(os.Args[2:])
	case "skills":
		err = skills(os.Args[2:])
	case "pgvector":
		err = pgvectorBench(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
