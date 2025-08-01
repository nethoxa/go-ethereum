// Copyright 2023 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package catalyst

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/txpool"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/params/forks"
	"github.com/ethereum/go-ethereum/rpc"
)

const devEpochLength = 32

// withdrawalQueue implements a FIFO queue which holds withdrawals that are
// pending inclusion.
type withdrawalQueue struct {
	pending types.Withdrawals
	mu      sync.Mutex
	feed    event.Feed
	subs    event.SubscriptionScope
}

type newWithdrawalsEvent struct{ Withdrawals types.Withdrawals }

// add queues a withdrawal for future inclusion.
func (w *withdrawalQueue) add(withdrawal *types.Withdrawal) error {
	w.mu.Lock()
	w.pending = append(w.pending, withdrawal)
	w.mu.Unlock()

	w.feed.Send(newWithdrawalsEvent{types.Withdrawals{withdrawal}})
	return nil
}

// pop dequeues the specified number of withdrawals from the queue.
func (w *withdrawalQueue) pop(count int) types.Withdrawals {
	w.mu.Lock()
	defer w.mu.Unlock()

	count = min(count, len(w.pending))
	popped := w.pending[0:count]
	w.pending = w.pending[count:]

	return popped
}

// subscribe allows a listener to be updated when new withdrawals are added to
// the queue.
func (w *withdrawalQueue) subscribe(ch chan<- newWithdrawalsEvent) event.Subscription {
	sub := w.feed.Subscribe(ch)
	return w.subs.Track(sub)
}

// SimulatedBeacon drives an Ethereum instance as if it were a real beacon
// client. It can run in period mode where it mines a new block every period
// (seconds) or on every transaction via Commit, Fork and AdjustTime.
type SimulatedBeacon struct {
	shutdownCh  chan struct{}
	eth         *eth.Ethereum
	period      uint64
	withdrawals withdrawalQueue

	feeRecipient     common.Address
	feeRecipientLock sync.Mutex // lock gates concurrent access to the feeRecipient

	engineAPI          *ConsensusAPI
	curForkchoiceState engine.ForkchoiceStateV1
	lastBlockTime      uint64
}

func payloadVersion(config *params.ChainConfig, time uint64) engine.PayloadVersion {
	switch config.LatestFork(time) {
	case forks.Prague, forks.Cancun:
		return engine.PayloadV3
	case forks.Paris, forks.Shanghai:
		return engine.PayloadV2
	}
	panic("invalid fork, simulated beacon needs to be started post-merge")
}

// NewSimulatedBeacon constructs a new simulated beacon chain.
func NewSimulatedBeacon(period uint64, feeRecipient common.Address, eth *eth.Ethereum) (*SimulatedBeacon, error) {
	block := eth.BlockChain().CurrentBlock()
	current := engine.ForkchoiceStateV1{
		HeadBlockHash:      block.Hash(),
		SafeBlockHash:      block.Hash(),
		FinalizedBlockHash: block.Hash(),
	}
	engineAPI := newConsensusAPIWithoutHeartbeat(eth)

	// if genesis block, send forkchoiceUpdated to trigger transition to PoS
	if block.Number.Sign() == 0 {
		version := payloadVersion(eth.BlockChain().Config(), block.Time)
		if _, err := engineAPI.forkchoiceUpdated(current, nil, version, false); err != nil {
			return nil, err
		}
	}

	// cap the dev mode period to a reasonable maximum value to avoid
	// overflowing the time.Duration (int64) that it will occupy
	const maxPeriod = uint64(math.MaxInt64 / time.Second)
	return &SimulatedBeacon{
		eth:                eth,
		period:             min(period, maxPeriod),
		shutdownCh:         make(chan struct{}),
		engineAPI:          engineAPI,
		lastBlockTime:      block.Time,
		curForkchoiceState: current,
		feeRecipient:       feeRecipient,
	}, nil
}

func (c *SimulatedBeacon) setFeeRecipient(feeRecipient common.Address) {
	c.feeRecipientLock.Lock()
	c.feeRecipient = feeRecipient
	c.feeRecipientLock.Unlock()
}

