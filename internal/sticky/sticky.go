// Package sticky provides the overcomplicated Java sticky partitioning
// strategy for Kafka, with modifications made to be stickier and fairer.
//
// For some points on how Java's strategy is flawed, see
// https://github.com/Shopify/sarama/pull/1416/files/b29086bdaae0da7ce71eae3f854d50685fd6b631#r315005878
package sticky

import (
	"fmt"
	"reflect"
	"sort"

	"github.com/google/btree"

	"github.com/twmb/kgo/kmsg"
)

// Sticky partitioning has two versions, the latter from KIP-341 preventing a
// bug. The second version introduced generations with the default generation
// from the first generation's consumers defaulting to -1.

const defaultGeneration = -1

type GroupMember struct {
	ID string

	Version  int16
	Topics   []string
	UserData []byte
}

type Plan map[string]map[string][]int32

type balancer struct {
	// members are the members in play for this balance.
	//
	// This is built in newBalancer mapping member IDs to the GroupMember.
	members map[string]GroupMember

	// topics are the topic names and partitions that the client knows of
	// and passed to be used for balancing.
	//
	// This is repeatedly used for filtering topics that members indicate
	// they can consume but that our client does not know of.
	topics map[string][]int32

	// plan is the plan that we are building to balance partitions.
	//
	// This is initialized with data from the userdata each group member
	// is sending with the join. After, we use this to move partitions
	// around or assign new partitions.
	plan membersPartitions

	// planByNumPartitions orders plan member partitions by the number of
	// partitions each member is consuming.
	//
	// The nodes in the btree reference values in plan, meaning updates in
	// this field are visible in plan.
	planByNumPartitions *btree.BTree

	// isFreshAssignment tracks whether this is the first join for a group.
	// This is true if no member has userdata (plan is empty)
	isFreshAssignment bool
	// areSubscriptionsIdentical tracks if every member can consume the
	// same partitions. If true, this makes the isBalanced check much
	// simpler.
	areSubscriptionsIdentical bool

	// partitionConsumers maps all possible partitions to consume to the
	// members that are consuming them.
	//
	// We initialize this from our plan and modify it during reassignment.
	// We use this to know what member we are stealing partitions from.
	partitionConsumers map[topicPartition]string

	// consumers2AllPotentialPartitions maps each member to all of the
	// partitions it theoretically could consume. This is repeatedly used
	// during assignment to see if a partition we want to move can be moved
	// to a member.
	//
	// (maps each partition => each member that could consume it)
	//
	// This is built once and never modified thereafter.
	consumers2AllPotentialPartitions staticMembersPartitions

	// partitions2AllPotentialConsumers maps each partition to a member
	// that could theoretically consume it. This is repeatedly used during
	// assignment to see which members could consume a partition we want to
	// move.
	//
	// (maps each member => each partition it could consume)
	//
	// This is built once and never modified thereafter.
	partitions2AllPotentialConsumers staticPartitionMembers
}

type topicPartition struct {
	topic     string
	partition int32
}

func newBalancer(members []GroupMember, topics map[string][]int32) *balancer {
	b := &balancer{
		members: make(map[string]GroupMember, len(members)),
		topics:  topics,

		plan: make(membersPartitions),

		partitionConsumers: make(map[topicPartition]string),

		partitions2AllPotentialConsumers: make(staticPartitionMembers),
		consumers2AllPotentialPartitions: make(staticMembersPartitions),
	}
	for _, member := range members {
		b.members[member.ID] = member
	}
	return b
}

func (b *balancer) into() Plan {
	plan := make(Plan)
	for member, partitions := range b.plan {
		topics, exists := plan[member]
		if !exists {
			topics = make(map[string][]int32)
			plan[member] = topics
		}
		for _, partition := range *partitions {
			topics[partition.topic] = append(topics[partition.topic], partition.partition)
		}
	}
	return plan
}

// staticMembersPartitions is like membersPartitions below, but is used only
// for consumers2AllPotentialPartitions. The value is built once and never
// changed. Essentially, this is a clearer type.
type staticMembersPartitions map[string]map[topicPartition]struct{}

