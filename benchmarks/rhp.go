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
	rhp3 "go.sia.tech/benchmark/internal/rhp/v3"
	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/gateway"
	proto2 "go.sia.tech/core/rhp/v2"
	proto3 "go.sia.tech/core/rhp/v3"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	"go.sia.tech/coreutils/wallet"
	"go.uber.org/zap"
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

// helper to scan the chain and update the wallet. Should only be used with a
// local testnet.
func scanWallet(ctx context.Context, cm *chain.Manager, sw *wallet.SingleAddressWallet, ss *testutil.EphemeralWalletStore) error {
	var index types.ChainIndex
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		var done bool
		err := ss.UpdateChainState(func(ux wallet.UpdateTx) error {
			var applied []chain.ApplyUpdate
			reverted, applied, err := cm.UpdatesSince(index, 1000)
			if err != nil {
				return fmt.Errorf("failed to get updates: %w", err)
			} else if len(applied) == 0 && len(reverted) == 0 {
				done = true
				return nil
			}

			if len(applied) > 0 {
				index = applied[len(applied)-1].State.Index
			} else if len(reverted) > 0 {
				index = reverted[len(reverted)-1].State.Index
			}

			return sw.UpdateChainState(ux, reverted, applied)
		})
		if err != nil {
			return err
		} else if done {
			return nil
		}
	}
}

func setupRenterWallet(ctx context.Context, cm *chain.Manager, nm *nodes.Manager, renterKey types.PrivateKey) (*wallet.SingleAddressWallet, error) {
	// mine some utxos for the renter
	renterAddr := types.StandardUnlockHash(renterKey.PublicKey())
	if err := nm.MineBlocks(ctx, 20, renterAddr); err != nil {
		return nil, fmt.Errorf("failed to mine blocks: %w", err)
	}

	// create a wallet for the renter
	ws := testutil.NewEphemeralWalletStore()
	sw, err := wallet.NewSingleAddressWallet(renterKey, cm, ws)
	if err != nil {
		return nil, fmt.Errorf("failed to create renter wallet: %w", err)
	}
	defer sw.Close()

	if err := scanWallet(ctx, cm, sw, ws); err != nil {
		return nil, fmt.Errorf("failed to scan wallet: %w", err)
	}
	return sw, nil
}

