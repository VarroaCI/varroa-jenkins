package recordstore

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
)

// Member roles. An active member holds one slot index. A standby holds
// the tenant folder, holds no slot, and is never a job's home.
const (
	MemberRoleActive  = "active"
	MemberRoleStandby = "standby"
)

// member is one entry of tenant_placements.members. It carries the wire
// shape the announce request sends, so the stored column and the request
// field are one type.
type member = TenantSyncMember

// placementMembers is one decoded tenant_placements row's member view: the
// member list and the job home map. Every reader of a member's identity
// resolves it through this type.
type placementMembers struct {
	members  []member
	jobHomes map[string]int32
}

// placementScalars are the row's scalar columns. A row whose members list
// is empty answers one synthetic slot-0 active member built from them, so
// a controller with no cell role needs no branch at any call site.
type placementScalars struct {
	cell               string
	namespace          string
	cellInstance       string
	rehydratedInstance string
	pendingJobs        []string
}

// decodePlacementMembers decodes the members and job_homes columns of one
// tenant_placements row. An empty members list answers one synthetic
// slot-0 active member built from scalars. An empty or absent job_homes
// map answers an empty map, which resolves every job path to its computed
// home slot.
func decodePlacementMembers(membersRaw, jobHomesRaw []byte, scalars placementScalars) (placementMembers, error) {
	out := placementMembers{jobHomes: map[string]int32{}}
	if len(membersRaw) > 0 {
		if err := json.Unmarshal(membersRaw, &out.members); err != nil {
			return placementMembers{}, fmt.Errorf("decode placement members: %w", err)
		}
	}
	if len(jobHomesRaw) > 0 {
		if err := json.Unmarshal(jobHomesRaw, &out.jobHomes); err != nil {
			return placementMembers{}, fmt.Errorf("decode job homes: %w", err)
		}
		if out.jobHomes == nil {
			out.jobHomes = map[string]int32{}
		}
	}
	return newPlacementMembers(out.members, out.jobHomes, scalars), nil
}

// newPlacementMembers builds a member view from a decoded list and a
// home map. An empty list answers one synthetic slot-0 active member
// built from scalars, which is the same answer the decoder gives. A
// caller that holds the values already, such as the announce handler,
// calls this instead of encoding and decoding them again.
func newPlacementMembers(members []member, jobHomes map[string]int32, scalars placementScalars) placementMembers {
	if jobHomes == nil {
		jobHomes = map[string]int32{}
	}
	if len(members) == 0 {
		members = []member{syntheticSlotZero(scalars)}
	}
	return placementMembers{members: members, jobHomes: jobHomes}
}

// syntheticSlotZero builds the slot-0 active member a row with an empty
// members list answers.
func syntheticSlotZero(scalars placementScalars) member {
	slot := int32(0)
	return member{
		Cell:               scalars.cell,
		Namespace:          scalars.namespace,
		Role:               MemberRoleActive,
		Slot:               &slot,
		Instance:           scalars.cellInstance,
		RehydratedInstance: scalars.rehydratedInstance,
		PendingJobs:        scalars.pendingJobs,
	}
}

// memberForJob answers the active member that holds jobPath's home slot.
// A slot that holds no member answers the zero member, whose empty cell
// and empty instance make every comparison fail closed. This helper never
// answers a standby.
func memberForJob(row placementMembers, jobPath string) member {
	slot := homeSlotForJob(row, jobPath)
	for _, m := range row.members {
		if m.Role != MemberRoleActive || m.Slot == nil {
			continue
		}
		if *m.Slot == slot {
			return m
		}
	}
	return member{}
}

// homeSlotForJob answers jobPath's home slot. A stored job_homes entry
// wins. A path with no entry resolves to its computed hash home over the
// row's active slot count, the same value the next recompute stores. The
// dispatcher, the claim fence, and the schedule gate must read one home
// from the moment a job exists, before any recompute stores its entry.
// An empty path is a tenant-wide call, such as a fleet lock, and resolves
// to slot 0.
func homeSlotForJob(row placementMembers, jobPath string) int32 {
	if slot, ok := row.jobHomes[jobPath]; ok {
		return slot
	}
	if jobPath == "" {
		return 0
	}
	return jobHomeSlot(jobPath, activeSlotCount(row.members))
}