// Start invokes the SimulatedBeacon life-cycle function in a goroutine.
func (c *SimulatedBeacon) Start() error {
	if c.period == 0 {
		// if period is set to 0, do not mine at all
		// this is used in the simulated backend where blocks
		// are explicitly mined via Commit, AdjustTime and Fork
	} else {
		go c.loop()
	}
	return nil
}

// Stop halts the SimulatedBeacon service.
func (c *SimulatedBeacon) Stop() error {
	close(c.shutdownCh)
	return nil
}

// sealBlock initiates payload building for a new block and creates a new block
// with the completed payload.
func (c *SimulatedBeacon) sealBlock(withdrawals []*types.Withdrawal, timestamp uint64) error {
	if timestamp <= c.lastBlockTime {
		timestamp = c.lastBlockTime + 1
	}
	c.feeRecipientLock.Lock()
	feeRecipient := c.feeRecipient
	c.feeRecipientLock.Unlock()

	// Reset to CurrentBlock in case of the chain was rewound
	if header := c.eth.BlockChain().CurrentBlock(); c.curForkchoiceState.HeadBlockHash != header.Hash() {
		finalizedHash := c.finalizedBlockHash(header.Number.Uint64())
		c.setCurrentState(header.Hash(), *finalizedHash)
	}

	// Because transaction insertion, block insertion, and block production will
	// happen without any timing delay between them in simulator mode and the
	// transaction pool will be running its internal reset operation on a
	// background thread, flaky executions can happen. To avoid the racey
	// behavior, the pool will be explicitly blocked on its reset before
	// continuing to the block production below.
	if err := c.eth.APIBackend.TxPool().Sync(); err != nil {
		return fmt.Errorf("failed to sync txpool: %w", err)
	}

	version := payloadVersion(c.eth.BlockChain().Config(), timestamp)

	var random [32]byte
	rand.Read(random[:])
	fcResponse, err := c.engineAPI.forkchoiceUpdated(c.curForkchoiceState, &engine.PayloadAttributes{
		Timestamp:             timestamp,
		SuggestedFeeRecipient: feeRecipient,
		Withdrawals:           withdrawals,
		Random:                random,
		BeaconRoot:            &common.Hash{},
	}, version, false)
	if err != nil {
		return err
	}
	if fcResponse == engine.STATUS_SYNCING {
		return errors.New("chain rewind prevented invocation of payload creation")
	}

	// If the payload was already known, we can skip the rest of the process.
	// This edge case is possible due to a race condition between seal and debug.setHead.
	if fcResponse.PayloadStatus.Status == engine.VALID && fcResponse.PayloadID == nil {
		return nil
	}

	envelope, err := c.engineAPI.getPayload(*fcResponse.PayloadID, true)
	if err != nil {
		return err
	}
	payload := envelope.ExecutionPayload

	var finalizedHash common.Hash
	if payload.Number%devEpochLength == 0 {
		finalizedHash = payload.BlockHash
	} else {
		if fh := c.finalizedBlockHash(payload.Number); fh == nil {
			return errors.New("chain rewind interrupted calculation of finalized block hash")
		} else {
			finalizedHash = *fh
		}
	}

	var (
		blobHashes []common.Hash
		beaconRoot *common.Hash
		requests   [][]byte
	)
	// Compute post-shanghai fields
	if version > engine.PayloadV2 {
		// Independently calculate the blob hashes from sidecars.
		blobHashes = make([]common.Hash, 0)
		if envelope.BlobsBundle != nil {
			hasher := sha256.New()
			for _, commit := range envelope.BlobsBundle.Commitments {
				var c kzg4844.Commitment
				if len(commit) != len(c) {
					return errors.New("invalid commitment length")
				}
				copy(c[:], commit)
				blobHashes = append(blobHashes, kzg4844.CalcBlobHashV1(hasher, &c))
			}
		}
		beaconRoot = &common.Hash{}
		requests = envelope.Requests
	}

	// Mark the payload as canon
	_, err = c.engineAPI.newPayload(*payload, blobHashes, beaconRoot, requests, false)
	if err != nil {
		return err
	}
	c.setCurrentState(payload.BlockHash, finalizedHash)

	// Mark the block containing the payload as canonical
	if _, err = c.engineAPI.forkchoiceUpdated(c.curForkchoiceState, nil, version, false); err != nil {
		return err
	}
	c.lastBlockTime = payload.Timestamp
	return nil
}

