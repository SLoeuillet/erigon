// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package committee_subscription

import (
	"context"
	"fmt"
	"sync"
	"time"

	sentinel "github.com/ledgerwatch/erigon-lib/gointerfaces/sentinelproto"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/log/v3"
	"github.com/ledgerwatch/erigon/cl/aggregation"
	"github.com/ledgerwatch/erigon/cl/beacon/synced_data"
	"github.com/ledgerwatch/erigon/cl/clparams"
	"github.com/ledgerwatch/erigon/cl/cltypes"
	"github.com/ledgerwatch/erigon/cl/cltypes/solid"
	"github.com/ledgerwatch/erigon/cl/gossip"
	"github.com/ledgerwatch/erigon/cl/phase1/core/state"
	"github.com/ledgerwatch/erigon/cl/phase1/network/subnets"
	"github.com/ledgerwatch/erigon/cl/utils/eth_clock"
)

var (
	ErrIgnore                   = fmt.Errorf("ignore")
	ErrCommitteeIndexOutOfRange = fmt.Errorf("committee index out of range")
	ErrWrongSubnet              = fmt.Errorf("attestation is for the wrong subnet")
	ErrNotInPropagationRange    = fmt.Errorf("attestation is not in propagation range. %w", ErrIgnore)
	ErrEpochMismatch            = fmt.Errorf("epoch mismatch")
	ErrExactlyOneBitSet         = fmt.Errorf("exactly one aggregation bit should be set")
	ErrAggregationBitsMismatch  = fmt.Errorf("aggregation bits mismatch committee size")
)

type CommitteeSubscribeMgmt struct {
	indiciesDB   kv.RoDB
	ethClock     eth_clock.EthereumClock
	beaconConfig *clparams.BeaconChainConfig
	netConfig    *clparams.NetworkConfig
	sentinel     sentinel.SentinelClient
	state        *state.CachingBeaconState
	syncedData   *synced_data.SyncedDataManager
	// subscriptions
	aggregationPool    aggregation.AggregationPool
	validatorSubsMutex sync.RWMutex
	validatorSubs      map[uint64]*validatorSub // committeeIndex -> validatorSub
}

func NewCommitteeSubscribeManagement(
	ctx context.Context,
	indiciesDB kv.RoDB,
	beaconConfig *clparams.BeaconChainConfig,
	netConfig *clparams.NetworkConfig,
	ethClock eth_clock.EthereumClock,
	sentinel sentinel.SentinelClient,
	state *state.CachingBeaconState,
	aggregationPool aggregation.AggregationPool,
	syncedData *synced_data.SyncedDataManager,
) *CommitteeSubscribeMgmt {
	c := &CommitteeSubscribeMgmt{
		indiciesDB:      indiciesDB,
		beaconConfig:    beaconConfig,
		netConfig:       netConfig,
		ethClock:        ethClock,
		sentinel:        sentinel,
		state:           state,
		aggregationPool: aggregationPool,
		syncedData:      syncedData,
		validatorSubs:   make(map[uint64]*validatorSub),
	}
	go c.sweepByStaleSlots(ctx)
	return c
}

type validatorSub struct {
	subnetId         uint64
	aggregate        bool
	latestTargetSlot uint64
}

func (c *CommitteeSubscribeMgmt) AddAttestationSubscription(ctx context.Context, p *cltypes.BeaconCommitteeSubscription) error {
	var (
		slot   = p.Slot
		cIndex = p.CommitteeIndex
	)
	headState := c.syncedData.HeadState()
	if headState == nil {
		return fmt.Errorf("head state not available")
	}

	log.Debug("Add attestation subscription", "slot", slot, "committeeIndex", cIndex, "isAggregator", p.IsAggregator, "validatorIndex", p.ValidatorIndex)
	commiteePerSlot := headState.CommitteeCount(p.Slot / c.beaconConfig.SlotsPerEpoch)
	subnetId := subnets.ComputeSubnetForAttestation(commiteePerSlot, slot, cIndex, c.beaconConfig.SlotsPerEpoch, c.netConfig.AttestationSubnetCount)
	// add validator to subscription
	c.validatorSubsMutex.Lock()

	if _, ok := c.validatorSubs[cIndex]; !ok {
		c.validatorSubs[cIndex] = &validatorSub{
			subnetId:         subnetId,
			aggregate:        p.IsAggregator,
			latestTargetSlot: slot,
		}
	} else {
		// set aggregator to true if any validator in the committee is an aggregator
		c.validatorSubs[cIndex].aggregate = (c.validatorSubs[cIndex].aggregate || p.IsAggregator)
		// update latest target slot
		if c.validatorSubs[cIndex].latestTargetSlot < slot {
			c.validatorSubs[cIndex].latestTargetSlot = slot
		}
	}

	c.validatorSubsMutex.Unlock()

	// set sentinel gossip expiration by subnet id
	request := sentinel.RequestSubscribeExpiry{
		Topic:          gossip.TopicNameBeaconAttestation(subnetId),
		ExpiryUnixSecs: uint64(time.Now().Add(30 * time.Minute).Unix()), // temporarily set to 30 minutes
	}
	if _, err := c.sentinel.SetSubscribeExpiry(ctx, &request); err != nil {
		return err
	}
	return nil
}

func (c *CommitteeSubscribeMgmt) CheckAggregateAttestation(att *solid.Attestation) error {
	committeeIndex := att.AttestantionData().CommitteeIndex()
	c.validatorSubsMutex.RLock()
	defer c.validatorSubsMutex.RUnlock()
	if sub, ok := c.validatorSubs[committeeIndex]; ok && sub.aggregate {
		// aggregate attestation
		if err := c.aggregationPool.AddAttestation(att); err != nil {
			return err
		}
	}
	return nil
}

func (c *CommitteeSubscribeMgmt) sweepByStaleSlots(ctx context.Context) {
	slotIsStale := func(curSlot, targetSlot uint64) bool {
		if curSlot <= targetSlot {
			// Avoid subtracting unsigned integers
			return false
		}
		return curSlot-targetSlot > c.netConfig.AttestationPropagationSlotRange
	}
	// sweep every minute
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			curSlot := c.ethClock.GetCurrentSlot()
			c.validatorSubsMutex.Lock()
			for committeeIdx, sub := range c.validatorSubs {
				if slotIsStale(curSlot, sub.latestTargetSlot) {
					delete(c.validatorSubs, committeeIdx)
				}
			}
			c.validatorSubsMutex.Unlock()
		}
	}
}
