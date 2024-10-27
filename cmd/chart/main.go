package main

import (
	"flag"
	"path/filepath"
)

func main() {
	var resultsDir string

	flag.StringVar(&resultsDir, "dir", "results", "results directory")
	flag.Parse()

	if err := generateRHPBox("RHP2", filepath.Join(resultsDir, "rhp2.csv"), filepath.Join(resultsDir, "rhp2.png")); err != nil {
		panic(err)
	}
	if err := generateRHPBox("RHP3", filepath.Join(resultsDir, "rhp3.csv"), filepath.Join(resultsDir, "rhp3.png")); err != nil {
		panic(err)
	}
	if err := generateRHPBox("RHP4", filepath.Join(resultsDir, "rhp4.csv"), filepath.Join(resultsDir, "rhp4.png")); err != nil {
		panic(err)
	}
	if err := generateE2EBox("End to End", filepath.Join(resultsDir, "e2e.csv"), filepath.Join(resultsDir, "e2e.png")); err != nil {
		panic(err)
	}
}
