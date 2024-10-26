package benchmarks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.sia.tech/cluster/nodes"
	"go.sia.tech/core/gateway"
	proto4 "go.sia.tech/core/rhp/v4"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils"
	"go.sia.tech/coreutils/chain"
	rhp4 "go.sia.tech/coreutils/rhp/v4"
	"go.sia.tech/coreutils/syncer"
	"go.sia.tech/coreutils/testutil"
	"go.sia.tech/coreutils/wallet"
	"go.uber.org/zap"
	"lukechampine.com/frand"
)

type fundAndSign struct {
	w  *wallet.SingleAddressWallet
	pk types.PrivateKey
}

func (fs *fundAndSign) FundV2Transaction(txn *types.V2Transaction, amount types.Currency) (types.ChainIndex, []int, error) {
	return fs.w.FundV2Transaction(txn, amount, true)
}
func (fs *fundAndSign) ReleaseInputs(txns []types.V2Transaction) {
	fs.w.ReleaseInputs(nil, txns)
}

func (fs *fundAndSign) SignV2Inputs(txn *types.V2Transaction, toSign []int) {
	fs.w.SignV2Inputs(txn, toSign)
}
func (fs *fundAndSign) SignHash(h types.Hash256) types.Signature {
	return fs.pk.SignHash(h)
}
func (fs *fundAndSign) PublicKey() types.PublicKey {
	return fs.pk.PublicKey()
}
func (fs *fundAndSign) Address() types.Address {
	return fs.w.Address()
}

type readerLen struct {
	r      io.Reader
	length int
}

func NewReaderLen(buf []byte) rhp4.ReaderLen {
	return &readerLen{r: bytes.NewReader(buf), length: len(buf)}
}

func (r *readerLen) Len() (int, error) {
	return r.length, nil
}

func (r *readerLen) Read(p []byte) (int, error) {
	return r.r.Read(p)
}

// helper to get the last host announcement. Scans the whole chain. Should only
// be used with a local testnet.
func findV2HostAnnouncement(cm *chain.Manager, sk types.PrivateKey) (addresses []chain.NetAddress, err error) {
	var index types.ChainIndex
	hostKey := sk.PublicKey()
	for {
		var applied []chain.ApplyUpdate
		_, applied, err = cm.UpdatesSince(index, 1000)
		if err != nil {
			return nil, fmt.Errorf("failed to get updates: %w", err)
		} else if len(applied) == 0 {
			return
		}
		for _, cau := range applied {
			chain.ForEachV2HostAnnouncement(cau.Block, func(pk types.PublicKey, na []chain.NetAddress) {
				if pk == hostKey {
					addresses = na
				}
			})
			index = cau.State.Index
		}
	}
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

func RHP4(ctx context.Context, dir string, log *zap.Logger) (RHPResult, error) {
	const (
		benchmarkSectors = 256
		benchmarkSize    = benchmarkSectors * proto4.SectorSize
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

	n, genesis := benchmarkV2Network()
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
	}, syncer.WithMaxOutboundPeers(10000), syncer.WithMaxInboundPeers(10000), syncer.WithBanDuration(time.Second), syncer.WithPeerDiscoveryInterval(5*time.Second), syncer.WithSyncInterval(5*time.Second)) // essentially no limit on inbound peers
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create syncer: %w", err)
	}
	defer s.Close()
	go s.Run(ctx)

	// create a node manager
	nm := nodes.NewManager(dir, cm, s, log.Named("cluster"))
	defer nm.Close()

	hostKey, renterKey := types.GeneratePrivateKey(), types.GeneratePrivateKey()

	// start the hostd node
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

	// mine some utxos for the renter
	renterAddr := types.StandardUnlockHash(renterKey.PublicKey())
	if err := nm.MineBlocks(ctx, 150, renterAddr); err != nil {
		return RHPResult{}, fmt.Errorf("failed to mine blocks: %w", err)
	}

	addresses, err := findV2HostAnnouncement(cm, hostKey)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to find host announcement: %w", err)
	} else if len(addresses) == 0 {
		return RHPResult{}, fmt.Errorf("host announcement not found")
	}

	// create a wallet for the renter
	ws := testutil.NewEphemeralWalletStore()
	wm, err := wallet.NewSingleAddressWallet(renterKey, cm, ws)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to create renter wallet: %w", err)
	}
	defer wm.Close()

	if err := scanWallet(ctx, cm, wm, ws); err != nil {
		return RHPResult{}, fmt.Errorf("failed to scan wallet: %w", err)
	}

	// wait for syncing
	time.Sleep(15 * time.Second) // TODO: be better

	transport, err := rhp4.DialSiaMux(ctx, addresses[0].Address, hostKey.PublicKey())
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to dial host: %w", err)
	}
	defer transport.Close()

	settings, err := rhp4.RPCSettings(ctx, transport)
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to get host settings: %w", err)
	}

	fs := &fundAndSign{wm, renterKey}
	formResult, err := rhp4.RPCFormContract(ctx, transport, cm, fs, cm.TipState(), settings.Prices, hostKey.PublicKey(), settings.WalletAddress, proto4.RPCFormContractParams{
		RenterPublicKey: renterKey.PublicKey(),
		RenterAddress:   renterAddr,
		Allowance:       types.Siacoins(50),
		Collateral:      types.Siacoins(100),
		ProofHeight:     cm.Tip().Height + 200,
	})
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to form contract: %w", err)
	}
	revision := formResult.Contract

	accountID := proto4.Account(renterKey.PublicKey())
	fundResult, err := rhp4.RPCFundAccounts(ctx, transport, cm.TipState(), fs, revision, []proto4.AccountDeposit{
		{Account: accountID, Amount: types.Siacoins(10)},
	})
	if err != nil {
		return RHPResult{}, fmt.Errorf("failed to fund account: %w", err)
	}
	revision.Revision = fundResult.Revision

	log.Info("starting upload")
	at := proto4.AccountToken{
		Account:    accountID,
		ValidUntil: time.Now().Add(5 * time.Minute),
	}
	at.Signature = renterKey.SignHash(at.SigHash())

	appendTimes := make([]time.Duration, benchmarkSectors)
	roots := make([]types.Hash256, benchmarkSectors)
	for i := range benchmarkSectors {
		var sector [proto4.SectorSize]byte
		frand.Read(sector[:])

		start := time.Now()
		result, err := rhp4.RPCWriteSector(ctx, transport, settings.Prices, at, NewReaderLen(sector[:]), 10)
		if err != nil {
			return RHPResult{}, fmt.Errorf("failed to append sector %d: %w", i, err)
		}
		appendTimes[i] = time.Since(start)
		roots[i] = result.Root
	}

	log.Info("starting download")
	readTimes := make([]time.Duration, benchmarkSectors)
	ttfbTimes := make([]time.Duration, benchmarkSectors)
	for i, root := range roots {
		start := time.Now()
		tw := newTTFBWriter()
		_, err := rhp4.RPCReadSector(ctx, transport, settings.Prices, at, tw, root, 0, proto4.SectorSize)
		if err != nil {
			return RHPResult{}, fmt.Errorf("failed to read sector %d: %w", i, err)
		}
		readTimes[i] = time.Since(start)
		ttfbTimes[i] = tw.TTFB()
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