// memberForInstance answers the member whose cell instance equals
// instance. An empty instance never matches.
func memberForInstance(row placementMembers, instance string) (member, bool) {
	if instance == "" {
		return member{}, false
	}
	for _, m := range row.members {
		if m.Instance == instance {
			return m, true
		}
	}
	return member{}, false
}

// memberForCell answers the member bound to the cell named by namespace
// and cell. The pair is a member's key, so the lookup is exact. An empty
// cell never matches.
func memberForCell(row placementMembers, namespace, cell string) (member, bool) {
	if cell == "" {
		return member{}, false
	}
	for _, m := range row.members {
		if m.Namespace == namespace && m.Cell == cell {
			return m, true
		}
	}
	return member{}, false
}

// renderMemberRole answers the rendered role a usage read reports:
// "slot <n>" for an active member and "standby" for a standby. An
// active member with no slot reads as slot 0, which is the synthetic
// member a row with an empty list answers.
func renderMemberRole(role string, slot *int32) string {
	if role == MemberRoleStandby {
		return MemberRoleStandby
	}
	n := int32(0)
	if slot != nil {
		n = *slot
	}
	return fmt.Sprintf("slot %d", n)
}

// hasMemberInNamespace reports whether any member of the row sits in
// namespace. The fence tests the namespace before it tests the cell, so
// it names the two answers apart.
func hasMemberInNamespace(row placementMembers, namespace string) bool {
	for _, m := range row.members {
		if m.Namespace == namespace {
			return true
		}
	}
	return false
}

// activeSlotCount answers the number of active slots the member list
// holds. It is the bucket count of the home hash. A list with no active
// member answers one, because slot 0 always exists.
func activeSlotCount(members []member) int32 {
	highest := int32(-1)
	for _, m := range members {
		if m.Role != MemberRoleActive || m.Slot == nil {
			continue
		}
		if *m.Slot > highest {
			highest = *m.Slot
		}
	}
	if highest < 0 {
		return 1
	}
	return highest + 1
}

// jobHomeSlot answers jobPath's home slot over buckets active slots. The
// key is the FNV-1a 64-bit hash of the job path.
func jobHomeSlot(jobPath string, buckets int32) int32 {
	if buckets <= 1 {
		return 0
	}
	h := fnv.New64a()
	// Write on a hash never answers an error.
	_, _ = h.Write([]byte(jobPath))
	return jumpHash(h.Sum64(), buckets)
}

// jumpHash is the Lamping and Veach jump consistent hash. It answers a
// bucket in [0, buckets).
//
// The algorithm has the property this design needs. When the bucket count
// grows from N to N+1, about 1/(N+1) of the keys move, and every key that
// moves lands on the new bucket. No key moves between two old buckets.
// Modulo hashing moves about N/(N+1) of them instead.
func jumpHash(key uint64, buckets int32) int32 {
	var b, j int64 = -1, 0
	for j < int64(buckets) {
		b = j
		key = key*2862933555777941757 + 1
		j = int64(float64(b+1) * (float64(int64(1)<<31) / float64((key>>33)+1)))
	}
	return int32(b)
}

// computeJobHomes answers the home map for jobPaths over the active slot
// count of members. The answer is deterministic and holds one entry for
// each distinct path.
func computeJobHomes(jobPaths []string, members []member) map[string]int32 {
	buckets := activeSlotCount(members)
	out := make(map[string]int32, len(jobPaths))
	for _, p := range jobPaths {
		if p == "" {
			continue
		}
		out[p] = jobHomeSlot(p, buckets)
	}
	return out
}

// jobHomesEqual reports whether two home maps hold the same entries.
// A recompute writes a new map only when the map changes.
func jobHomesEqual(a, b map[string]int32) bool {
	if len(a) != len(b) {
		return false
	}
	for path, slot := range a {
		other, ok := b[path]
		if !ok || other != slot {
			return false
		}
	}
	return true
}

// movedJobPaths answers the paths whose home differs between two maps,
// sorted. A path present in one map alone counts as moved.
func movedJobPaths(before, after map[string]int32) []string {
	moved := map[string]struct{}{}
	for path, slot := range after {
		if old, ok := before[path]; !ok || old != slot {
			moved[path] = struct{}{}
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			moved[path] = struct{}{}
		}
	}
	out := make([]string, 0, len(moved))
	for path := range moved {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