// loop runs the block production loop for non-zero period configuration
func (c *SimulatedBeacon) loop() {
	timer := time.NewTimer(0)
	for {
		select {
		case <-c.shutdownCh:
			return
		case <-timer.C:
			if err := c.sealBlock(c.withdrawals.pop(10), uint64(time.Now().Unix())); err != nil {
				log.Warn("Error performing sealing work", "err", err)
			} else {
				timer.Reset(time.Second * time.Duration(c.period))
			}
		}
	}
}

// finalizedBlockHash returns the block hash of the finalized block corresponding
// to the given number or nil if doesn't exist in the chain.
func (c *SimulatedBeacon) finalizedBlockHash(number uint64) *common.Hash {
	var finalizedNumber uint64
	if number%devEpochLength == 0 {
		finalizedNumber = number
	} else {
		finalizedNumber = (number - 1) / devEpochLength * devEpochLength
	}
	if finalizedBlock := c.eth.BlockChain().GetBlockByNumber(finalizedNumber); finalizedBlock != nil {
		fh := finalizedBlock.Hash()
		return &fh
	}
	return nil
}

// setCurrentState sets the current forkchoice state
func (c *SimulatedBeacon) setCurrentState(headHash, finalizedHash common.Hash) {
	c.curForkchoiceState = engine.ForkchoiceStateV1{
		HeadBlockHash:      headHash,
		SafeBlockHash:      headHash,
		FinalizedBlockHash: finalizedHash,
	}
}

// Commit seals a block on demand.
func (c *SimulatedBeacon) Commit() common.Hash {
	withdrawals := c.withdrawals.pop(10)
	if err := c.sealBlock(withdrawals, uint64(time.Now().Unix())); err != nil {
		log.Warn("Error performing sealing work", "err", err)
	}
	return c.eth.BlockChain().CurrentBlock().Hash()
}

// Rollback un-sends previously added transactions.
func (c *SimulatedBeacon) Rollback() {
	c.eth.TxPool().Clear()
}

// Fork sets the head to the provided hash.
func (c *SimulatedBeacon) Fork(parentHash common.Hash) error {
	// Ensure no pending transactions.
	c.eth.TxPool().Sync()
	if len(c.eth.TxPool().Pending(txpool.PendingFilter{})) != 0 {
		return errors.New("pending block dirty")
	}

	parent := c.eth.BlockChain().GetBlockByHash(parentHash)
	if parent == nil {
		return errors.New("parent not found")
	}
	_, err := c.eth.BlockChain().SetCanonical(parent)
	return err
}

// AdjustTime creates a new block with an adjusted timestamp.
func (c *SimulatedBeacon) AdjustTime(adjustment time.Duration) error {
	if len(c.eth.TxPool().Pending(txpool.PendingFilter{})) != 0 {
		return errors.New("could not adjust time on non-empty block")
	}
	parent := c.eth.BlockChain().CurrentBlock()
	if parent == nil {
		return errors.New("parent not found")
	}
	withdrawals := c.withdrawals.pop(10)
	return c.sealBlock(withdrawals, parent.Time+uint64(adjustment/time.Second))
}

// RegisterSimulatedBeaconAPIs registers the simulated beacon's API with the
// stack.
func RegisterSimulatedBeaconAPIs(stack *node.Node, sim *SimulatedBeacon) {
	api := newSimulatedBeaconAPI(sim)
	stack.RegisterAPIs([]rpc.API{
		{
			Namespace: "dev",
			Service:   api,
			Version:   "1.0",
		},
	})
}