// helper to get the last host announcement. Scans the whole chain. Should only
// be used with a local testnet.
func findHostAnnouncement(cm *chain.Manager, sk types.PrivateKey) (address string, err error) {
	var index types.ChainIndex
	hostKey := sk.PublicKey()
	for {
		var applied []chain.ApplyUpdate
		_, applied, err = cm.UpdatesSince(index, 1000)
		if err != nil {
			return "", fmt.Errorf("failed to get updates: %w", err)
		} else if len(applied) == 0 {
			if address == "" {
				err = fmt.Errorf("host announcement not found")
			}
			return
		}
		for _, cau := range applied {
			chain.ForEachHostAnnouncement(cau.Block, func(a chain.HostAnnouncement) {
				if a.PublicKey == hostKey {
					address = a.NetAddress
				}
			})
			index = cau.State.Index
		}
	}
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
	s := syncer.New(syncerListener, cm, testutil.NewEphemeralPeerStore(), gateway.Header{
		GenesisID:  genesis.ID(),
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithMaxOutboundPeers(10000), syncer.WithMaxInboundPeers(10000), syncer.WithBanDuration(time.Second), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second)) // essentially no limit on inbound peers
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create syncer: %w", err)
	}
	defer s.Close()
	go s.Run()

	// create a node manager
	nm := nodes.NewManager(dir, cm, s, nodes.WithLog(log.Named("cluster")), nodes.WithSharedConsensus(true))
	defer nm.Close()

	hostKey, renterKey := types.GeneratePrivateKey(), types.GeneratePrivateKey()

	// start the host
	ready := make(chan struct{}, 1)
	go func() {
		// started in a goroutine to avoid blocking
		if err := nm.StartHostd(ctx, hostKey, ready); err != nil {
			log.Panic("hostd failed to start", zap.Error(err))
		}
	}()
	select {
	case <-ctx.Done():
		return RHPResult{}, ctx.Err()
	case <-ready:
	}

	rw, err := setupRenterWallet(ctx, cm, nm, renterKey)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to setup benchmark: %w", err)
	}

	// wait for syncing
	time.Sleep(15 * time.Second) // TODO: be better

	hostAddress, err := findHostAnnouncement(cm, hostKey)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to find host announcement: %w", err)
	}

	// create an RHP2 transport
	conn, err := net.Dial("tcp", hostAddress)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to dial host: %w", err)
	}
	defer conn.Close()

	transport, err := proto2.NewRenterTransport(conn, hostKey.PublicKey())
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create transport: %w", err)
	}
	defer transport.Close()

	// get the host's settings
	settings, err := rhp2.RPCSettings(transport)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to get host settings: %w", err)
	}

	fc := proto2.PrepareContractFormation(renterKey.PublicKey(), hostKey.PublicKey(), types.Siacoins(50), types.Siacoins(100), cm.Tip().Height+200, settings, rw.Address())
	formationTxn := types.Transaction{
		FileContracts: []types.FileContract{fc},
	}
	toSign, err := rw.FundTransaction(&formationTxn, proto2.ContractFormationCost(cm.TipState(), fc, settings.ContractPrice), false)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to fund contract formation: %w", err)
	}
	rw.SignTransaction(&formationTxn, toSign, wallet.ExplicitCoveredFields(formationTxn))

	revision, _, err := rhp2.RPCFormContract(transport, renterKey, []types.Transaction{formationTxn})
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to form contract: %w", err)
	}

	revision, err = rhp2.RPCLock(transport, renterKey, revision.ID())
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to lock contract: %w", err)
	}

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
		if err := rhp2.RPCWrite(transport, renterKey, &revision, actions, cost, collateral); err != nil {
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
		if err := rhp2.RPCRead(transport, tw, renterKey, &revision, sections, cost); err != nil {
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

func RHP3(ctx context.Context, dir string, log *zap.Logger) (RHPResult, error) {
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
	s := syncer.New(syncerListener, cm, testutil.NewEphemeralPeerStore(), gateway.Header{
		GenesisID:  genesis.ID(),
		UniqueID:   gateway.GenerateUniqueID(),
		NetAddress: "127.0.0.1:" + port,
	}, syncer.WithLogger(log.Named("syncer")), syncer.WithMaxOutboundPeers(10000), syncer.WithMaxInboundPeers(10000), syncer.WithBanDuration(time.Second), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second)) // essentially no limit on inbound peers
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create syncer: %w", err)
	}
	defer s.Close()
	go s.Run()

	// create a node manager
	nm := nodes.NewManager(dir, cm, s, nodes.WithLog(log.Named("cluster")), nodes.WithSharedConsensus(true))
	defer nm.Close()

	hostKey, renterKey := types.GeneratePrivateKey(), types.GeneratePrivateKey()

	// start the host
	ready := make(chan struct{}, 1)
	go func() {
		// started in a goroutine to avoid blocking
		if err := nm.StartHostd(ctx, hostKey, ready); err != nil {
			log.Panic("hostd failed to start", zap.Error(err))
		}
	}()
	select {
	case <-ctx.Done():
		return RHPResult{}, ctx.Err()
	case <-ready:
	}

	rw, err := setupRenterWallet(ctx, cm, nm, renterKey)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to setup benchmark: %w", err)
	}

	// wait for syncing
	time.Sleep(15 * time.Second) // TODO: be better

	hostAddress, err := findHostAnnouncement(cm, hostKey)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to find host announcement: %w", err)
	}

	// create an RHP2 transport
	conn, err := net.Dial("tcp", hostAddress)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to dial host: %w", err)
	}
	defer conn.Close()

	transport, err := proto2.NewRenterTransport(conn, hostKey.PublicKey())
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create transport: %w", err)
	}
	defer transport.Close()

	// get the host's settings
	settings, err := rhp2.RPCSettings(transport)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to get host settings: %w", err)
	}

	fc := proto2.PrepareContractFormation(renterKey.PublicKey(), hostKey.PublicKey(), types.Siacoins(50), types.Siacoins(100), cm.Tip().Height+200, settings, rw.Address())
	formationTxn := types.Transaction{
		FileContracts: []types.FileContract{fc},
	}
	toSign, err := rw.FundTransaction(&formationTxn, proto2.ContractFormationCost(cm.TipState(), fc, settings.ContractPrice), false)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to fund contract formation: %w", err)
	}
	rw.SignTransaction(&formationTxn, toSign, wallet.ExplicitCoveredFields(formationTxn))

	revision, _, err := rhp2.RPCFormContract(transport, renterKey, []types.Transaction{formationTxn})
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to form contract: %w", err)
	} else if err := transport.Close(); err != nil {
		return RHPResult{}, fmt.Errorf("failed to close rhp2 transport: %w", err)
	}

	addr, _, err := net.SplitHostPort(hostAddress)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to split host address: %w", err)
	}
	rhp3Address := net.JoinHostPort(addr, settings.SiaMuxPort)

	// create an RHP3 transport
	rhp3Ctx, rhp3Cancel := context.WithTimeout(ctx, 30*time.Second)
	defer rhp3Cancel()
	session, err := rhp3.NewSession(rhp3Ctx, hostKey.PublicKey(), rhp3Address)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	log.Debug("creating session")

	accountID := proto3.Account(renterKey.PublicKey())
	accountPayment := rhp3.AccountPayment(accountID, renterKey)
	contractPayment := rhp3.ContractPayment(&revision, renterKey, accountID)

	// register the price table
	if _, err := session.RegisterPriceTable(cm.Tip(), contractPayment); err != nil {
		return RHPResult{}, fmt.Errorf("failed to register price table: %w", err)
	} else if _, err := session.FundAccount(cm.Tip(), accountID, contractPayment, types.Siacoins(10)); err != nil {
		return RHPResult{}, fmt.Errorf("failed to fund account: %w", err)
	}

	// upload the data
	log.Info("starting upload")
	appendTimes := make([]time.Duration, 0, benchmarkSectors)
	roots := make([]types.Hash256, 0, benchmarkSectors)
	for i := range benchmarkSectors {
		// generate random data to upload
		sector := (*[proto2.SectorSize]byte)(frand.Bytes(proto2.SectorSize))

		start := time.Now()
		_, err := session.AppendSector(cm.Tip(), sector, &revision, renterKey, accountPayment, types.Siacoins(1).Div64(10)) // just overpay
		appendTimes = append(appendTimes, time.Since(start))
		if err != nil {
			return RHPResult{}, fmt.Errorf("failed to append sector %d: %w", i+1, err)
		}
		roots = append(roots, proto2.SectorRoot(sector))
		log.Debug("appended sector", zap.Duration("elapsed", appendTimes[len(appendTimes)-1]), zap.Int("n", i+1))
	}

	// download the data
	log.Info("starting download")
	readTimes := make([]time.Duration, 0, benchmarkSectors)
	ttfbTimes := make([]time.Duration, 0, benchmarkSectors)
	for _, root := range roots {
		tw := newTTFBWriter()
		start := time.Now()
		_, err := session.ReadSector(cm.Tip(), tw, root, 0, proto2.SectorSize, accountPayment, types.Siacoins(1).Div64(10)) // just overpay
		readTimes = append(readTimes, time.Since(start))
		ttfbTimes = append(ttfbTimes, tw.TTFB())
		if err != nil {
			return RHPResult{}, fmt.Errorf("failed to read sector: %w", err)
		}
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
