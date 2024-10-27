package benchmarks

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/consensus"
	"go.sia.tech/core/gateway"
	rhp2 "go.sia.tech/core/rhp/v2"
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
	"lukechampine.com/frand"
)

// An E2EResult contains the results of a production-like end-to-end
// benchmark that uploaded and downloaded a large object to multiple hosts.
type E2EResult struct {
	ObjectSize   uint64        `json:"objectSize"`
	UploadTime   time.Duration `json:"uploadTime"`
	DownloadTime time.Duration `json:"downloadTime"`
	TTFB         time.Duration `json:"ttfb"`
}

// setupE2EBenchmark creates a testnet with the given number of hosts and a
// single renter. It waits for the renter to form contracts with all hosts
// before returning.
func setupE2EBenchmark(ctx context.Context, network *consensus.Network, nm *nodes.Manager, hostCount int, log *zap.Logger) (*worker.Client, error) {
	renterKey := types.GeneratePrivateKey()

	// mine additional blocks to ensure the renter has enough funds to form
	// contracts
	renterAddr := types.StandardUnlockHash(renterKey.PublicKey())
	if err := nm.MineBlocks(ctx, 50+int(network.MaturityDelay), renterAddr); err != nil {
		return nil, fmt.Errorf("failed to mine blocks: %w", err)
	}

	// start the hosts
	for i := 0; i < hostCount; i++ {
		ready := make(chan struct{}, 1)
		go func() {
			// started in a goroutine to avoid blocking
			if err := nm.StartHostd(ctx, types.GeneratePrivateKey(), ready); err != nil {
				log.Panic("hostd failed to start", zap.Error(err))
			}
		}()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ready:
			log.Info("started hostd node", zap.Int("nodes", i+1))
		}
	}

	// start the renter
	ready := make(chan struct{}, 1)
	go func() {
		// started in a goroutine to avoid blocking
		if err := nm.StartRenterd(ctx, renterKey, ready); err != nil {
			log.Panic("renterd failed to start", zap.Error(err))
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ready:
	}

	// setup the renter
	renter := nm.Renterd()[0]
	autopilot := autopilot.NewClient(renter.APIAddress+"/api/autopilot", renter.Password)
	autopilotCfg, err := autopilot.Config()
	if err != nil {
		return nil, fmt.Errorf("failed to get autopilot config: %w", err)
	}
	// set the contract count to match the number of hosts
	autopilotCfg.Contracts.Amount = uint64(hostCount)
	if err := autopilot.UpdateConfig(autopilotCfg); err != nil {
		return nil, fmt.Errorf("failed to update autopilot config: %w", err)
	}

	bus := bus.NewClient(renter.APIAddress+"/api/bus", renter.Password)

	err = bus.UpdateUploadSettings(ctx, rapi.UploadSettings{
		DefaultContractSet: "autopilot",
		Packing: rapi.UploadPackingSettings{
			Enabled: false,
		},
		Redundancy: rapi.RedundancySettings{
			MinShards:   10,
			TotalShards: 30,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update upload settings: %w", err)
	}

	if err := bus.CreateBucket(ctx, "test", rapi.CreateBucketOptions{}); err != nil {
		return nil, fmt.Errorf("failed to create bucket: %w", err)
	}

	// mine until all payouts have matured
	if err := nm.MineBlocks(ctx, 10, renterAddr); err != nil {
		return nil, fmt.Errorf("failed to mine blocks: %w", err)
	}

	// wait for nodes to sync
	time.Sleep(30 * time.Second) // TODO: be better

	if _, err := autopilot.Trigger(true); err != nil {
		return nil, fmt.Errorf("failed to trigger autopilot: %w", err)
	}

	// wait for contracts with all hosts to form
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(15 * time.Second):
		}

		contracts, err := bus.Contracts(ctx, rapi.ContractsOpts{ContractSet: "autopilot"})
		if err != nil {
			return nil, fmt.Errorf("failed to get contracts: %w", err)
		} else if len(contracts) >= hostCount {
			break
		}

		hosts, err := bus.Hosts(ctx, rapi.HostOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to get hosts: %w", err)
		}

		log.Info("waiting for contracts", zap.Int("contracts", len(contracts)), zap.Int("hosts", len(hosts)))
		// bit of a hack to ensure the nodes end up in a good state during
		// contract formation.
		if err := nm.MineBlocks(ctx, 1, renterAddr); err != nil {
			return nil, fmt.Errorf("failed to mine blocks: %w", err)
		}
	}
	return worker.NewClient(renter.APIAddress+"/api/worker", renter.Password), nil
}

