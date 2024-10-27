package main

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/opts"
	"github.com/go-echarts/snapshot-chromedp/render"
)

func median(data []float64) float64 {
	n := len(data)
	if n == 0 {
		return 0
	}

	mid := n / 2
	if n%2 == 0 {
		return (data[mid-1] + data[mid]) / 2
	}
	return data[mid]
}

func createBoxPlotData(data []float64) []float64 {
	n := len(data)
	if n == 0 {
		return nil
	} else if n < 3 {
		return []float64{data[0], data[0], data[0], data[0], data[0]}
	}

	slices.Sort(data)
	min := slices.Min(data)
	max := slices.Max(data)
	q1 := median(data[:n/2])
	q2 := median(data)
	q3 := median(data[n/2:])

	return []float64{
		min,
		q1,
		q2,
		q3,
		max,
	}
}

func normalizeGoVersion(v string) string {
	parts := strings.Split(v, "-")
	if len(parts) == 3 {
		return parts[2] // commit hash
	}
	return v
}

func generateRHPBox(title, inputPath, outputPath string) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	if _, err := r.Read(); err != nil {
		return fmt.Errorf("failed to read header: %w", err)
	}

	var versions []string
	uploadData := make(map[string][]float64)
	downloadData := make(map[string][]float64)
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("failed to read record: %w", err)
		}
		version := normalizeGoVersion(record[4])
		if uploadData[version] == nil {
			versions = append(versions, version)
		}

		uploadSpeed, err := strconv.ParseFloat(strings.Fields(record[5])[0], 64)
		if err != nil {
			return fmt.Errorf("failed to parse upload speed: %w", err)
		}
		uploadData[version] = append(uploadData[version], uploadSpeed)

		downloadSpeed, err := strconv.ParseFloat(strings.Fields(record[6])[0], 64)
		if err != nil {
			return fmt.Errorf("failed to parse download speed: %w", err)
		}
		downloadData[version] = append(downloadData[version], downloadSpeed)
	}

	var uploadSeries, downloadSeries []opts.BoxPlotData
	for _, version := range versions {
		uploadSeries = append(uploadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(uploadData[version]),
		})
		downloadSeries = append(downloadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(downloadData[version]),
		})
	}

	bp := charts.NewBoxPlot()
	bp.SetGlobalOptions(charts.WithInitializationOpts(opts.Initialization{
		Theme: "dark",
	}), charts.WithTitleOpts(opts.Title{
		Title: title,
	}), charts.WithAnimation(false))
	bp.SetXAxis(versions).
		AddSeries("Upload", uploadSeries).
		AddSeries("Download", downloadSeries)
	render.MakeChartSnapshot(bp.RenderContent(), outputPath)
	return nil
}

func generateE2EBox(title, inputPath, outputPath string) error {
	f, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	if _, err := r.Read(); err != nil {
		return fmt.Errorf("failed to read header: %w", err)
	}

	var versionPairs []string
	uploadData := make(map[string][]float64)
	downloadData := make(map[string][]float64)
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("failed to read record: %w", err)
		}
		hostdVersion, renterdVersion := normalizeGoVersion(record[4]), normalizeGoVersion(record[5])
		versionPair := fmt.Sprintf("hostd %s / renterd %s", hostdVersion, renterdVersion)
		if uploadData[versionPair] == nil {
			versionPairs = append(versionPairs, versionPair)
		}

		uploadSpeed, err := strconv.ParseFloat(strings.Fields(record[6])[0], 64)
		if err != nil {
			return fmt.Errorf("failed to parse upload speed: %w", err)
		}
		uploadData[versionPair] = append(uploadData[versionPair], uploadSpeed)

		downloadSpeed, err := strconv.ParseFloat(strings.Fields(record[7])[0], 64)
		if err != nil {
			return fmt.Errorf("failed to parse download speed: %w", err)
		}
		downloadData[versionPair] = append(downloadData[versionPair], downloadSpeed)
	}

	var uploadSeries, downloadSeries []opts.BoxPlotData
	for _, version := range versionPairs {
		uploadSeries = append(uploadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(uploadData[version]),
		})
		downloadSeries = append(downloadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(downloadData[version]),
		})
	}

	bp := charts.NewBoxPlot()
	bp.SetGlobalOptions(charts.WithInitializationOpts(opts.Initialization{
		Theme: "dark",
	}), charts.WithTitleOpts(opts.Title{
		Title: title,
	}), charts.WithAnimation(false))
	bp.SetXAxis(versionPairs).
		AddSeries("Upload", uploadSeries).
		AddSeries("Download", downloadSeries)
	render.MakeChartSnapshot(bp.RenderContent(), outputPath)
	return nil
}
