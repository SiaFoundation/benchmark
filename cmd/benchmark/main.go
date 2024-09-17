package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/klauspost/cpuid/v2"
	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/gateway"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	rapi "go.sia.tech/renterd/api"
	"go.sia.tech/renterd/autopilot"
	"go.sia.tech/renterd/bus"
	"go.sia.tech/renterd/worker"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"lukechampine.com/frand"
)

type BenchmarkResult struct {
	CPU           string        `json:"cpu"`
	HostdCommit   string        `json:"hostdCommit"`
	RenterdCommit string        `json:"renterdCommit"`
	ObjectSize    uint64        `json:"objectSize"`
	UploadTime    time.Duration `json:"uploadTime"`
	DownloadTime  time.Duration `json:"downloadTime"`
}

func main() {
	var (
		outputPath string
		dir        string
		logLevel   string

		renterdCount int
		hostdCount   int
	)

	flag.StringVar(&outputPath, "output", "", "output directory for benchmark results")
	flag.StringVar(&dir, "dir", "", "directory to store node data")
	flag.StringVar(&logLevel, "log", "info", "logging level")
	flag.IntVar(&hostdCount, "hosts", 50, "number of hosts to connect to")
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

	if hostdCount == 0 && renterdCount == 0 {
		log.Panic("no nodes to run")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	dir, err := os.MkdirTemp(dir, "sia-cluster-*")
	if err != nil {
		log.Panic("failed to create temp dir", zap.Error(err))
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Error("failed to remove temp dir", zap.Error(err))
		} else {
			log.Debug("removed temp dir", zap.String("dir", dir))
		}
	}()

	// use modified Zen testnet
	n, genesis := chain.TestnetZen()
	n.InitialTarget = types.BlockID{0xFF}
	n.HardforkDevAddr.Height = 1
	n.HardforkTax.Height = 1
	n.HardforkStorageProof.Height = 1
	n.HardforkOak.Height = 1
	n.HardforkASIC.Height = 1
	n.HardforkFoundation.Height = 1
	n.HardforkV2.AllowHeight = 1000000 // should be unattainable
	n.HardforkV2.RequireHeight = 1200000

	bdb, err := coreutils.OpenBoltChainDB(filepath.Join(dir, "consensus.db"))
	if err != nil {
		log.Panic("failed to open bolt db", zap.Error(err))
	}
	defer bdb.Close()

	dbstore, tipState, err := chain.NewDBStore(bdb, n, genesis)
	if err != nil {
		log.Panic("failed to create dbstore", zap.Error(err))
	}
	cm := chain.NewManager(dbstore, tipState)

	syncerListener, err := net.Listen("tcp", ":0")
	if err != nil {
		log.Panic("failed to listen on api address", zap.Error(err))
	}
	defer syncerListener.Close()

	_, port, err := net.SplitHostPort(syncerListener.Addr().String())
	s := syncer.New(syncerListener, cm, testutil.NewMemPeerStore(), gateway.Header{
		GenesisID:  genesis.ID(),
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithMaxOutboundPeers(10000), syncer.WithMaxInboundPeers(10000), syncer.WithBanDuration(time.Second), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second)) // essentially no limit on inbound peers
	if err != nil {
		log.Panic("failed to create syncer", zap.Error(err))
	}
	defer s.Close()
	go s.Run(ctx)

	nm := nodes.NewManager(dir, cm, s, log.Named("cluster"))

	var wg sync.WaitGroup
	// start the hosts
	for i := 0; i < hostdCount; i++ {
		wg.Add(1)
		ready := make(chan struct{}, 1)
		go func() {
			defer wg.Done()
			if err := nm.StartHostd(ctx, ready); err != nil {
				log.Panic("hostd failed to start", zap.Error(err))
			}
		}()
		select {
		case <-ctx.Done():
			log.Panic("context canceled")
		case <-ready:
		}
	}

	// start the renter
	wg.Add(1)
	ready := make(chan struct{}, 1)
	go func() {
		defer wg.Done()
		if err := nm.StartRenterd(ctx, ready); err != nil {
			log.Panic("renterd failed to start", zap.Error(err))
		}
	}()

	select {
	case <-ctx.Done():
		log.Panic("context canceled")
	case <-ready:
	}

	// setup the renter
	renter := nm.Renterd()[0]
	autopilot := autopilot.NewClient(renter.APIAddress+"/api/autopilot", renter.Password)
	autopilotCfg, err := autopilot.Config()
	if err != nil {
		log.Panic("failed to get autopilot config", zap.Error(err))
	}
	// set the contract count to match the number of hosts
	autopilotCfg.Contracts.Amount = uint64(hostdCount)
	if err := autopilot.UpdateConfig(autopilotCfg); err != nil {
		log.Panic("failed to update autopilot config", zap.Error(err))
	}

	// mine until all payouts have matured
	if err := nm.MineBlocks(ctx, 144, types.VoidAddress); err != nil {
		log.Panic("failed to mine blocks", zap.Error(err))
	}

	if _, err := autopilot.Trigger(true); err != nil {
		log.Panic("failed to trigger autopilot", zap.Error(err))
	}

	// wait for contracts with all hosts to form
	bus := bus.NewClient(renter.APIAddress+"/api/bus", renter.Password)
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}

		contracts, err := bus.Contracts(ctx, rapi.ContractsOpts{ContractSet: "autopilot"})
		if err != nil {
			log.Panic("failed to get contracts", zap.Error(err))
		} else if len(contracts) >= hostdCount {
			break
		}
		log.Info("waiting for contracts", zap.Int("count", len(contracts)))
		// bit of a hack to ensure the consensus state ends up in a good state
		// during contract formation.
		if err := nm.MineBlocks(ctx, 1, types.VoidAddress); err != nil {
			log.Panic("failed to mine blocks", zap.Error(err))
		}
	}

	if err := runE2EBenchmark(ctx, hostdVersion, renterdVersion, filepath.Join(outputPath, "e2e.csv"), renter, log); err != nil {
		log.Panic("failed to run e2e benchmark", zap.Error(err))
	}
}

