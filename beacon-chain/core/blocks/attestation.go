package blocks

import (
	"bytes"
	"context"
	"fmt"

	"github.com/pkg/errors"
	types "github.com/prysmaticlabs/eth2-types"
	ethpb "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/epoch"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	stateTrie "github.com/prysmaticlabs/prysm/beacon-chain/state"
	"github.com/prysmaticlabs/prysm/shared/attestationutil"
	"github.com/prysmaticlabs/prysm/shared/bls"
	"github.com/prysmaticlabs/prysm/shared/mathutil"
	"github.com/prysmaticlabs/prysm/shared/params"
	"go.opencensus.io/trace"
)

// ProcessAttestations applies processing operations to a block's inner attestation
// records.
func ProcessAttestations(
	ctx context.Context,
	beaconState *stateTrie.BeaconState,
	b *ethpb.SignedBeaconBlock,
) (*stateTrie.BeaconState, error) {
	if err := helpers.VerifyNilBeaconBlock(b); err != nil {
		return nil, err
	}

	var err error
	for idx, attestation := range b.Block.Body.Attestations {
		beaconState, err = ProcessAttestation(ctx, beaconState, attestation)
		if err != nil {
			return nil, errors.Wrapf(err, "could not verify attestation at index %d in block", idx)
		}
	}
	return beaconState, nil
}

// ProcessAttestation verifies an input attestation can pass through processing using the given beacon state.
//
// Spec pseudocode definition:
//  def process_attestation(state: BeaconState, attestation: Attestation) -> None:
//    data = attestation.data
//    assert data.target.epoch in (get_previous_epoch(state), get_current_epoch(state))
//    assert data.target.epoch == compute_epoch_at_slot(data.slot)
//    assert data.slot + MIN_ATTESTATION_INCLUSION_DELAY <= state.slot <= data.slot + SLOTS_PER_EPOCH
//    assert data.index < get_committee_count_per_slot(state, data.target.epoch)
//
//    committee = get_beacon_committee(state, data.slot, data.index)
//    assert len(attestation.aggregation_bits) == len(committee)
//
//    if data.target.epoch == get_current_epoch(state):
//        epoch_participation = state.current_epoch_participation
//        justified_checkpoint = state.current_justified_checkpoint
//    else:
//        epoch_participation = state.previous_epoch_participation
//        justified_checkpoint = state.previous_justified_checkpoint
//
//    # Matching roots
//    is_matching_head = data.beacon_block_root == get_block_root_at_slot(state, data.slot)
//    is_matching_source = data.source == justified_checkpoint
//    is_matching_target = data.target.root == get_block_root(state, data.target.epoch)
//    assert is_matching_source
//
//    # Verify signature
//    assert is_valid_indexed_attestation(state, get_indexed_attestation(state, attestation))
//
//    # Participation flags
//    participation_flags = []
//    if is_matching_head and is_matching_target and state.slot <= data.slot + MIN_ATTESTATION_INCLUSION_DELAY:
//        participation_flags.append(TIMELY_HEAD_FLAG)
//    if is_matching_source and state.slot <= data.slot + integer_squareroot(SLOTS_PER_EPOCH):
//        participation_flags.append(TIMELY_SOURCE_FLAG)
//    if is_matching_target and state.slot <= data.slot + SLOTS_PER_EPOCH:
//        participation_flags.append(TIMELY_TARGET_FLAG)
//
//    # Update epoch participation flags
//    proposer_reward_numerator = 0
//    for index in get_attesting_indices(state, data, attestation.aggregation_bits):
//        for flag, numerator in get_flags_and_numerators():
//            if flag in participation_flags and not has_validator_flags(epoch_participation[index], flag):
//                epoch_participation[index] = add_validator_flags(epoch_participation[index], flag)
//                proposer_reward_numerator += get_base_reward(state, index) * numerator
//
//    # Reward proposer
//    proposer_reward = Gwei(proposer_reward_numerator // (REWARD_DENOMINATOR * PROPOSER_REWARD_QUOTIENT))
//    increase_balance(state, get_beacon_proposer_index(state), proposer_reward)
func ProcessAttestation(
	ctx context.Context,
	beaconState *stateTrie.BeaconState,
	att *ethpb.Attestation,
) (*stateTrie.BeaconState, error) {
	beaconState, err := ProcessAttestationNoVerifySignature(ctx, beaconState, att)
	if err != nil {
		return nil, err
	}
	return beaconState, VerifyAttestationSignature(ctx, beaconState, att)
}

// ProcessAttestationsNoVerifySignature applies processing operations to a block's inner attestation
// records. The only difference would be that the attestation signature would not be verified.
func ProcessAttestationsNoVerifySignature(
	ctx context.Context,
	beaconState *stateTrie.BeaconState,
	b *ethpb.SignedBeaconBlock,
) (*stateTrie.BeaconState, error) {
	if err := helpers.VerifyNilBeaconBlock(b); err != nil {
		return nil, err
	}
	body := b.Block.Body
	var err error
	for idx, attestation := range body.Attestations {
		beaconState, err = ProcessAttestationNoVerifySignature(ctx, beaconState, attestation)
		if err != nil {
			return nil, errors.Wrapf(err, "could not verify attestation at index %d in block", idx)
		}
	}
	return beaconState, nil
}