// membersPartitions maps members to a pointer of their partitions.  We use a
// pointer so that modifications through memberWithPartitions can be seen in
// any membersPartitions map.
type membersPartitions map[string]*[]topicPartition

// memberWithPartitions ties a member to a pointer to its partitions.
//
// This is generally used for sorting purposes.
type memberWithPartitions struct {
	member     string
	partitions *[]topicPartition
}

func (l memberWithPartitions) less(r memberWithPartitions) bool {
	return len(*l.partitions) < len(*r.partitions) ||
		len(*l.partitions) == len(*r.partitions) &&
			l.member < r.member
}

func (l memberWithPartitions) Less(r btree.Item) bool {
	return l.less(r.(memberWithPartitions))
}

func (m membersPartitions) intoConsumersPartitions() []memberWithPartitions {
	var consumersPartitions []memberWithPartitions
	for member, partitions := range m {
		consumersPartitions = append(consumersPartitions, memberWithPartitions{
			member,
			partitions,
		})
	}
	return consumersPartitions
}

func (m membersPartitions) btreeByConsumersPartitions() *btree.BTree {
	bt := btree.New(8)
	for _, memberWithPartitions := range m.intoConsumersPartitions() {
		bt.ReplaceOrInsert(memberWithPartitions)
	}
	return bt
}

func (mps membersPartitions) deepClone() membersPartitions {
	clone := make(membersPartitions, len(mps))
	for member, partitions := range mps {
		dup := append([]topicPartition(nil), *partitions...)
		clone[member] = &dup
	}
	return clone
}

// staticPartitionMember is the same as partitionMembers, but we type name it
// to imply immutability in reading. All mutable uses go through cloneKeys
// or shallowClone.
type staticPartitionMembers map[topicPartition]map[string]struct{}

func (orig staticPartitionMembers) cloneKeys() map[topicPartition]struct{} {
	dup := make(map[topicPartition]struct{}, len(orig))
	for partition := range orig {
		dup[partition] = struct{}{}
	}
	return dup
}

func Balance(members []GroupMember, topics map[string][]int32) Plan {
	// Code below relies on members to be sorted. It should be: that is the
	// contract of the Balance interface. But, just in case.
	sort.Slice(members, func(i, j int) bool { return members[i].ID < members[j].ID })

	b := newBalancer(members, topics)

	// Parse the member metadata for figure out what everybody was doing.
	b.parseMemberMetadata()
	b.initAllConsumersPartitions()
	// For planByNumPartitions, we use a btree heap since we will be
	// accessing both the min and max often as well as ranging from
	// smallest to largest.
	//
	// We init this after initAllConsumersPartitions, which can add new
	// members that were not in the prior plan.
	b.planByNumPartitions = b.plan.btreeByConsumersPartitions()
	b.assignUnassignedPartitions()

	b.balance()

	return b.into()
}

func strsHas(search []string, needle string) bool {
	for _, check := range search {
		if check == needle {
			return true
		}
	}
	return false
}

// parseMemberMetadata parses all member userdata to initialize the prior plan.
func (b *balancer) parseMemberMetadata() {
	type memberGeneration struct {
		member     string
		generation int32
	}

	// all partitions => members that are consuming those partitions
	// Each partition should only have one consumer, but a flaky member
	// could rejoin with an old generation (stale user data) and say it
	// is consuming something a different member is. See KIP-341.
	partitionConsumersByGeneration := make(map[topicPartition][]memberGeneration)

	for _, member := range b.members {
		memberPlan, generation := deserializeUserData(member.Version, member.UserData)
		memberGeneration := memberGeneration{
			member.ID,
			generation,
		}
		fmt.Println("deserialized", memberPlan, generation)
		for _, topicPartition := range memberPlan {
			partitionConsumers := partitionConsumersByGeneration[topicPartition]
			var doublyConsumed bool
			for _, otherConsumer := range partitionConsumers { // expected to be very few if any others
				if otherConsumer.generation == generation {
					doublyConsumed = true
					break
				}
			}
			// Two members should not be consuming the same topic and partition
			// within the same generation. If see this, we drop the second.
			if doublyConsumed {
				continue
			}
			partitionConsumers = append(partitionConsumers, memberGeneration)
			partitionConsumersByGeneration[topicPartition] = partitionConsumers
		}
	}

	for partition, partitionConsumers := range partitionConsumersByGeneration {
		sort.Slice(partitionConsumers, func(i, j int) bool {
			return partitionConsumers[i].generation > partitionConsumers[j].generation
		})

		member := partitionConsumers[0].member
		memberPartitions := b.plan[member]
		if memberPartitions == nil {
			memberPartitions = new([]topicPartition)
			b.plan[member] = memberPartitions
		}
		*memberPartitions = append(*memberPartitions, partition)
	}

	b.isFreshAssignment = len(b.plan) == 0
}

