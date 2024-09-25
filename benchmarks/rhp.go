package benchmarks

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	rhp2 "go.sia.tech/benchmark/internal/rhp/v2"
	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/gateway"
	proto2 "go.sia.tech/core/rhp/v2"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	rapi "go.sia.tech/renterd/api"
	"go.sia.tech/renterd/autopilot"
	"go.sia.tech/renterd/bus"
	"go.uber.org/zap"
	"golang.org/x/crypto/blake2b"
	"lukechampine.com/frand"
)

type RHPResult struct {
	Sectors         uint64        `json:"sectors"`
	UploadTime      time.Duration `json:"uploadTime"`
	DownloadTime    time.Duration `json:"downloadTime"`
	AppendSectorP99 time.Duration `json:"appendSectorP99"`
	ReadSectorP99   time.Duration `json:"readSectorP99"`
	ReadSectorTTFB  time.Duration `json:"readSectorTTFB"`
}

// had to rewrite instead of importing from renterd because Chris is mean
func deriveContractKey(renterKey types.PrivateKey, hostKey types.PublicKey) types.PrivateKey {
	h, _ := blake2b.New256(nil)
	defer h.Reset()
	h.Write(renterKey[:32])
	h.Write([]byte("renterkey"))
	seed := types.NewPrivateKeyFromSeed(h.Sum(nil))
	h.Reset()
	h.Write(seed)
	h.Write(hostKey[:])
	return types.NewPrivateKeyFromSeed(h.Sum(nil))
}

// setupRHPBenchmark creates a testnet with a single host and renter node
func setupRHPBenchmark(ctx context.Context, nm *nodes.Manager, log *zap.Logger) (*bus.Client, types.PrivateKey, error) {
	// start the host and renter
	ready := make(chan struct{}, 1)
	go func() {
		// started in a goroutine to avoid blocking
		if err := nm.StartHostd(ctx, types.GeneratePrivateKey(), ready); err != nil {
			log.Panic("hostd failed to start", zap.Error(err))
		}
	}()
	select {
	case <-ctx.Done():
		return nil, types.PrivateKey{}, ctx.Err()
	case <-ready:
	}

	sk := types.GeneratePrivateKey()
	go func() {
		// started in a goroutine to avoid blocking
		if err := nm.StartRenterd(ctx, sk, ready); err != nil {
			log.Panic("renterd failed to start", zap.Error(err))
		}
	}()

	select {
	case <-ctx.Done():
		return nil, types.PrivateKey{}, ctx.Err()
	case <-ready:
	}

	// mine until all payouts have matured
	if err := nm.MineBlocks(ctx, 144, types.VoidAddress); err != nil {
		return nil, types.PrivateKey{}, fmt.Errorf("failed to mine blocks: %w", err)
	}

	// setup the renter
	renter := nm.Renterd()[0]
	autopilot := autopilot.NewClient(renter.APIAddress+"/api/autopilot", renter.Password)
	// trigger autopilot
	if _, err := autopilot.Trigger(true); err != nil {
		return nil, types.PrivateKey{}, fmt.Errorf("failed to trigger autopilot: %w", err)
	}

	// wait for a contract with the host to form
	bus := bus.NewClient(renter.APIAddress+"/api/bus", renter.Password)
	for i := 0; i < 100; i++ {
		select {
		case <-ctx.Done():
			return nil, types.PrivateKey{}, ctx.Err()
		case <-time.After(5 * time.Second):
		}

		contracts, err := bus.Contracts(ctx, rapi.ContractsOpts{ContractSet: "autopilot"})
		if err != nil {
			return nil, types.PrivateKey{}, fmt.Errorf("failed to get contracts: %w", err)
		} else if len(contracts) > 0 {
			break
		}
		log.Info("waiting for contracts", zap.Int("count", len(contracts)))
		// bit of a hack to ensure the nodes end up in a good state during
		// contract formation.
		if err := nm.MineBlocks(ctx, 1, types.VoidAddress); err != nil {
			return nil, types.PrivateKey{}, fmt.Errorf("failed to mine blocks: %w", err)
		}
	}
	return bus, sk, nil
}

