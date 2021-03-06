package blockchain

import (
	"bytes"
	"context"
	"encoding/hex"

	"github.com/gogo/protobuf/proto"
	"github.com/pkg/errors"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/go-ssz"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/epoch/precompute"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/feed"
	statefeed "github.com/prysmaticlabs/prysm/beacon-chain/core/feed/state"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/featureconfig"
	"github.com/prysmaticlabs/prysm/shared/traceutil"
	"github.com/sirupsen/logrus"
	"go.opencensus.io/trace"
)

// BlockReceiver interface defines the methods of chain service receive and processing new blocks.
type BlockReceiver interface {
	ReceiveBlock(ctx context.Context, block *ethpb.SignedBeaconBlock) error
	ReceiveBlockNoPubsub(ctx context.Context, block *ethpb.SignedBeaconBlock) error
	ReceiveBlockNoPubsubForkchoice(ctx context.Context, block *ethpb.SignedBeaconBlock) error
	ReceiveBlockNoVerify(ctx context.Context, block *ethpb.SignedBeaconBlock) error
}

// ReceiveBlock is a function that defines the operations that are preformed on
// blocks that is received from rpc service. The operations consists of:
//   1. Gossip block to other peers
//   2. Validate block, apply state transition and update check points
//   3. Apply fork choice to the processed block
//   4. Save latest head info
func (s *Service) ReceiveBlock(ctx context.Context, block *ethpb.SignedBeaconBlock) error {
	ctx, span := trace.StartSpan(ctx, "beacon-chain.blockchain.ReceiveBlock")
	defer span.End()

	root, err := ssz.HashTreeRoot(block.Block)
	if err != nil {
		return errors.Wrap(err, "could not get signing root on received block")
	}

	// Broadcast the new block to the network.
	if err := s.p2p.Broadcast(ctx, block); err != nil {
		return errors.Wrap(err, "could not broadcast block")
	}
	log.WithFields(logrus.Fields{
		"blockRoot": hex.EncodeToString(root[:]),
	}).Debug("Broadcasting block")

	if err := s.ReceiveBlockNoPubsub(ctx, block); err != nil {
		return err
	}

	processedBlk.Inc()
	return nil
}

// ReceiveBlockNoPubsub is a function that defines the the operations (minus pubsub)
// that are preformed on blocks that is received from regular sync service. The operations consists of:
//   1. Validate block, apply state transition and update check points
//   2. Apply fork choice to the processed block
//   3. Save latest head info
func (s *Service) ReceiveBlockNoPubsub(ctx context.Context, block *ethpb.SignedBeaconBlock) error {
	ctx, span := trace.StartSpan(ctx, "beacon-chain.blockchain.ReceiveBlockNoPubsub")
	defer span.End()
	blockCopy := proto.Clone(block).(*ethpb.SignedBeaconBlock)

	// Apply state transition on the new block.
	if err := s.forkChoiceStore.OnBlockCacheFilteredTree(ctx, blockCopy); err != nil {
		err := errors.Wrap(err, "could not process block from fork choice service")
		traceutil.AnnotateError(span, err)
		return err
	}
	root, err := ssz.HashTreeRoot(blockCopy.Block)
	if err != nil {
		return errors.Wrap(err, "could not get signing root on received block")
	}

	if featureconfig.Get().DisableForkChoice {
		if err := s.saveHead(ctx, blockCopy, root); err != nil {
			return errors.Wrap(err, "could not save head")
		}
	} else {
		// Run fork choice after applying state transition on the new block.
		headRoot, err := s.forkChoiceStore.Head(ctx)
		if err != nil {
			return errors.Wrap(err, "could not get head from fork choice service")
		}

		// Only save head if it's different than the current head.
		cachedHeadRoot, err := s.HeadRoot(ctx)
		if err != nil {
			return errors.Wrap(err, "could not get head root from cache")
		}
		if !bytes.Equal(headRoot, cachedHeadRoot) {
			signedHeadBlock, err := s.beaconDB.Block(ctx, bytesutil.ToBytes32(headRoot))
			if err != nil {
				return errors.Wrap(err, "could not compute state from block head")
			}
			if signedHeadBlock == nil || signedHeadBlock.Block == nil {
				return errors.New("nil head block")
			}
			if err := s.saveHead(ctx, signedHeadBlock, bytesutil.ToBytes32(headRoot)); err != nil {
				return errors.Wrap(err, "could not save head")
			}
			isCompetingBlock(root[:], blockCopy.Block.Slot, headRoot, signedHeadBlock.Block.Slot)
		}
	}

	// Send notification of the processed block to the state feed.
	s.stateNotifier.StateFeed().Send(&feed.Event{
		Type: statefeed.BlockProcessed,
		Data: &statefeed.BlockProcessedData{
			BlockRoot: root,
			Verified:  true,
		},
	})

	// Add attestations from the block to the pool for fork choice.
	if err := s.attPool.SaveBlockAttestations(blockCopy.Block.Body.Attestations); err != nil {
		log.Errorf("Could not save attestation for fork choice: %v", err)
		return nil
	}

	// Reports on block and fork choice metrics.
	s.reportSlotMetrics(blockCopy.Block.Slot)

	// Log state transition data.
	logStateTransitionData(blockCopy.Block)

	s.epochParticipationLock.Lock()
	defer s.epochParticipationLock.Unlock()
	s.epochParticipation[helpers.SlotToEpoch(blockCopy.Block.Slot)] = precompute.Balances

	processedBlkNoPubsub.Inc()

	return nil
}