// deserializeUserData returns the topic partitions a member was consuming and
// the join generation it was consuming from.
//
// If anything fails or we do not understand the userdata parsing generation,
// we return empty defaults. The member will just be assumed to have no
// history.
func deserializeUserData(version int16, userdata []byte) (memberPlan []topicPartition, generation int32) {
	generation = defaultGeneration
	switch version {
	case 0:
		var v0 kmsg.StickyMemberMetadataV0
		if err := v0.ReadFrom(userdata); err != nil {
			return nil, 0
		}
		for _, topicAssignment := range v0.CurrentAssignment {
			for _, partition := range topicAssignment.Partitions {
				memberPlan = append(memberPlan, topicPartition{
					topicAssignment.Topic,
					partition,
				})
			}
		}
	case 1:
		var v1 kmsg.StickyMemberMetadataV1
		if err := v1.ReadFrom(userdata); err != nil {
			return nil, 0
		}
		generation = v1.Generation
		for _, topicAssignment := range v1.CurrentAssignment {
			for _, partition := range topicAssignment.Partitions {
				memberPlan = append(memberPlan, topicPartition{
					topicAssignment.Topic,
					partition,
				})
			}
		}
	}

	return memberPlan, generation
}

// initAllConsumersPartitions initializes the two "2All" fields in our
// balancer.
//
// Note that the Java code puts topic partitions that no member is interested
// in into partitions2AllPotentialConsumers. This provides no benefit to any
// part of our balancing and, at worse, could change our partitions by move
// preference unnecessarily.
func (b *balancer) initAllConsumersPartitions() {
	for _, member := range b.members {
		for _, topic := range member.Topics {
			partitions, exists := b.topics[topic]
			if !exists {
				continue
			}
			for _, partition := range partitions {
				consumerPotentialPartitions := b.consumers2AllPotentialPartitions[member.ID]
				if consumerPotentialPartitions == nil {
					consumerPotentialPartitions = make(map[topicPartition]struct{})
					b.consumers2AllPotentialPartitions[member.ID] = consumerPotentialPartitions
				}

				topicPartition := topicPartition{topic, partition}
				partitionPotentialConsumers := b.partitions2AllPotentialConsumers[topicPartition]
				if partitionPotentialConsumers == nil {
					partitionPotentialConsumers = make(map[string]struct{})
					b.partitions2AllPotentialConsumers[topicPartition] = partitionPotentialConsumers
				}

				consumerPotentialPartitions[topicPartition] = struct{}{}
				partitionPotentialConsumers[member.ID] = struct{}{}
			}
		}
		// Lastly, if this is a new member, the plan everything is
		// using will not know of it. We add that it is consuming nothing
		// in that plan here.
		if _, exists := b.plan[member.ID]; !exists {
			b.plan[member.ID] = new([]topicPartition)
		}
	}

	b.setIfMemberSubscriptionsIdentical()
}

// Determines whether each member can consume the same partitions.
//
// The Java code also checks consumers2, but it also stuffs partitions that no
// members can consume into partitions2, which returns false unnecessarily.
// With our code, the maps should be reverse identical.
func (b *balancer) setIfMemberSubscriptionsIdentical() {
	var firstMembers map[string]struct{}
	var firstSet bool
	for _, members := range b.partitions2AllPotentialConsumers {
		if !firstSet {
			firstMembers = members
			firstSet = true
			continue
		}
		if !reflect.DeepEqual(members, firstMembers) {
			return
		}
	}
	b.areSubscriptionsIdentical = true
}

