package main

import (
	"context"
	"flag"
	"path/filepath"

	"github.com/chromedp/chromedp"
)

func main() {
	var resultsDir string

	flag.StringVar(&resultsDir, "dir", "results", "results directory")
	flag.Parse()

	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()

	if err := generateRHPBox(ctx, "RHP4 SiaMux", filepath.Join(resultsDir, "rhp4.csv"), filepath.Join(resultsDir, "rhp4.png")); err != nil {
		panic(err)
	}
	if err := generateRHPBox(ctx, "RHP4 QUIC", filepath.Join(resultsDir, "rhp4.quic.csv"), filepath.Join(resultsDir, "rhp4.quic.png")); err != nil {
		panic(err)
	}
	if err := generateE2EBox(ctx, "End to End", filepath.Join(resultsDir, "e2e.csv"), filepath.Join(resultsDir, "e2e.png")); err != nil {
		panic(err)
	}
}
