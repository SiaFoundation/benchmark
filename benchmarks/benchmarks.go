package benchmarks

import (
	"time"

	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
	"go.sia.tech/coreutils/chain"
)

func benchmarkV2Network() (*consensus.Network, types.Block) {
	// use modified Zen testnet
	n, genesis := chain.TestnetZen()
	n.InitialTarget = types.BlockID{0xFF}
	n.HardforkDevAddr.Height = 0
	n.HardforkTax.Height = 0
	n.HardforkStorageProof.Height = 0
	n.HardforkOak.Height = 0
	n.HardforkASIC.Height = 0
	n.HardforkFoundation.Height = 0
	n.HardforkV2.AllowHeight = 0
	n.HardforkV2.RequireHeight = 1
	n.MaturityDelay = 5
	n.BlockInterval = time.Second
	return n, genesis
}