// assignUnassignedPartitions does what the name says.
//
// Partitions that a member was consuming but is no longer interested in, as
// well as new partitions that nobody was consuming, are unassigned.
func (b *balancer) assignUnassignedPartitions() {
	// To build a list of unassigned partitions, we visit all partitions
	// in the current plan and, if they still exist and the prior consumer
	// no longer wants to consume them, we track it as unassigned.
	// After, we add all new partitions.
	unvisitedPartitions := b.partitions2AllPotentialConsumers.cloneKeys()

	var unassignedPartitions []topicPartition
	for member, partitions := range b.plan {
		var keepIdx int
		for _, partition := range *partitions {
			// If this partition no longer exists at all, likely due to the
			// topic being deleted, we remove the partition from the member.
			if _, exists := b.partitions2AllPotentialConsumers[partition]; !exists {
				continue
			}

			delete(unvisitedPartitions, partition)
			b.partitionConsumers[partition] = member

			if !strsHas(b.members[member].Topics, partition.topic) {
				unassignedPartitions = append(unassignedPartitions, partition)
				continue
			}

			(*partitions)[keepIdx] = partition
			keepIdx++
		}
		*partitions = (*partitions)[:keepIdx]
	}
	for unvisited := range unvisitedPartitions {
		unassignedPartitions = append(unassignedPartitions, unvisited)
	}

	// With our list of unassigned partitions, if the partition can be
	// assigned, we assign it to the least loaded member.
	for _, partition := range unassignedPartitions {
		if _, exists := b.partitions2AllPotentialConsumers[partition]; !exists {
			continue
		}
		b.assignPartition(partition)
	}
}

func (b *balancer) balance() {
	// Make two copies of our current plan: one for the balance score
	// calculation later, and one for easy steal lookup in reassigning.
	preBalancePlan := b.plan.deepClone()
	startingPlan := make(map[string]map[topicPartition]struct{}, len(preBalancePlan))
	for member, partitions := range preBalancePlan {
		memberPartitions := make(map[topicPartition]struct{}, len(*partitions))
		for _, partition := range *partitions {
			memberPartitions[partition] = struct{}{}
		}
		startingPlan[member] = memberPartitions
	}

	didReassign := b.doReassigning(startingPlan)

	if !b.isFreshAssignment && didReassign && calcBalanceScore(b.plan) >= calcBalanceScore(preBalancePlan) {
		fmt.Printf("resetting plan, score sux, before: %d, after %d\n",
			calcBalanceScore(preBalancePlan),
			calcBalanceScore(b.plan))
		b.plan = preBalancePlan
	}
}

// calcBalanceScore calculates how balanced a plan is by summing deltas of how
// many partitions each member is consuming. The lower the aggregate delta, the
// beter.
func calcBalanceScore(plan membersPartitions) int {
	absDelta := func(l, r int) int {
		v := l - r
		if v < 0 {
			return -v
		}
		return v
	}

	var score int
	memberSizes := make(map[string]int, len(plan))
	for member, partitions := range plan {
		memberSizes[member] = len(*partitions)
	}

	// Sums a triangle of size deltas.
	for member, size := range memberSizes {
		delete(memberSizes, member)
		for _, otherSize := range memberSizes {
			score += absDelta(size, otherSize)
		}
	}
	return score
}

// assignPartition looks for the first member that can assume this unassigned
// partition, in order from members with smallest partitions, and assigns
// the partition to it.
func (b *balancer) assignPartition(unassigned topicPartition) {
	b.planByNumPartitions.Ascend(func(item btree.Item) bool {
		memberWithFewestPartitions := item.(memberWithPartitions)
		member := memberWithFewestPartitions.member
		memberPotentials := b.consumers2AllPotentialPartitions[member]
		if _, memberCanUse := memberPotentials[unassigned]; !memberCanUse {
			return true
		}

		// Before we change the sort order, delete this item from our
		// btree. If we edo this after changing the order, the tree
		// will not be able to delete the item.
		b.planByNumPartitions.Delete(item)

		partitions := memberWithFewestPartitions.partitions
		*partitions = append(*partitions, unassigned)
		// Add the item back to its new sorted position.
		b.planByNumPartitions.ReplaceOrInsert(memberWithFewestPartitions)

		b.partitionConsumers[unassigned] = member
		return false
	})
}