func RHP2(ctx context.Context, dir string, log *zap.Logger) (RHPResult, error) {
	const (
		benchmarkSectors = 256
		benchmarkSize    = benchmarkSectors * proto2.SectorSize
	)

	// wrap the context with a cancel func to ensure the benchmark resources
	// are cleaned up after completion
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// create a temp dir to store the benchmark data
	dir, err := os.MkdirTemp(dir, "sia-benchmark-*")
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create temp dir: %w", err)
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
		return RHPResult{}, fmt.Errorf("failed to create syncer listener: %w", err)
	}
	defer syncerListener.Close()

	_, port, err := net.SplitHostPort(syncerListener.Addr().String())
	s := syncer.New(syncerListener, cm, testutil.NewMemPeerStore(), gateway.Header{
		GenesisID:  genesis.ID(),
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithMaxOutboundPeers(10000), syncer.WithMaxInboundPeers(10000), syncer.WithBanDuration(time.Second), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second)) // essentially no limit on inbound peers
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create syncer: %w", err)
	}
	defer s.Close()
	go s.Run(ctx)

	// create a node manager
	nm := nodes.NewManager(dir, cm, s, log.Named("cluster"))
	defer nm.Close()

	// grab the contract details
	bus, renterKey, err := setupRHPBenchmark(ctx, nm, log)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to setup benchmark: %w", err)
	}

	contracts, err := bus.Contracts(ctx, rapi.ContractsOpts{ContractSet: "autopilot"})
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to get contracts: %w", err)
	} else if len(contracts) == 0 {
		return RHPResult{}, fmt.Errorf("no contracts found")
	}

	time.Sleep(10 * time.Second) // TODO: replace this with an actual check that the host has finished syncing

	contract := contracts[0]
	hostAddress, hostKey := contract.HostIP, contract.HostKey
	contractID := contract.ID

	// create an RHP2 transport
	conn, err := net.Dial("tcp", hostAddress)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to dial host: %w", err)
	}
	defer conn.Close()

	transport, err := proto2.NewRenterTransport(conn, hostKey)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create transport: %w", err)
	}
	defer transport.Close()

	// get the host's settings
	settings, err := rhp2.RPCSettings(transport)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to get host settings: %w", err)
	}

	log.Debug("locked contract", zap.Stringer("contractID", contractID))
	contractKey := deriveContractKey(renterKey, hostKey)
	// get the latest contract revision
	revision, err := rhp2.RPCLock(transport, contractKey, contractID)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to lock contract: %w", err)
	}
	defer rhp2.RPCUnlock(transport)

	// upload the data
	log.Info("starting upload")
	appendTimes := make([]time.Duration, 0, benchmarkSectors)
	roots := make([]types.Hash256, 0, benchmarkSectors)
	for i := range benchmarkSectors {
		// generate random data to upload
		sector := (*[proto2.SectorSize]byte)(frand.Bytes(proto2.SectorSize))
		actions := []proto2.RPCWriteAction{
			{Type: proto2.RPCWriteActionAppend, Data: sector[:]},
		}
		duration := revision.Revision.WindowEnd - cm.Tip().Height
		usage, err := settings.RPCWriteCost(actions, uint64(len(roots)), duration, true)
		if err != nil {
			return RHPResult{}, fmt.Errorf("failed to calculate cost: %w", err)
		}
		cost, collateral := usage.Total()
		start := time.Now()
		if err := rhp2.RPCWrite(transport, contractKey, &revision, actions, cost, collateral); err != nil {
			return RHPResult{}, fmt.Errorf("failed to write sector %d: %w", i+1, err)
		}
		appendTimes = append(appendTimes, time.Since(start))
		roots = append(roots, proto2.SectorRoot(sector))
		log.Debug("appended sector", zap.Duration("elapsed", appendTimes[len(appendTimes)-1]), zap.Int("n", i+1))
	}

	// download the data
	log.Info("starting download")
	readTimes := make([]time.Duration, 0, benchmarkSectors)
	ttfbTimes := make([]time.Duration, 0, benchmarkSectors)
	for _, root := range roots {
		sections := []proto2.RPCReadRequestSection{
			{MerkleRoot: root, Offset: 0, Length: proto2.SectorSize},
		}
		usage, err := settings.RPCReadCost(sections, true)
		if err != nil {
			return RHPResult{}, fmt.Errorf("failed to calculate cost: %w", err)
		}

		cost, _ := usage.Total()
		tw := newTTFBWriter()
		start := time.Now()
		if err := rhp2.RPCRead(transport, tw, contractKey, &revision, sections, cost); err != nil {
			return RHPResult{}, fmt.Errorf("failed to read sector: %w", err)
		}
		readTimes = append(readTimes, time.Since(start))
		ttfbTimes = append(ttfbTimes, tw.TTFB())
		log.Debug("read sector", zap.Duration("elapsed", readTimes[len(readTimes)-1]), zap.Int("n", len(readTimes)))
	}

	result := RHPResult{
		Sectors: benchmarkSectors,
	}

	for _, d := range appendTimes {
		result.UploadTime += d
	}
	for _, d := range readTimes {
		result.DownloadTime += d
	}

	sort.Slice(appendTimes, func(i, j int) bool { return appendTimes[i] < appendTimes[j] })
	i := int(float64(len(appendTimes)) * 0.99)
	result.AppendSectorP99 = appendTimes[i]

	sort.Slice(readTimes, func(i, j int) bool { return readTimes[i] < readTimes[j] })
	i = int(float64(len(readTimes)) * 0.99)
	result.ReadSectorP99 = readTimes[i]

	sort.Slice(ttfbTimes, func(i, j int) bool { return ttfbTimes[i] < ttfbTimes[j] })
	i = int(float64(len(ttfbTimes)) * 0.99)
	result.ReadSectorTTFB = ttfbTimes[i]

	return result, nil
}
