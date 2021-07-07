package sync

import (
	"bytes"
	"context"

	"github.com/libp2p/go-libp2p-core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	types "github.com/prysmaticlabs/eth2-types"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/altair"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/feed"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/feed/operation"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	p2ptypes "github.com/prysmaticlabs/prysm/beacon-chain/p2p/types"
	iface "github.com/prysmaticlabs/prysm/beacon-chain/state/interface"
	prysmv2 "github.com/prysmaticlabs/prysm/proto/prysm/v2"
	"github.com/prysmaticlabs/prysm/shared/bls"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/interfaces/version"
	"github.com/prysmaticlabs/prysm/shared/params"
	"github.com/prysmaticlabs/prysm/shared/traceutil"
	"go.opencensus.io/trace"
)

// validateSyncContributionAndProof verifies the aggregated signature and the selection proof is valid before forwarding to the
// network and downstream services.
func (s *Service) validateSyncContributionAndProof(ctx context.Context, pid peer.ID, msg *pubsub.Message) pubsub.ValidationResult {
	if pid == s.cfg.P2P.PeerID() {
		return pubsub.ValidationAccept
	}

	ctx, span := trace.StartSpan(ctx, "sync.validateSyncContributionAndProof")
	defer span.End()

	// To process the following it requires the recent blocks to be present in the database, so we'll skip
	// validating or processing aggregated attestations until fully synced.
	if s.cfg.InitialSync.Syncing() {
		return pubsub.ValidationIgnore
	}

	raw, err := s.decodePubsubMessage(msg)
	if err != nil {
		log.WithError(err).Debug("Could not decode message")
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}
	m, ok := raw.(*prysmv2.SignedContributionAndProof)
	if !ok {
		return pubsub.ValidationReject
	}
	if m.Message == nil {
		return pubsub.ValidationReject
	}
	if err := altair.ValidateNilSyncContribution(m); err != nil {
		return pubsub.ValidationReject
	}

	// Broadcast the aggregated attestation on a feed to notify other services in the beacon node
	// of a received aggregated attestation.
	s.cfg.OperationNotifier.OperationFeed().Send(&feed.Event{
		Type: operation.SyncContributionReceived,
		Data: &operation.SyncContributionReceivedData{
			Contribution: m.Message,
		},
	})

	if err := helpers.VerifySlotTime(uint64(s.cfg.Chain.GenesisTime().Unix()), m.Message.Contribution.Slot, params.BeaconNetworkConfig().MaximumGossipClockDisparity); err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	if !s.hasBlockAndState(ctx, bytesutil.ToBytes32(m.Message.Contribution.BlockRoot)) {
		return pubsub.ValidationIgnore
	}
	if m.Message.Contribution.SubcommitteeIndex >= params.BeaconConfig().SyncCommitteeSubnetCount {
		return pubsub.ValidationReject
	}

	if s.hasSeenSyncContributionIndexSlot(m.Message.Contribution.Slot, m.Message.AggregatorIndex, types.CommitteeIndex(m.Message.Contribution.SubcommitteeIndex)) {
		return pubsub.ValidationIgnore
	}
	if !altair.IsSyncCommitteeAggregator(m.Message.SelectionProof) {
		return pubsub.ValidationReject
	}
	// This could be better, retrieving the state multiple times with copies can
	// easily lead to higher resource consumption by the node.
	blkState, err := s.cfg.StateGen.StateByRoot(ctx, bytesutil.ToBytes32(m.Message.Contribution.BlockRoot))
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	bState, ok := blkState.(iface.BeaconStateAltair)
	if !ok || bState.Version() != version.Altair {
		log.Errorf("Sync contribution referencing non-altair state")
		return pubsub.ValidationReject
	}
	syncPubkeys, err := altair.SyncSubCommitteePubkeys(bState, types.CommitteeIndex(m.Message.Contribution.SubcommitteeIndex))
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	aggregator, err := bState.ValidatorAtIndexReadOnly(m.Message.AggregatorIndex)
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	aggPubkey := aggregator.PublicKey()
	keyIsValid := false
	for _, pk := range syncPubkeys {
		if bytes.Equal(pk, aggPubkey[:]) {
			keyIsValid = true
			break
		}
	}
	if !keyIsValid {
		return pubsub.ValidationReject
	}
	if err := altair.VerifySyncSelectionData(bState, m.Message); err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}
	d, err := helpers.Domain(bState.Fork(), helpers.SlotToEpoch(bState.Slot()), params.BeaconConfig().DomainContributionAndProof, bState.GenesisValidatorRoot())
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	if err := helpers.VerifySigningRoot(m.Message, aggPubkey[:], m.Signature, d); err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}
	activePubkeys := []bls.PublicKey{}
	bVector := m.Message.Contribution.AggregationBits

	for i, pk := range syncPubkeys {
		if bVector.BitAt(uint64(i)) {
			pubK, err := bls.PublicKeyFromBytes(pk)
			if err != nil {
				traceutil.AnnotateError(span, err)
				return pubsub.ValidationIgnore
			}
			activePubkeys = append(activePubkeys, pubK)
		}
	}
	sig, err := bls.SignatureFromBytes(m.Message.Contribution.Signature)
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}
	d, err = helpers.Domain(bState.Fork(), helpers.SlotToEpoch(bState.Slot()), params.BeaconConfig().DomainSyncCommittee, bState.GenesisValidatorRoot())
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	rawBytes := p2ptypes.SSZBytes(m.Message.Contribution.BlockRoot)
	sigRoot, err := helpers.ComputeSigningRoot(&rawBytes, d)
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	verified := sig.Eth2FastAggregateVerify(activePubkeys, sigRoot)
	if !verified {
		return pubsub.ValidationReject
	}

	s.setSyncContributionIndexSlotSeen(m.Message.Contribution.Slot, m.Message.AggregatorIndex, types.CommitteeIndex(m.Message.Contribution.SubcommitteeIndex))

	msg.ValidatorData = m

	return pubsub.ValidationAccept
}

// Returns true if the node has received sync contribution for the aggregator with index,slot and subcommittee index.
func (s *Service) hasSeenSyncContributionIndexSlot(slot types.Slot, aggregatorIndex types.ValidatorIndex, subComIdx types.CommitteeIndex) bool {
	s.seenSyncContributionLock.RLock()
	defer s.seenSyncContributionLock.RUnlock()

	b := append(bytesutil.Bytes32(uint64(aggregatorIndex)), bytesutil.Bytes32(uint64(slot))...)
	b = append(b, bytesutil.Bytes32(uint64(subComIdx))...)
	_, seen := s.seenSyncContributionCache.Get(string(b))
	return seen
}

// Set sync contributor's aggregate index, slot and subcommittee index as seen.
func (s *Service) setSyncContributionIndexSlotSeen(slot types.Slot, aggregatorIndex types.ValidatorIndex, subComIdx types.CommitteeIndex) {
	s.seenSyncContributionLock.Lock()
	defer s.seenSyncContributionLock.Unlock()
	b := append(bytesutil.Bytes32(uint64(aggregatorIndex)), bytesutil.Bytes32(uint64(slot))...)
	b = append(b, bytesutil.Bytes32(uint64(subComIdx))...)
	s.seenSyncContributionCache.Add(string(b), true)
}