func (b *balancer) isBalanced() bool {
	// The plan could be empty if no member is subscribing to anything the
	// client has or if all members are fixed.
	if len(b.plan) == 0 {
		return true
	}
	minConsumer := b.planByNumPartitions.Min().(memberWithPartitions)
	maxConsumer := b.planByNumPartitions.Max().(memberWithPartitions)
	// If the delta between the min and the max consumer's partition's
	// is 0 or 1, we are balanced.
	if len(*minConsumer.partitions) >= len(*maxConsumer.partitions)-1 {
		return true
	}
	// An optimization not in the Java code: if we know all subscriptions
	// are identical, then if the partition delta is more than one, we know
	// that we are not balanced.
	if b.areSubscriptionsIdentical {
		return false
	}

	// Across all members, across the partitions a member could have, if
	// any of those partitions are on a member that has _more_ partitions,
	// then this is not balanced.
	//
	// Note that we check one more case than the Java code, but it is
	// not detrimental.
	balanced := true
	b.planByNumPartitions.Ascend(func(item btree.Item) bool {
		current := item.(memberWithPartitions)
		currentMember := current.member
		currentPartitions := *current.partitions

		possiblePartitions := b.consumers2AllPotentialPartitions[currentMember]
		maxPartitions := len(possiblePartitions)

		if len(currentPartitions) == maxPartitions {
			return true
		}

		for possiblePartition := range possiblePartitions {
			otherMember := b.partitionConsumers[possiblePartition]
			if otherMember == currentMember {
				continue
			}

			otherPartitions := *b.plan[otherMember]

			if len(currentPartitions) < len(otherPartitions) {
				balanced = false
				return false
			}
		}
		return true
	})
	return balanced
}

