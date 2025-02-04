// Copyright 2021 Kaleido
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package events

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hyperledger-labs/firefly-fabconnect/internal/errors"
	eventsapi "github.com/hyperledger-labs/firefly-fabconnect/internal/events/api"
	"github.com/hyperledger-labs/firefly-fabconnect/internal/fabric"
	"github.com/hyperledger/fabric-sdk-go/pkg/client/event"
	"github.com/hyperledger/fabric-sdk-go/pkg/common/providers/fab"
	log "github.com/sirupsen/logrus"
)

// subscription is the runtime that manages the subscription
type subscription struct {
	info               *eventsapi.SubscriptionInfo
	client             fabric.RPCClient
	ep                 *evtProcessor
	registration       fab.Registration
	blockEventNotifier <-chan *fab.BlockEvent
	ccEventNotifier    <-chan *fab.CCEvent
	eventClient        *event.Client
	filterStale        bool
	deleting           bool
	resetRequested     bool
}

func newSubscription(sm subscriptionManager, rpc fabric.RPCClient, i *eventsapi.SubscriptionInfo) (*subscription, error) {
	stream, err := sm.streamByID(i.Stream)
	if err != nil {
		return nil, err
	}
	s := &subscription{
		info:        i,
		client:      rpc,
		ep:          newEvtProcessor(i.ID, stream),
		filterStale: true,
	}
	i.Summary = fmt.Sprintf(`FromBlock=%s,Chaincode=%s,Filter=%s`, i.FromBlock, i.Filter.ChaincodeId, i.Filter.Filter)
	// If a name was not provided by the end user, set it to the system generated summary
	if i.Name == "" {
		log.Debugf("No name provided for subscription, using auto-generated ID:%s", i.ID)
		i.Name = i.ID
	}
	log.Infof("Created subscription ID:%s Chaincode: %s name:%s", i.ID, i.Filter.ChaincodeId, i.Name)
	return s, nil
}

func restoreSubscription(sm subscriptionManager, rpc fabric.RPCClient, i *eventsapi.SubscriptionInfo) (*subscription, error) {
	if i.GetID() == "" {
		return nil, errors.Errorf(errors.EventStreamsNoID)
	}
	stream, err := sm.streamByID(i.Stream)
	if err != nil {
		return nil, err
	}
	s := &subscription{
		client:      rpc,
		info:        i,
		ep:          newEvtProcessor(i.ID, stream),
		filterStale: true,
	}
	return s, nil
}

func (s *subscription) setInitialBlockHeight(ctx context.Context) (uint64, error) {
	log.Debugf(`%s: Setting initial block height. "fromBlock" value in the subscription is %s`, s.info.ID, s.info.FromBlock)
	if s.info.FromBlock != "" && s.info.FromBlock != FromBlockNewest {
		fromBlock, err := strconv.ParseUint(s.info.FromBlock, 10, 64)
		if err != nil {
			return 0, errors.Errorf(errors.EventStreamsSubscribeBadBlock)
		}
		log.Infof("%s: initial block height for subscription (latest block): %d", s.info.ID, fromBlock)
		return fromBlock, nil
	}
	result, err := s.client.QueryChainInfo(s.info.ChannelId, s.info.Signer)
	if err != nil {
		return 0, errors.Errorf(errors.RPCCallReturnedError, "QSCC GetChainInfo()", err)
	}
	i := result.BCI.Height
	s.ep.initBlockHWM(i)
	log.Infof("%s: initial block height for subscription (latest block): %d", s.info.ID, i)
	return i, nil
}

func (s *subscription) setCheckpointBlockHeight(i uint64) {
	s.ep.initBlockHWM(i)
	log.Infof("%s: checkpoint restored block height for subscription: %d", s.info.ID, i)
}

func (s *subscription) restartFilter(ctx context.Context, since uint64) error {
	reg, blockEventNotifier, ccEventNotifier, eventClient, err := s.client.SubscribeEvent(s.info, since)
	if err != nil {
		return errors.Errorf(errors.RPCCallReturnedError, "SubscribeEvent", err)
	}
	s.registration = reg
	s.blockEventNotifier = blockEventNotifier
	s.ccEventNotifier = ccEventNotifier
	s.eventClient = eventClient
	s.markFilterStale(false)

	// launch the events relay from the events pipe coming from the node to the batch queue
	go s.processNewEvents()

	log.Infof("%s: created filter from block %d: %+v", s.info.ID, since, s.info.Filter)
	return err
}

func (s *subscription) processNewEvents() {
	for {
		select {
		case blockEvent, ok := <-s.blockEventNotifier:
			if !ok {
				log.Infof("%s: Block event notifier channel closed", s.info.ID)
				return
			}
			events := fabric.GetEvents(blockEvent.Block)
			for _, event := range events {
				if err := s.ep.processEventEntry(s.info.ID, event); err != nil {
					log.Errorf("Failed to process event: %s", err)
				}
			}
		case ccEvent, ok := <-s.ccEventNotifier:
			if !ok {
				log.Infof("%s: Chaincode event notifier channel closed", s.info.ID)
				return
			}
			event := &eventsapi.EventEntry{
				ChaincodeId:   ccEvent.ChaincodeID,
				BlockNumber:   ccEvent.BlockNumber,
				TransactionId: ccEvent.TxID,
				EventName:     ccEvent.EventName,
				Payload:       ccEvent.Payload,
			}
			if err := s.ep.processEventEntry(s.info.ID, event); err != nil {
				log.Errorf("Failed to process event: %s", err)
			}
		}
	}
}

func (s *subscription) unsubscribe(deleting bool) {
	log.Infof("%s: Unsubscribing existing filter (deleting=%t)", s.info.ID, deleting)
	s.deleting = deleting
	s.resetRequested = false
	s.markFilterStale(true)
}

func (s *subscription) requestReset() {
	// We simply set a flag, which is picked up by the event stream thread on the next polling cycle
	// and results in an unsubscribe/subscribe cycle.
	log.Infof("%s: Requested reset from block '%s'", s.info.ID, s.info.FromBlock)
	s.resetRequested = true
}

func (s *subscription) blockHWM() uint64 {
	return s.ep.getBlockHWM()
}

func (s *subscription) markFilterStale(newFilterStale bool) {
	log.Debugf("%s: Marking filter stale=%t, current sub filter stale=%t", s.info.ID, newFilterStale, s.filterStale)
	// If unsubscribe is called multiple times, we might not have a filter
	if newFilterStale && !s.filterStale {
		s.eventClient.Unregister(s.registration)
		// We treat error as informational here - the filter might already not be valid (if the node restarted)
		log.Infof("%s: Uninstalled subscription by unregistering", s.info.ID)
	}
	s.filterStale = newFilterStale
}

func (s *subscription) close() {
	// the unregistration will close the notifier channel which will
	// terminate the processNewEvents() go routine
	log.Debugf("%s: Unregistering event listener", s.info.ID)
	s.eventClient.Unregister(s.registration)
}