// ProcessAttestationNoVerifySignature processes the attestation without verifying the attestation signature. This
// method is used to validate attestations whose signatures have already been verified.
func ProcessAttestationNoVerifySignature(
	ctx context.Context,
	beaconState *stateTrie.BeaconState,
	att *ethpb.Attestation,
) (*stateTrie.BeaconState, error) {
	ctx, span := trace.StartSpan(ctx, "core.ProcessAttestationNoVerifySignature")
	defer span.End()

	if err := helpers.ValidateNilAttestation(att); err != nil {
		return nil, err
	}
	currEpoch := helpers.CurrentEpoch(beaconState)
	prevEpoch := helpers.PrevEpoch(beaconState)
	data := att.Data
	if data.Target.Epoch != prevEpoch && data.Target.Epoch != currEpoch {
		return nil, fmt.Errorf(
			"expected target epoch (%d) to be the previous epoch (%d) or the current epoch (%d)",
			data.Target.Epoch,
			prevEpoch,
			currEpoch,
		)
	}
	if err := helpers.ValidateSlotTargetEpoch(att.Data); err != nil {
		return nil, err
	}

	s := att.Data.Slot
	minInclusionCheck := s+params.BeaconConfig().MinAttestationInclusionDelay <= beaconState.Slot()
	if !minInclusionCheck {
		return nil, fmt.Errorf(
			"attestation slot %d + inclusion delay %d > state slot %d",
			s,
			params.BeaconConfig().MinAttestationInclusionDelay,
			beaconState.Slot(),
		)
	}
	epochInclusionCheck := beaconState.Slot() <= s+params.BeaconConfig().SlotsPerEpoch
	if !epochInclusionCheck {
		return nil, fmt.Errorf(
			"state slot %d > attestation slot %d + SLOTS_PER_EPOCH %d",
			beaconState.Slot(),
			s,
			params.BeaconConfig().SlotsPerEpoch,
		)
	}
	activeValidatorCount, err := helpers.ActiveValidatorCount(beaconState, att.Data.Target.Epoch)
	if err != nil {
		return nil, err
	}
	c := helpers.SlotCommitteeCount(activeValidatorCount)
	if uint64(att.Data.CommitteeIndex) >= c {
		return nil, fmt.Errorf("committee index %d >= committee count %d", att.Data.CommitteeIndex, c)
	}
	if err := helpers.VerifyAttestationBitfieldLengths(beaconState, att); err != nil {
		return nil, errors.Wrap(err, "could not verify attestation bitfields")
	}

	// Verify attesting indices are correct.
	committee, err := helpers.BeaconCommitteeFromState(beaconState, att.Data.Slot, att.Data.CommitteeIndex)
	if err != nil {
		return nil, err
	}
	indexedAtt, err := attestationutil.ConvertToIndexed(ctx, att, committee)
	if err != nil {
		return nil, err
	}
	if err := attestationutil.IsValidAttestationIndices(ctx, indexedAtt); err != nil {
		return nil, err
	}

	var justifiedCheckpt *ethpb.Checkpoint
	var ffgTargetEpoch types.Epoch
	var epochParticipation []byte
	if data.Target.Epoch == currEpoch {
		justifiedCheckpt = beaconState.CurrentJustifiedCheckpoint()
		ffgTargetEpoch = currEpoch
		epochParticipation = beaconState.CurrentEpochParticipation()
	} else {
		justifiedCheckpt = beaconState.PreviousJustifiedCheckpoint()
		ffgTargetEpoch = prevEpoch
		epochParticipation = beaconState.PreviousEpochParticipation()
	}
	if data.Target.Epoch != ffgTargetEpoch {
		return nil, fmt.Errorf("expected target epoch %d, received %d", ffgTargetEpoch, data.Target.Epoch)
	}
	matchingSource := attestationutil.CheckPointIsEqual(data.Source, justifiedCheckpt)
	if !matchingSource {
		return nil, errors.New("source epoch does not match")
	}
	r, err := helpers.BlockRoot(beaconState, data.Target.Epoch)
	if err != nil {
		return nil, err
	}
	matchingTarget := bytes.Equal(r, data.Target.Root)
	r, err = helpers.BlockRootAtSlot(beaconState, data.Slot)
	if err != nil {
		return nil, err
	}
	matchingHead := bytes.Equal(r, data.BeaconBlockRoot)

	// Process participation flags.
	participatedFlags := make(map[uint8]bool)
	headFlag := params.BeaconConfig().TimelyHeadFlag
	sourceFlag := params.BeaconConfig().TimelySourceFlag
	targetFlag := params.BeaconConfig().TimelyTargetFlag
	if matchingHead && matchingTarget && beaconState.Slot() <= data.Slot+params.BeaconConfig().MinAttestationInclusionDelay {
		participatedFlags[headFlag] = true
	}
	if matchingSource && beaconState.Slot() <= data.Slot.Add(mathutil.IntegerSquareRoot(uint64(params.BeaconConfig().SlotsPerEpoch))) {
		participatedFlags[sourceFlag] = true
	}
	if matchingTarget && beaconState.Slot() <= data.Slot+params.BeaconConfig().SlotsPerEpoch {
		participatedFlags[targetFlag] = true
	}
	indices, err := attestationutil.AttestingIndices(att.AggregationBits, committee)
	if err != nil {
		return nil, err
	}
	proposerRewardNumerator := uint64(0)
	for _, index := range indices {
		br, err := epoch.BaseReward(beaconState, types.ValidatorIndex(index))
		if err != nil {
			return nil, err
		}
		if participatedFlags[headFlag] && !helpers.HasValidatorFlag(epochParticipation[index], headFlag) {
			epochParticipation[index] = helpers.AddValidatorFlag(epochParticipation[index], headFlag)
			proposerRewardNumerator += br * params.BeaconConfig().TimelyHeadNumerator
		}
		if participatedFlags[sourceFlag] && !helpers.HasValidatorFlag(epochParticipation[index], sourceFlag) {
			epochParticipation[index] = helpers.AddValidatorFlag(epochParticipation[index], sourceFlag)
			proposerRewardNumerator += br * params.BeaconConfig().TimelySourceNumerator
		}
		if participatedFlags[targetFlag] && !helpers.HasValidatorFlag(epochParticipation[index], targetFlag) {
			epochParticipation[index] = helpers.AddValidatorFlag(epochParticipation[index], targetFlag)
			proposerRewardNumerator += br * params.BeaconConfig().TimelyTargetNumerator
		}
	}

	if data.Target.Epoch == currEpoch {
		if err := beaconState.SetCurrentParticipationBits(epochParticipation); err != nil {
			return nil, err
		}
	} else {
		if err := beaconState.SetPreviousParticipationBits(epochParticipation); err != nil {
			return nil, err
		}
	}
	proposerReward := proposerRewardNumerator / (params.BeaconConfig().RewardDenominator * params.BeaconConfig().ProposerRewardQuotient)
	i, err := helpers.BeaconProposerIndex(beaconState)
	if err != nil {
		return nil, err
	}
	if err := helpers.IncreaseBalance(beaconState, i, proposerReward); err != nil {
		return nil, err
	}
	return beaconState, nil
}