func runE2EBenchmark(ctx context.Context, hostVersion, renterVersion, outputPath string, renter nodes.Node, log *zap.Logger) error {
	oneGiB := frand.Bytes(1 << 30)
	worker := worker.NewClient(renter.APIAddress+"/api/worker", renter.Password)

	log.Info("starting upload")
	uploadStart := time.Now()
	_, err := worker.UploadObject(ctx, bytes.NewReader(oneGiB), "default", "1gib.test", rapi.UploadObjectOptions{
		MinShards:   10,
		TotalShards: 30,
	})
	uploadDuration := time.Since(uploadStart)
	if err != nil {
		log.Panic("failed to upload object", zap.Error(err))
	}
	log.Info("uploaded 1 GiB object", zap.Duration("duration", uploadDuration))

	log.Info("starting download")
	downloadStart := time.Now()
	err = worker.DownloadObject(ctx, io.Discard, "default", "1gib.test", rapi.DownloadObjectOptions{})
	downloadDuration := time.Since(downloadStart)
	if err != nil {
		log.Panic("failed to download object", zap.Error(err))
	}
	log.Info("downloaded 1 GiB object", zap.Duration("duration", downloadDuration))

	result := BenchmarkResult{
		CPU:           cpuid.CPU.BrandName,
		HostdCommit:   hostVersion,
		RenterdCommit: renterVersion,
		ObjectSize:    uint64(len(oneGiB)),
		UploadTime:    uploadDuration,
		DownloadTime:  downloadDuration,
	}

	if err := writeResult(outputPath, result); err != nil {
		log.Panic("failed to write results", zap.Error(err))
	}
	return nil
}

func writeResult(outputPath string, results BenchmarkResult) error {
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
		row := []string{"Timestamp", "CPU", "hostd version", "renterd version", "upload speed", "download speed"}
		if err := enc.Write(row); err != nil {
			return fmt.Errorf("failed to write header: %w", err)
		}
	}

	row := []string{
		time.Now().Format(time.RFC3339),
		results.CPU,
		results.HostdCommit,
		results.RenterdCommit,
		fmt.Sprintf("%.4f Mbps", float64(results.ObjectSize*8/(1<<20))/results.UploadTime.Seconds()),
		fmt.Sprintf("%.4f Mbps", float64(results.ObjectSize*8/(1<<20))/results.DownloadTime.Seconds()),
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