// doReassigning loops trying to move partitions until the plan is balanced
// or until no moves happen.
//
// This loops over all partitions, each time seeing if each partition has a
// better place to be.
//
// cyclers is how we prevent partition stealing cycles.
//
// Say we have 5 members, A B C D E.
//
// A consumes 1 2 3 4 5
// B consumes 1 2 3 4 5
// C consumes 1 2 3 4 5
// D consumes 7 8 9 a b c
// E consumes 7 8 9 a b c
//
// D and E exist to ensure that isBalanced returns false.
//
// If the setup is
// A -> 1 2
// B -> 3 4
// C -> 5
// ... (D and E do not matter)
//
// Then we have a steal cycle: none of A, B, nor C will be happy since they
// will all think they can steal one more from the other two.
//
// If a partition CAN get stolen around a set of members, then those members
// MUST be the members with the fewest partitions, and they MUST have at most
// one partition difference between them.
//
// The reason for this is that cycles can only form by the least consuming
// member stealing a single partition from another member that then itself
// becomes the least consuming member. Thus, at most one difference.
//
// If a partition can go in a cycle, we freeze the member it ends on, which is
// the one it started on.
//
// It is expected that cycles will be of length two due to logic below about
// stealing back partitions. If C steals 5 from B, B will next see 5 as a
// candidate to steal, know that it had 5 prior, and steal it back. B freezes,
// the same process happens between C and A, and A freezes. This preserves
// stickiness.
func (b *balancer) doReassigning(startingPlan map[string]map[topicPartition]struct{}) (didReassign bool) {
	cyclers := make(map[topicPartition]map[string]struct{})
	frozenMembers := make(map[string]struct{})
	modified := true
	fmt.Println("before reassign:")
	for member, partitions := range b.plan {
		fmt.Printf("%s => %v\n", member, *partitions)
	}
	fmt.Println("reassigning!")
	for modified {
		if b.isBalanced() {
			fmt.Println("is balanced! quitting.")
			return
		}
		modified = false

		b.planByNumPartitions.Ascend(func(item btree.Item) bool {
			leastLoaded := item.(memberWithPartitions)
			myMember := leastLoaded.member
			fmt.Println("ascending on", myMember)
			if _, frozen := frozenMembers[myMember]; frozen {
				fmt.Println("frozen! continuing...")
				return true
			}
			myPartitions := *leastLoaded.partitions

			type stealCandidate struct {
				member    string
				partition topicPartition
			}
			var stealCandidates []stealCandidate
			var mostOtherPartitions int
			for partition := range b.consumers2AllPotentialPartitions[myMember] {
				otherMember := b.partitionConsumers[partition]
				if otherMember == leastLoaded.member {
					continue
				}
				if _, frozen := frozenMembers[otherMember]; frozen {
					continue
				}

				otherPartitions := *b.plan[otherMember]
				if len(myPartitions) < len(otherPartitions) &&
					len(otherPartitions) >= mostOtherPartitions {
					if mostOtherPartitions > 0 &&
						mostOtherPartitions < len(otherPartitions) {
						fmt.Println("resetting steal candidates, found member with higher partitions", len(otherPartitions))
						stealCandidates = stealCandidates[:0]
					}
					mostOtherPartitions = len(otherPartitions)
					fmt.Printf("found candidate with %d partitions to steal from %s: %v\n", mostOtherPartitions, otherMember, partition)
					stealCandidates = append(stealCandidates, stealCandidate{
						otherMember,
						partition,
					})
				}
			}

			if len(stealCandidates) == 0 {
				// If we did not find a partition to steal, we freeze
				// this member to prevent it from consideration for
				// future loops. Nothing should steal from us since we
				// are the least loaded member.
				frozenMembers[myMember] = struct{}{}
				fmt.Println("no steal candidates! freezing...")
				return true
			}

			steal := stealCandidates[0]
			myStartingPartitions := startingPlan[myMember]
			for _, candidate := range stealCandidates {
				if _, hadPrior := myStartingPartitions[candidate.partition]; hadPrior {
					steal = candidate
					break
				}
			}

			fmt.Printf("%s: stealing t %s p %d from %s\n", myMember, steal.partition.topic, steal.partition.partition, steal.member)

			cycle := cyclers[steal.partition]
			if cycle == nil {
				cycle = make(map[string]struct{})
				cyclers[steal.partition] = cycle
			} else if _, exists := cycle[myMember]; exists {
				fmt.Printf("freezing %s\n", myMember)
				frozenMembers[myMember] = struct{}{}
			}
			cycle[myMember] = struct{}{}
			cycle[steal.member] = struct{}{}

			b.reassignPartition(steal.partition, steal.member, myMember)
			didReassign = true
			modified = true
			return false

		})
	}
	return didReassign
}

// reassignPartition reassigns a partition from srcMember to dstMember, potentially
// undoing a prior move if this detects a partition when there-and-back.
func (b *balancer) reassignPartition(partition topicPartition, srcMember, dstMember string) {
	oldPartitions := b.plan[srcMember]
	newPartitions := b.plan[dstMember]

	// Remove the elements from our btree before we change the sort order.
	b.planByNumPartitions.Delete(memberWithPartitions{
		srcMember,
		oldPartitions,
	})
	b.planByNumPartitions.Delete(memberWithPartitions{
		dstMember,
		newPartitions,
	})

	for idx, oldPartition := range *oldPartitions { // remove from old member
		if oldPartition == partition {
			(*oldPartitions)[idx] = (*oldPartitions)[len(*oldPartitions)-1]
			*oldPartitions = (*oldPartitions)[:len(*oldPartitions)-1]
			break
		}
	}
	*newPartitions = append(*newPartitions, partition) // add to new

	fmt.Println("reassign results")
	fmt.Printf("%s => %v\n", srcMember, *oldPartitions)
	fmt.Printf("%s => %v\n", dstMember, *newPartitions)

	// Now add back the changed elements to our btree.
	b.planByNumPartitions.ReplaceOrInsert(memberWithPartitions{
		srcMember,
		oldPartitions,
	})
	b.planByNumPartitions.ReplaceOrInsert(memberWithPartitions{
		dstMember,
		newPartitions,
	})

	// Finally, update which member is consuming the partition.
	b.partitionConsumers[partition] = dstMember
}