// VerifyAttestationSignature converts and attestation into an indexed attestation and verifies
// the signature in that attestation.
func VerifyAttestationSignature(ctx context.Context, beaconState *stateTrie.BeaconState, att *ethpb.Attestation) error {
	if err := helpers.ValidateNilAttestation(att); err != nil {
		return err
	}
	committee, err := helpers.BeaconCommitteeFromState(beaconState, att.Data.Slot, att.Data.CommitteeIndex)
	if err != nil {
		return err
	}
	indexedAtt, err := attestationutil.ConvertToIndexed(ctx, att, committee)
	if err != nil {
		return err
	}
	return VerifyIndexedAttestation(ctx, beaconState, indexedAtt)
}

// VerifyIndexedAttestation determines the validity of an indexed attestation.
//
// Spec pseudocode definition:
//  def is_valid_indexed_attestation(state: BeaconState, indexed_attestation: IndexedAttestation) -> bool:
//    """
//    Check if ``indexed_attestation`` is not empty, has sorted and unique indices and has a valid aggregate signature.
//    """
//    # Verify indices are sorted and unique
//    indices = indexed_attestation.attesting_indices
//    if len(indices) == 0 or not indices == sorted(set(indices)):
//        return False
//    # Verify aggregate signature
//    pubkeys = [state.validators[i].pubkey for i in indices]
//    domain = get_domain(state, DOMAIN_BEACON_ATTESTER, indexed_attestation.data.target.epoch)
//    signing_root = compute_signing_root(indexed_attestation.data, domain)
//    return bls.FastAggregateVerify(pubkeys, signing_root, indexed_attestation.signature)
func VerifyIndexedAttestation(ctx context.Context, beaconState *stateTrie.BeaconState, indexedAtt *ethpb.IndexedAttestation) error {
	ctx, span := trace.StartSpan(ctx, "core.VerifyIndexedAttestation")
	defer span.End()

	if err := attestationutil.IsValidAttestationIndices(ctx, indexedAtt); err != nil {
		return err
	}
	domain, err := helpers.Domain(beaconState.Fork(), indexedAtt.Data.Target.Epoch, params.BeaconConfig().DomainBeaconAttester, beaconState.GenesisValidatorRoot())
	if err != nil {
		return err
	}
	indices := indexedAtt.AttestingIndices
	var pubkeys []bls.PublicKey
	for i := 0; i < len(indices); i++ {
		pubkeyAtIdx := beaconState.PubkeyAtIndex(types.ValidatorIndex(indices[i]))
		pk, err := bls.PublicKeyFromBytes(pubkeyAtIdx[:])
		if err != nil {
			return errors.Wrap(err, "could not deserialize validator public key")
		}
		pubkeys = append(pubkeys, pk)
	}
	return attestationutil.VerifyIndexedAttestationSig(ctx, indexedAtt, pubkeys, domain)
}
