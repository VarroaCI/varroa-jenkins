// Package cells: this file holds the member decision of the placement
// pass. A pipelines tenant runs on spec.pipelines.members active slots and
// spec.pipelines.standby warm members. Each member is one cell that holds
// the tenant's folder, and a cell holds at most one member of a tenant.
//
// desiredMembers is pure. It performs no Kubernetes read and no
// Kubernetes write, and it never creates a cell: a pool with no free cell
// reports SlotPending or StandbyPending, and the pool's own gates state
// why.
package cells

import (
	"sort"
	"time"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// MemberBinding is one member of a pipelines tenant as the pass decides
// it this tick. It carries the eight fields the announcement sends, plus
// the applied flag the status reports.
type MemberBinding struct {
	Cell      string
	Namespace string
	// Role is v1alpha1.TeamMemberRoleActive or TeamMemberRoleStandby.
	Role string
	// Slot is set only on an active member.
	Slot *int32
	// Instance is the cell pod's unique identifier, empty when no pod
	// runs.
	Instance string
	// Applied is true when this member's cell reports the tenant applied.
	Applied bool
	// AppliedAt is when this member's cell first reported the tenant
	// applied. The promotion choice reads it.
	AppliedAt *time.Time
	// RehydratedInstance and PendingJobs are this member's own seed state.
	RehydratedInstance string
	PendingJobs        []string
	// LastSeenAt is this member's own newest mite-presence reading.
	LastSeenAt *time.Time
}

// slotValue answers a member's slot index, and -1 for a standby.
func (m MemberBinding) slotValue() int32 {
	if m.Slot == nil {
		return -1
	}
	return *m.Slot
}

// memberKey names a member's cell. A cell holds at most one member of a
// tenant, so the pair is the member's key.
func memberKey(namespace, cell string) string { return namespace + "/" + cell }

// desiredMembers answers the member list the tenant should hold this
// tick, and the Placed reason the member set implies. An empty reason
// means the member set adds no reason of its own.
//
// slotZero is the tenant's scalar binding, which the existing keep and
// move path decides. A nil slotZero means the tenant holds no cell yet.
func desiredMembers(in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus,
	slotZero *CellCandidate, nowT time.Time) ([]MemberBinding, string) {
	wantedSlots := team.Spec.Pipelines.EffectiveMembers()
	wantedStandby := team.Spec.Pipelines.EffectiveStandby()

	surviving, standbys := survivingMembers(in, team, status, nowT)
	held := map[string]bool{}
	var out []MemberBinding

	for slot := int32(0); slot < wantedSlots; slot++ {
		bound := bindSlot(in, team, slot, slotZero, surviving, &standbys, held)
		if bound == nil {
			continue
		}
		held[memberKey(bound.Namespace, bound.Cell)] = true
		out = append(out, *bound)
	}

	// The surviving standbys keep their cells, and the pass fills the rest
	// from the pool. A scale-down of the standby count releases the
	// highest cell names first, because the surviving list is sorted.
	var keptStandby []MemberBinding
	for _, s := range standbys {
		if int32(len(keptStandby)) >= wantedStandby {
			break
		}
		if held[memberKey(s.Namespace, s.Cell)] {
			continue
		}
		held[memberKey(s.Namespace, s.Cell)] = true
		s.Role = v1alpha1.TeamMemberRoleStandby
		s.Slot = nil
		keptStandby = append(keptStandby, s)
	}
	for int32(len(keptStandby)) < wantedStandby {
		candidate := selectCandidateExcluding(in, team, held)
		if candidate == nil {
			break
		}
		held[memberKey(candidate.Namespace, candidate.Name)] = true
		keptStandby = append(keptStandby, newMember(in, team, *candidate,
			v1alpha1.TeamMemberRoleStandby, nil, nowT))
	}
	out = append(out, keptStandby...)

	return out, memberReason(out, wantedSlots, wantedStandby)
}

// bindSlot answers the member that holds slot this tick. Slot 0 is the
// scalar binding. Every other slot keeps its live holder, else takes the
// newest applied standby, else takes a fresh candidate from the pool.
func bindSlot(in Inputs, team *v1alpha1.Team, slot int32, slotZero *CellCandidate,
	surviving map[int32]MemberBinding, standbys *[]MemberBinding, held map[string]bool) *MemberBinding {
	nowT := placementNow(in)
	if slot == 0 {
		if slotZero == nil {
			return nil
		}
		zero := newMember(in, team, *slotZero, v1alpha1.TeamMemberRoleActive, &slot, nowT)
		return &zero
	}
	if existing, ok := surviving[slot]; ok && !held[memberKey(existing.Namespace, existing.Cell)] {
		refreshed := existing
		if cell, found := lookupCell(in.Cells, existing.Cell, existing.Namespace); found {
			refreshed = newMember(in, team, cell, v1alpha1.TeamMemberRoleActive, &slot, nowT)
			refreshed.AppliedAt = keepAppliedAt(existing.AppliedAt, refreshed.Applied, nowT)
		}
		return &refreshed
	}
	if promoted := takePromotionSource(standbys, held); promoted != nil {
		promoted.Role = v1alpha1.TeamMemberRoleActive
		s := slot
		promoted.Slot = &s
		return promoted
	}
	candidate := selectCandidateExcluding(in, team, held)
	if candidate == nil {
		return nil
	}
	fresh := newMember(in, team, *candidate, v1alpha1.TeamMemberRoleActive, &slot, nowT)
	return &fresh
}

// takePromotionSource removes and answers the standby that should take a
// slot: the applied standby with the newest applied time, tie-broken by
// cell name ascending. A newer applied time means a fresher folder. It
// answers nil when no applied standby remains.
func takePromotionSource(standbys *[]MemberBinding, held map[string]bool) *MemberBinding {
	best := -1
	for i, s := range *standbys {
		if !s.Applied || held[memberKey(s.Namespace, s.Cell)] {
			continue
		}
		if best < 0 || newerApplied(s, (*standbys)[best]) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	chosen := (*standbys)[best]
	*standbys = append((*standbys)[:best], (*standbys)[best+1:]...)
	return &chosen
}

// newerApplied reports whether a should be promoted before b: a newer
// applied time first, then the lower cell name.
func newerApplied(a, b MemberBinding) bool {
	switch {
	case a.AppliedAt != nil && b.AppliedAt != nil && !a.AppliedAt.Equal(*b.AppliedAt):
		return a.AppliedAt.After(*b.AppliedAt)
	case a.AppliedAt != nil && b.AppliedAt == nil:
		return true
	case a.AppliedAt == nil && b.AppliedAt != nil:
		return false
	default:
		return a.Cell < b.Cell
	}
}

// survivingMembers answers the tenant's current active members by slot and
// its current standbys, dropping every member whose cell left the
// candidate set, is evacuating, or is disabled. Slot 0 is excluded: the
// keep and move path owns it.
func survivingMembers(in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus,
	nowT time.Time) (map[int32]MemberBinding, []MemberBinding) {
	active := map[int32]MemberBinding{}
	var standbys []MemberBinding
	if status.Placement == nil {
		return active, standbys
	}

	for _, m := range status.Placement.Members {
		cell, found := lookupCell(in.Cells, m.Cell, m.CellNamespace)
		if !found || cell.Evacuate || !cell.Enabled {
			continue
		}
		role := m.Role
		if role == "" {
			role = v1alpha1.TeamMemberRoleActive
		}
		bound := newMember(in, team, cell, role, m.Slot, nowT)
		bound.AppliedAt = keepAppliedAt(memberAppliedAt(in, m.CellNamespace, m.Cell), bound.Applied, nowT)
		if role == v1alpha1.TeamMemberRoleStandby {
			standbys = append(standbys, bound)
			continue
		}
		if m.Slot != nil && *m.Slot > 0 {
			active[*m.Slot] = bound
		}
	}
	sort.Slice(standbys, func(i, j int) bool { return standbys[i].Cell < standbys[j].Cell })
	return active, standbys
}

// memberAppliedAt answers the applied time the last announcement recorded
// for one member cell, so a promotion choice survives a tick.
func memberAppliedAt(in Inputs, namespace, cell string) *time.Time {
	if in.LastSync == nil {
		return nil
	}
	for _, m := range in.LastSync.Members {
		if m.Namespace == namespace && m.Cell == cell {
			return m.AppliedAt
		}
	}
	return nil
}

// keepAppliedAt answers the applied time to carry forward: the stored one
// when it exists, this tick's clock when the member is applied and no
// stored time exists, and nil when the member is not applied.
func keepAppliedAt(stored *time.Time, applied bool, nowT time.Time) *time.Time {
	if !applied {
		return nil
	}
	if stored != nil {
		return stored
	}
	at := nowT
	return &at
}

// newMember builds one member from a cell candidate. Its seed state comes
// from that cell's own rehydration entry, through the MemberSeed seam, so
// one member's deferred job never holds another member's build.
func newMember(in Inputs, team *v1alpha1.Team, cell CellCandidate, role string,
	slot *int32, nowT time.Time) MemberBinding {
	m := MemberBinding{
		Cell:       cell.Name,
		Namespace:  cell.Namespace,
		Role:       role,
		Instance:   cell.Instance,
		Applied:    containsString(cell.AppliedTenants, team.Name),
		LastSeenAt: cell.MiteLastSeen,
	}
	if slot != nil {
		s := *slot
		m.Slot = &s
	}
	if in.MemberSeed != nil {
		m.RehydratedInstance, m.PendingJobs = in.MemberSeed(cell.Namespace, cell.Name)
	}
	m.AppliedAt = keepAppliedAt(nil, m.Applied, nowT)
	return m
}

// promotionCellForSlotZero answers the cell a standby holds that should
// take slot 0, and nil when no applied standby exists. The slot then
// takes the existing move path, which selects a fresh candidate cell.
//
// A newer applied time means a fresher folder, so a promoted standby
// carries a usable history the moment it takes the slot.
func promotionCellForSlotZero(in Inputs, team *v1alpha1.Team, status *v1alpha1.TeamStatus) *CellCandidate {
	if status.Placement == nil || team.Spec.Pipelines == nil {
		return nil
	}
	_, standbys := survivingMembers(in, team, status, placementNow(in))
	chosen := takePromotionSource(&standbys, map[string]bool{})
	if chosen == nil {
		return nil
	}
	cell, found := lookupCell(in.Cells, chosen.Cell, chosen.Namespace)
	if !found || !cell.CellReady {
		return nil
	}
	return &cell
}

// selectCandidateExcluding is selectCandidate with one added test: the
// cell must not already hold a member of this tenant.
func selectCandidateExcluding(in Inputs, team *v1alpha1.Team, held map[string]bool) *CellCandidate {
	narrowed := in
	narrowed.Cells = nil
	for _, c := range in.Cells {
		if held[memberKey(c.Namespace, c.Name)] {
			continue
		}
		narrowed.Cells = append(narrowed.Cells, c)
	}
	return selectCandidate(narrowed, team)
}

// memberReason answers the Placed reason the member set implies.
// SlotPending outranks StandbyPending. An empty answer means the member
// set adds no reason, and the caller's own reason stands.
func memberReason(members []MemberBinding, wantedSlots, wantedStandby int32) string {
	activeCount := int32(0)
	standbyCount := int32(0)
	applied := true
	for _, m := range members {
		if m.Role == v1alpha1.TeamMemberRoleStandby {
			standbyCount++
		} else {
			activeCount++
		}
		if !m.Applied {
			applied = false
		}
	}
	switch {
	case activeCount < wantedSlots:
		return v1alpha1.TeamReasonSlotPending
	case standbyCount < wantedStandby:
		return v1alpha1.TeamReasonStandbyPending
	case applied && wantedSlots+wantedStandby > 1:
		// A tenant that asks for one member keeps today's TenantPlaced
		// reason, so every earlier rule is unchanged for it.
		return v1alpha1.TeamReasonMembersApplied
	default:
		return ""
	}
}

// memberStatuses turns the decided member list into the status shape.
func memberStatuses(members []MemberBinding) []v1alpha1.TeamMemberStatus {
	if len(members) == 0 {
		return nil
	}
	out := make([]v1alpha1.TeamMemberStatus, 0, len(members))
	for _, m := range members {
		entry := v1alpha1.TeamMemberStatus{
			Cell: m.Cell, CellNamespace: m.Namespace, Role: m.Role, Applied: m.Applied,
		}
		if m.Slot != nil {
			s := *m.Slot
			entry.Slot = &s
		}
		out = append(out, entry)
	}
	return out
}

// announceMembers turns the decided member list into the announcement
// shape, ordered by role then slot then cell name, so a repeat
// announcement of an unchanged set compares equal.
func announceMembers(members []MemberBinding) []AnnounceMember {
	if len(members) == 0 {
		return nil
	}
	sorted := make([]MemberBinding, len(members))
	copy(sorted, members)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].slotValue() != sorted[j].slotValue() {
			// A standby sorts after every active slot.
			if sorted[i].slotValue() < 0 {
				return false
			}
			if sorted[j].slotValue() < 0 {
				return true
			}
			return sorted[i].slotValue() < sorted[j].slotValue()
		}
		return sorted[i].Cell < sorted[j].Cell
	})

	out := make([]AnnounceMember, 0, len(sorted))
	for _, m := range sorted {
		entry := AnnounceMember{
			Cell: m.Cell, Namespace: m.Namespace, Role: m.Role,
			Instance: m.Instance, RehydratedInstance: m.RehydratedInstance,
			PendingJobs: m.PendingJobs, AppliedAt: m.AppliedAt,
			LastSeenAt: m.LastSeenAt,
		}
		if m.Slot != nil {
			s := *m.Slot
			entry.Slot = &s
		}
		out = append(out, entry)
	}
	return out
}

// placementNow answers the pass's clock.
func placementNow(in Inputs) time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}
