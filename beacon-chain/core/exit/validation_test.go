package exit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/go-ssz"
	mockChain "github.com/prysmaticlabs/prysm/beacon-chain/blockchain/testing"
	blk "github.com/prysmaticlabs/prysm/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/exit"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/state"
	dbutil "github.com/prysmaticlabs/prysm/beacon-chain/db/testing"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/testutil"
)

// Set genesis to a small set for faster test processing.
func init() {
	p := params.BeaconConfig()
	p.MinGenesisActiveValidatorCount = 8
	params.OverrideBeaconConfig(p)
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name           string
		epoch          uint64
		validatorIndex uint64
		signature      []byte
		err            error
	}{
		{
			name:           "MissingValidator",
			epoch:          2048,
			validatorIndex: 16,
			err:            errors.New("unknown validator index 16"),
		},
		{
			name:           "EarlyExit",
			epoch:          2047,
			validatorIndex: 0,
			err:            errors.New("validator 0 cannot exit before epoch 2048"),
		},
		{
			name:           "NoSignature",
			epoch:          2048,
			validatorIndex: 0,
			err:            errors.New("malformed signature: signature must be 96 bytes"),
		},
		{
			name:           "InvalidSignature",
			epoch:          2048,
			validatorIndex: 0,
			signature:      []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			err:            errors.New("malformed signature: could not unmarshal bytes into signature: err blsSignatureDeserialize 000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000"),
		},
		{
			name:           "IncorrectSignature",
			epoch:          2048,
			validatorIndex: 0,
			signature:      []byte{0xab, 0xb0, 0x12, 0x4c, 0x75, 0x74, 0xf2, 0x81, 0xa2, 0x93, 0xf4, 0x18, 0x5c, 0xad, 0x3c, 0xb2, 0x26, 0x81, 0xd5, 0x20, 0x91, 0x7c, 0xe4, 0x66, 0x65, 0x24, 0x3e, 0xac, 0xb0, 0x51, 0x00, 0x0d, 0x8b, 0xac, 0xf7, 0x5e, 0x14, 0x51, 0x87, 0x0c, 0xa6, 0xb3, 0xb9, 0xe6, 0xc9, 0xd4, 0x1a, 0x7b, 0x02, 0xea, 0xd2, 0x68, 0x5a, 0x84, 0x18, 0x8a, 0x4f, 0xaf, 0xd3, 0x82, 0x5d, 0xaf, 0x6a, 0x98, 0x96, 0x25, 0xd7, 0x19, 0xcc, 0xd2, 0xd8, 0x3a, 0x40, 0x10, 0x1f, 0x4a, 0x45, 0x3f, 0xca, 0x62, 0x87, 0x8c, 0x89, 0x0e, 0xca, 0x62, 0x23, 0x63, 0xf9, 0xdd, 0xb8, 0xf3, 0x67, 0xa9, 0x1e, 0x84},
			err:            errors.New("incorrect signature"),
		},
		{
			name:           "Good",
			epoch:          2048,
			validatorIndex: 0,
			signature:      []byte{0xb3, 0xe1, 0x9d, 0xc6, 0x7c, 0x78, 0x6c, 0xcf, 0x33, 0x1d, 0xb9, 0x6f, 0x59, 0x64, 0x44, 0xe1, 0x29, 0xd0, 0x87, 0x03, 0x26, 0x6e, 0x49, 0x1c, 0x05, 0xae, 0x16, 0x7b, 0x04, 0x0f, 0x3f, 0xf8, 0x82, 0x77, 0x60, 0xfc, 0xcf, 0x2f, 0x59, 0xc7, 0x40, 0x0b, 0x2c, 0xa9, 0x23, 0x8a, 0x6c, 0x8d, 0x01, 0x21, 0x5e, 0xa8, 0xac, 0x36, 0x70, 0x31, 0xb0, 0xe1, 0xa8, 0xb8, 0x8f, 0x93, 0x8c, 0x1c, 0xa2, 0x86, 0xe7, 0x22, 0x00, 0x6a, 0x7d, 0x36, 0xc0, 0x2b, 0x86, 0x2c, 0xf5, 0xf9, 0x10, 0xb9, 0xf2, 0xbd, 0x5e, 0xa6, 0x5f, 0x12, 0x86, 0x43, 0x20, 0x4d, 0xa2, 0x9d, 0x8b, 0xe6, 0x6f, 0x09},
		},
	}

	db := dbutil.SetupDB(t)
	defer dbutil.TeardownDB(t, db)
	ctx := context.Background()
	deposits, _, _ := testutil.DeterministicDepositsAndKeys(params.BeaconConfig().MinGenesisActiveValidatorCount)
	beaconState, err := state.GenesisBeaconState(deposits, 0, &ethpb.Eth1Data{BlockHash: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	block := blk.NewGenesisBlock([]byte{})
	if err := db.SaveBlock(ctx, block); err != nil {
		t.Fatalf("Could not save genesis block: %v", err)
	}
	genesisRoot, err := ssz.HashTreeRoot(block.Block)
	if err != nil {
		t.Fatalf("Could not get signing root %v", err)
	}

	// Set genesis time to be 100 epochs ago
	genesisTime := time.Now().Add(time.Duration(-100*int64(params.BeaconConfig().SecondsPerSlot*params.BeaconConfig().SlotsPerEpoch)) * time.Second)
	mockChainService := &mockChain.ChainService{State: beaconState, Root: genesisRoot[:], Genesis: genesisTime}
	headState, err := mockChainService.HeadState(context.Background())
	if err != nil {
		t.Fatal("Failed to obtain head state")
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := &ethpb.SignedVoluntaryExit{
				Exit: &ethpb.VoluntaryExit{
					Epoch:          test.epoch,
					ValidatorIndex: test.validatorIndex,
				},
				Signature: test.signature,
			}

			err := exit.ValidateVoluntaryExit(headState, genesisTime, req)
			if test.err == nil {
				if err != nil {
					t.Errorf("Unexpected error: received %v", err)
				}
			} else {
				if err == nil {
					t.Error("Failed to receive expected error")
				}
				if err.Error() != test.err.Error() {
					t.Errorf("Unexpected error: expected %s, received %s", test.err.Error(), err.Error())
				}
			}
		})
	}
}