// ReceiveBlockNoPubsubForkchoice is a function that defines the all operations (minus pubsub and forkchoice)
// that are preformed blocks that is received from initial sync service. The operations consists of:
//   1. Validate block, apply state transition and update check points
//   2. Save latest head info
func (s *Service) ReceiveBlockNoPubsubForkchoice(ctx context.Context, block *ethpb.SignedBeaconBlock) error {
	ctx, span := trace.StartSpan(ctx, "beacon-chain.blockchain.ReceiveBlockNoForkchoice")
	defer span.End()
	blockCopy := proto.Clone(block).(*ethpb.SignedBeaconBlock)

	// Apply state transition on the incoming newly received block.
	if err := s.forkChoiceStore.OnBlock(ctx, blockCopy); err != nil {
		err := errors.Wrap(err, "could not process block from fork choice service")
		traceutil.AnnotateError(span, err)
		return err
	}
	root, err := ssz.HashTreeRoot(blockCopy.Block)
	if err != nil {
		return errors.Wrap(err, "could not get signing root on received block")
	}
	cachedHeadRoot, err := s.HeadRoot(ctx)
	if err != nil {
		return errors.Wrap(err, "could not get head root from cache")
	}
	if !bytes.Equal(root[:], cachedHeadRoot) {
		if err := s.saveHead(ctx, blockCopy, root); err != nil {
			return errors.Wrap(err, "could not save head")
		}
	}

	// Send notification of the processed block to the state feed.
	s.stateNotifier.StateFeed().Send(&feed.Event{
		Type: statefeed.BlockProcessed,
		Data: &statefeed.BlockProcessedData{
			BlockRoot: root,
			Verified:  true,
		},
	})

	// Reports on block and fork choice metrics.
	s.reportSlotMetrics(blockCopy.Block.Slot)

	// Log state transition data.
	logStateTransitionData(blockCopy.Block)

	s.epochParticipationLock.Lock()
	defer s.epochParticipationLock.Unlock()
	s.epochParticipation[helpers.SlotToEpoch(blockCopy.Block.Slot)] = precompute.Balances

	processedBlkNoPubsubForkchoice.Inc()
	return nil
}

// ReceiveBlockNoVerify runs state transition on a input block without verifying the block's BLS contents.
// Depends on the security model, this is the "minimal" work a node can do to sync the chain.
// It simulates light client behavior and assumes 100% trust with the syncing peer.
func (s *Service) ReceiveBlockNoVerify(ctx context.Context, block *ethpb.SignedBeaconBlock) error {
	ctx, span := trace.StartSpan(ctx, "beacon-chain.blockchain.ReceiveBlockNoVerify")
	defer span.End()
	blockCopy := proto.Clone(block).(*ethpb.SignedBeaconBlock)

	// Apply state transition on the incoming newly received blockCopy without verifying its BLS contents.
	if err := s.forkChoiceStore.OnBlockInitialSyncStateTransition(ctx, blockCopy); err != nil {
		return errors.Wrap(err, "could not process blockCopy from fork choice service")
	}
	root, err := ssz.HashTreeRoot(blockCopy.Block)
	if err != nil {
		return errors.Wrap(err, "could not get signing root on received blockCopy")
	}

	cachedHeadRoot, err := s.HeadRoot(ctx)
	if err != nil {
		return errors.Wrap(err, "could not get head root from cache")
	}

	if featureconfig.Get().InitSyncCacheState {
		if !bytes.Equal(root[:], cachedHeadRoot) {
			if err := s.saveHeadNoDB(ctx, blockCopy, root); err != nil {
				err := errors.Wrap(err, "could not save head")
				traceutil.AnnotateError(span, err)
				return err
			}
		}
	} else {
		if !bytes.Equal(root[:], cachedHeadRoot) {
			if err := s.saveHead(ctx, blockCopy, root); err != nil {
				err := errors.Wrap(err, "could not save head")
				traceutil.AnnotateError(span, err)
				return err
			}
		}
	}

	// Send notification of the processed block to the state feed.
	s.stateNotifier.StateFeed().Send(&feed.Event{
		Type: statefeed.BlockProcessed,
		Data: &statefeed.BlockProcessedData{
			BlockRoot: root,
			Verified:  false,
		},
	})

	// Reports on blockCopy and fork choice metrics.
	s.reportSlotMetrics(blockCopy.Block.Slot)

	// Log state transition data.
	log.WithFields(logrus.Fields{
		"slot":         blockCopy.Block.Slot,
		"attestations": len(blockCopy.Block.Body.Attestations),
		"deposits":     len(blockCopy.Block.Body.Deposits),
	}).Debug("Finished applying state transition")

	s.epochParticipationLock.Lock()
	defer s.epochParticipationLock.Unlock()
	s.epochParticipation[helpers.SlotToEpoch(blockCopy.Block.Slot)] = precompute.Balances

	return nil
}

// This checks if the block is from a competing chain, emits warning and updates metrics.
func isCompetingBlock(root []byte, slot uint64, headRoot []byte, headSlot uint64) {
	if !bytes.Equal(root[:], headRoot) {
		log.WithFields(logrus.Fields{
			"blkSlot":  slot,
			"blkRoot":  hex.EncodeToString(root[:]),
			"headSlot": headSlot,
			"headRoot": hex.EncodeToString(headRoot),
		}).Warn("Calculated head diffs from new block")
		competingBlks.Inc()
	}
}
