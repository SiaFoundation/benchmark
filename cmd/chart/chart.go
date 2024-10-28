package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/go-echarts/go-echarts/v2/charts"
	"github.com/go-echarts/go-echarts/v2/opts"
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
		return parts[2][:6] // commit hash
	}
	return v
}

func screenshot(ctx context.Context, chart *charts.BoxPlot, outputPath string) error {
	ctx, cancel := chromedp.NewContext(context.Background())
	defer cancel()

	f, err := os.CreateTemp("", "screenshot-*.html")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	defer f.Close()
	defer os.Remove(f.Name())

	if err := chart.Render(f); err != nil {
		return fmt.Errorf("failed to render chart: %w", err)
	} else if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}

	tp, err := filepath.Abs(f.Name())
	if err != nil {
		return fmt.Errorf("failed to get absolute path: %w", err)
	}

	var ss []byte
	err = chromedp.Run(ctx, chromedp.Tasks{
		chromedp.EmulateViewport(1920, 1080),
		chromedp.Navigate(`file://` + tp),
		chromedp.ScreenshotScale("div.item", 2, &ss, chromedp.NodeVisible),
	})
	if err != nil {
		return fmt.Errorf("failed to take screenshot: %w", err)
	}
	return os.WriteFile(outputPath, ss, 0644)
}

func createBoxPlot(title, subtitle, outputPath string, versions []string, uploadSeries, downloadSeries []opts.BoxPlotData, ymin, ymax float64) error {
	bp := charts.NewBoxPlot()
	bp.SetGlobalOptions(charts.WithInitializationOpts(opts.Initialization{
		Theme:           "dark",
		BackgroundColor: "#0d1116",
		Renderer:        "svg",
	}), charts.WithTitleOpts(opts.Title{
		Title:    title,
		Subtitle: subtitle,
	}), charts.WithAnimation(false), charts.WithXAxisOpts(opts.XAxis{
		AxisLabel: &opts.AxisLabel{
			FontSize: 8,
			Align:    "center",
		},
	}), charts.WithYAxisOpts(opts.YAxis{
		Min: math.Round(ymin * 0.9),
		Max: math.Round(ymax * 1.1),
	}))
	bp.SetXAxis(versions).
		AddSeries("Upload", uploadSeries).
		AddSeries("Download", downloadSeries)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return screenshot(ctx, bp, outputPath)
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

	var os, arch, cpu string
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

		if os != "" && os != record[1] {
			continue
		} else if arch != "" && arch != record[2] {
			continue
		} else if cpu != "" && cpu != record[3] {
			continue
		}
		os = record[1]
		arch = record[2]
		cpu = record[3]

		version := fmt.Sprintf("hostd %s", normalizeGoVersion(record[4]))
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

	if len(versions) > 10 {
		versions = versions[len(versions)-10:]
	}

	var cmin, cmax float64
	var uploadSeries, downloadSeries []opts.BoxPlotData
	for _, version := range versions {
		uploadData, downloadData := uploadData[version], downloadData[version]
		vmin := min(slices.Min(uploadData), slices.Min(downloadData))
		vmax := max(slices.Max(uploadData), slices.Max(downloadData))
		if cmin == 0 || vmin < cmin {
			cmin = vmin
		}
		if vmax > cmax {
			cmax = vmax
		}
		uploadSeries = append(uploadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(uploadData),
		})
		downloadSeries = append(downloadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(downloadData),
		})
	}

	return createBoxPlot(title, fmt.Sprintf("%s (%s/%s)", cpu, os, arch), outputPath, versions, uploadSeries, downloadSeries, cmin, cmax)
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

	var os, arch, cpu string
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

		if os != "" && os != record[1] {
			continue
		} else if arch != "" && arch != record[2] {
			continue
		} else if cpu != "" && cpu != record[3] {
			continue
		}

		os = record[1]
		arch = record[2]
		cpu = record[3]

		hostdVersion, renterdVersion := normalizeGoVersion(record[4]), normalizeGoVersion(record[5])
		versionPair := fmt.Sprintf("hostd %s\nrenterd %s", hostdVersion, renterdVersion)
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

	if len(versionPairs) > 10 {
		versionPairs = versionPairs[len(versionPairs)-10:]
	}

	var cmin, cmax float64
	var uploadSeries, downloadSeries []opts.BoxPlotData
	for _, version := range versionPairs {
		uploadData, downloadData := uploadData[version], downloadData[version]
		vmin := min(slices.Min(uploadData), slices.Min(downloadData))
		vmax := max(slices.Max(uploadData), slices.Max(downloadData))
		if cmin == 0 || vmin < cmin {
			cmin = vmin
		}
		if vmax > cmax {
			cmax = vmax
		}

		uploadSeries = append(uploadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(uploadData),
		})
		downloadSeries = append(downloadSeries, opts.BoxPlotData{
			Name:  version,
			Value: createBoxPlotData(downloadData),
		})
	}
	return createBoxPlot(title, fmt.Sprintf("%s (%s/%s)", cpu, os, arch), outputPath, versionPairs, uploadSeries, downloadSeries, cmin, cmax)
}
