package benchmarks

import (
	"time"

	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
)

func benchmarkV1Network() (*consensus.Network, types.Block) {
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
	n.MaturityDelay = 10
	n.BlockInterval = time.Second
	return n, genesis
}
