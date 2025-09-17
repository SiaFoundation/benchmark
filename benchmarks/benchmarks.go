package benchmarks

import (
	"time"

	"go.sia.tech/core/consensus"
	"go.sia.tech/core/types"
)

func benchmarkV2Network() (*consensus.Network, types.Block) {
	mustParseAddr := func(s string) types.Address {
		a, err := types.ParseAddress(s)
		if err != nil {
			panic(err)
		}
		return a
	}
	n := &consensus.Network{
		Name: "benchmark",

		InitialCoinbase: types.Siacoins(300000),
		MinimumCoinbase: types.Siacoins(300000),
		InitialTarget:   types.BlockID{0: 1},
		BlockInterval:   100 * time.Millisecond,
		MaturityDelay:   5,

		HardforkDevAddr: struct {
			Height     uint64        `json:"height"`
			OldAddress types.Address `json:"oldAddress"`
			NewAddress types.Address `json:"newAddress"`
		}{
			Height:     0,
			OldAddress: types.Address{},
			NewAddress: types.Address{},
		},

		HardforkTax: struct {
			Height uint64 `json:"height"`
		}{
			Height: 0,
		},

		HardforkStorageProof: struct {
			Height uint64 `json:"height"`
		}{
			Height: 0,
		},

		HardforkOak: struct {
			Height           uint64    `json:"height"`
			FixHeight        uint64    `json:"fixHeight"`
			GenesisTimestamp time.Time `json:"genesisTimestamp"`
		}{
			Height:           0,
			FixHeight:        0,
			GenesisTimestamp: time.Now(),
		},

		HardforkASIC: struct {
			Height      uint64        `json:"height"`
			OakTime     time.Duration `json:"oakTime"`
			OakTarget   types.BlockID `json:"oakTarget"`
			NonceFactor uint64        `json:"nonceFactor"`
		}{
			Height:      0,
			OakTime:     10 * time.Millisecond,
			OakTarget:   types.BlockID{0: 1},
			NonceFactor: 1009,
		},

		HardforkFoundation: struct {
			Height          uint64        `json:"height"`
			PrimaryAddress  types.Address `json:"primaryAddress"`
			FailsafeAddress types.Address `json:"failsafeAddress"`
		}{
			Height:          0,
			PrimaryAddress:  mustParseAddr("053b2def3cbdd078c19d62ce2b4f0b1a3c5e0ffbeeff01280efb1f8969b2f5bb4fdc680f0807"),
			FailsafeAddress: types.VoidAddress,
		},

		HardforkV2: struct {
			AllowHeight    uint64 `json:"allowHeight"`
			RequireHeight  uint64 `json:"requireHeight"`
			FinalCutHeight uint64 `json:"finalCutHeight"`
		}{
			AllowHeight:    0,
			RequireHeight:  1,
			FinalCutHeight: 5,
		},
	}

	b := types.Block{
		Timestamp: n.HardforkOak.GenesisTimestamp,
		Transactions: []types.Transaction{{
			SiacoinOutputs: []types.SiacoinOutput{{
				Address: mustParseAddr("3d7f707d05f2e0ec7ccc9220ed7c8af3bc560fbee84d068c2cc28151d617899e1ee8bc069946"),
				Value:   types.Siacoins(1).Mul64(1e12),
			}},
			SiafundOutputs: []types.SiafundOutput{{
				Address: mustParseAddr("053b2def3cbdd078c19d62ce2b4f0b1a3c5e0ffbeeff01280efb1f8969b2f5bb4fdc680f0807"),
				Value:   10000,
			}},
		}},
	}
	return n, b
}
