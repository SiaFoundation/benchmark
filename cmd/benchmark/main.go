package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/klauspost/cpuid"
	"go.sia.tech/benchmark/benchmarks"
	rhp2 "go.sia.tech/core/rhp/v2"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func writeRHPResult(hostdVersion, outputPath string, result benchmarks.RHPResult) error {
	var writeHeader bool
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		writeHeader = true
	} else if err != nil {
		return fmt.Errorf("failed to stat output file: %w", err)
	}

	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open output tmp file: %w", err)
	}
	defer f.Close()

	enc := csv.NewWriter(f)

	if writeHeader {
		row := []string{"Timestamp", "OS", "Arch", "CPU", "hostd Version", "Upload Speed", "Download Speed", "Append Sector P99", "Read Sector P99", "Read Sector TTFB"}
		if err := enc.Write(row); err != nil {
			return fmt.Errorf("failed to write header: %w", err)
		}
	}

	mbits := float64(result.Sectors*rhp2.SectorSize) / (1 << 20) * 8
	row := []string{
		time.Now().Format(time.RFC3339),
		runtime.GOOS,
		runtime.GOARCH,
		cpuid.CPU.BrandName,
		hostdVersion,
		fmt.Sprintf("%.4f Mbps", mbits/result.UploadTime.Seconds()),
		fmt.Sprintf("%.4f Mbps", mbits/result.DownloadTime.Seconds()),
		fmt.Sprintf("%d ms", result.AppendSectorP99.Milliseconds()),
		fmt.Sprintf("%d ms", result.ReadSectorP99.Milliseconds()),
		fmt.Sprintf("%d ms", result.ReadSectorTTFB.Milliseconds()),
	}
	if err := enc.Write(row); err != nil {
		return fmt.Errorf("failed to write row: %w", err)
	}
	enc.Flush()
	// fsync
	if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to sync output file: %w", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close output file: %w", err)
	}
	return nil
}

func writeE2EResult(hostdVersion, renterdVersion, outputPath string, result benchmarks.E2EResult) error {
	var writeHeader bool
	if _, err := os.Stat(outputPath); os.IsNotExist(err) {
		writeHeader = true
	} else if err != nil {
		return fmt.Errorf("failed to stat output file: %w", err)
	}

	f, err := os.OpenFile(outputPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open output tmp file: %w", err)
	}
	defer f.Close()

	enc := csv.NewWriter(f)

	if writeHeader {
		row := []string{"Timestamp", "OS", "Arch", "CPU", "hostd Version", "renterd Version", "Upload Speed", "Download Speed", "TTFB"}
		if err := enc.Write(row); err != nil {
			return fmt.Errorf("failed to write header: %w", err)
		}
	}

	mbits := float64(result.ObjectSize) / (1 << 20) * 8
	row := []string{
		time.Now().Format(time.RFC3339),
		runtime.GOOS,
		runtime.GOARCH,
		cpuid.CPU.BrandName,
		hostdVersion,
		renterdVersion,
		fmt.Sprintf("%.4f Mbps", mbits/result.UploadTime.Seconds()),
		fmt.Sprintf("%.4f Mbps", mbits/result.DownloadTime.Seconds()),
		fmt.Sprintf("%d ms", result.TTFB.Milliseconds()),
	}
	if err := enc.Write(row); err != nil {
		return fmt.Errorf("failed to write row: %w", err)
	}
	enc.Flush()
	// fsync
	if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to sync output file: %w", err)
	} else if err := f.Close(); err != nil {
		return fmt.Errorf("failed to close output file: %w", err)
	}
	return nil
}

func main() {
	var (
		outputPath string
		dir        string
		logLevel   string

		runRHP2 bool
		runRHP3 bool
		runE2E  bool
	)

	flag.StringVar(&outputPath, "output", "results", "output directory for benchmark results")
	flag.StringVar(&dir, "dir", "", "directory to store node data")
	flag.StringVar(&logLevel, "log", "info", "logging level")
	flag.BoolVar(&runRHP2, "rhp2", true, "run rhp2 benchmark")
	flag.BoolVar(&runRHP3, "rhp3", true, "run rhp3 benchmark")
	flag.BoolVar(&runE2E, "e2e", true, "run e2e benchmark")
	flag.Parse()

	if err := os.MkdirAll(outputPath, 0755); err != nil {
		panic(err)
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("Could not read build info")
		return
	}

	var hostdVersion, renterdVersion string
	for _, dep := range info.Deps {
		switch {
		case dep == nil:
			continue
		case dep.Path == "go.sia.tech/hostd":
			hostdVersion = dep.Version
		case dep.Path == "go.sia.tech/renterd":
			renterdVersion = dep.Version
		}
	}

	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "" // prevent duplicate timestamps
	cfg.EncodeTime = zapcore.RFC3339TimeEncoder
	cfg.EncodeDuration = zapcore.StringDurationEncoder
	cfg.EncodeLevel = zapcore.CapitalColorLevelEncoder

	cfg.StacktraceKey = ""
	cfg.CallerKey = ""
	encoder := zapcore.NewConsoleEncoder(cfg)

	var level zap.AtomicLevel
	switch logLevel {
	case "debug":
		level = zap.NewAtomicLevelAt(zap.DebugLevel)
	case "info":
		level = zap.NewAtomicLevelAt(zap.InfoLevel)
	case "warn":
		level = zap.NewAtomicLevelAt(zap.WarnLevel)
	case "error":
		level = zap.NewAtomicLevelAt(zap.ErrorLevel)
	default:
		fmt.Printf("invalid log level %q", level)
		os.Exit(1)
	}

	log := zap.New(zapcore.NewCore(encoder, zapcore.Lock(os.Stdout), level))
	defer log.Sync()

	zap.RedirectStdLog(log)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if runRHP2 {
		if rhp2, err := benchmarks.RHP2(ctx, dir, log.Named("rhp2")); err != nil {
			log.Panic("failed to run rhp2 benchmark", zap.Error(err))
		} else if err := writeRHPResult(hostdVersion, filepath.Join(outputPath, "rhp2.csv"), rhp2); err != nil {
			log.Panic("failed to write rhp2 result", zap.Error(err))
		}
	}

	if runRHP3 {
		if rhp3, err := benchmarks.RHP3(ctx, dir, log.Named("rhp3")); err != nil {
			log.Panic("failed to run rhp2 benchmark", zap.Error(err))
		} else if err := writeRHPResult(hostdVersion, filepath.Join(outputPath, "rhp3.csv"), rhp3); err != nil {
			log.Panic("failed to write rhp2 result", zap.Error(err))
		}
	}

	if runE2E {
		if e2e, err := benchmarks.E2E(ctx, dir, log.Named("e2e")); err != nil {
			log.Panic("failed to run e2e benchmark", zap.Error(err))
		} else if err := writeE2EResult(hostdVersion, renterdVersion, filepath.Join(outputPath, "e2e.csv"), e2e); err != nil {
			log.Panic("failed to write e2e result", zap.Error(err))
		}
	}
}