// E2E runs an end-to-end benchmark that uploads and downloads a large object
// to/from a cluster of hosts. The benchmark results are written to outputPath.
func E2E(ctx context.Context, dir string, log *zap.Logger) (E2EResult, error) {
	const (
		benchmarkSectors = 80
		benchmarkSize    = benchmarkSectors * rhp2.SectorSize
	)

	// wrap the context with a cancel func to ensure the benchmark resources
	// are cleaned up after completion
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// create a temp dir to store the benchmark data
	dir, err := os.MkdirTemp(dir, "sia-benchmark-*")
	if err != nil {
		return E2EResult{}, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Error("failed to remove temp dir", zap.Error(err))
		} else {
			log.Debug("removed temp dir", zap.String("dir", dir))
		}
	}()

	// create a chain manager for the node manager
	bdb, err := coreutils.OpenBoltChainDB(filepath.Join(dir, "consensus.db"))
	if err != nil {
		log.Panic("failed to open bolt db", zap.Error(err))
	}
	defer bdb.Close()

	n, genesis := benchmarkV1Network()
	dbstore, tipState, err := chain.NewDBStore(bdb, n, genesis)
	if err != nil {
		log.Panic("failed to create dbstore", zap.Error(err))
	}
	cm := chain.NewManager(dbstore, tipState)

	// create a syncer for the node manager
	syncerListener, err := net.Listen("tcp", ":0")
	if err != nil {
		return E2EResult{}, fmt.Errorf("failed to create syncer listener: %w", err)
	}
	defer syncerListener.Close()

	_, port, err := net.SplitHostPort(syncerListener.Addr().String())
	s := syncer.New(syncerListener, cm, testutil.NewMemPeerStore(), gateway.Header{
		GenesisID:  genesis.ID(),
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithMaxOutboundPeers(10000), syncer.WithMaxInboundPeers(10000), syncer.WithBanDuration(time.Second), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second)) // essentially no limit on inbound peers
	if err != nil {
		return E2EResult{}, fmt.Errorf("failed to create syncer: %w", err)
	}
	defer s.Close()
	go s.Run(ctx)

	// create a node manager
	nm := nodes.NewManager(dir, cm, s, nodes.WithLog(log.Named("cluster")), nodes.WithSharedConsensus(true))
	defer nm.Close()

	// setup the benchmark
	worker, err := setupE2EBenchmark(ctx, n, nm, 50, log)
	if err != nil {
		return E2EResult{}, fmt.Errorf("failed to setup benchmark: %w", err)
	}

	// generate random data to upload
	data := frand.Bytes(benchmarkSize)

	// upload the data
	log.Info("starting upload")
	uploadStart := time.Now()
	_, err = worker.UploadObject(ctx, bytes.NewReader(data), "test", "benchmark-e2e", rapi.UploadObjectOptions{
		MinShards:   10,
		TotalShards: 30,
	})
	uploadDuration := time.Since(uploadStart)
	if err != nil {
		return E2EResult{}, fmt.Errorf("failed to upload object: %w", err)
	}
	log.Info("uploaded  object", zap.Duration("duration", uploadDuration))

	// download the data
	log.Info("starting download")
	downloadStart := time.Now()
	tw := newTTFBWriter()
	err = worker.DownloadObject(ctx, tw, "test", "benchmark-e2e", rapi.DownloadObjectOptions{})
	downloadDuration := time.Since(downloadStart)
	if err != nil {
		return E2EResult{}, fmt.Errorf("failed to download object: %w", err)
	}
	log.Info("downloaded  object", zap.Duration("duration", downloadDuration))

	return E2EResult{
		ObjectSize:   benchmarkSize,
		UploadTime:   uploadDuration,
		DownloadTime: downloadDuration,
		TTFB:         tw.TTFB(),
	}, nil
}
