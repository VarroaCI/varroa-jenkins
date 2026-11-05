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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// MemberBinding is one member of a pipelines tenant as the pass decides
// it this tick. It carries the eight fields the announcement sends, plus
// the applied flag the status reports.
type MemberBinding struct {
	Cell      string
	Namespace string
	// Role is v1alpha1.TenantMemberRoleActive or TenantMemberRoleStandby.
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
	// Stale is true when this member's cell fails the keep rule: it serves
	// another profile or belongs to another pool. A stale member keeps its
	// place until a replacement exists and its cell has drained. A stale
	// standby never takes a slot.
	Stale bool
	// Draining is true while this member's cell drops the tenant's folder
	// before the pass releases it. A draining member keeps its role and
	// slot, and the announcement still names it.
	Draining bool
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
// A dropped member keeps its place, marked Draining, until its cell drains.
func desiredMembers(in Inputs, tenant *v1alpha1.Tenant, status *v1alpha1.TenantStatus,
	slotZero *CellCandidate, nowT time.Time) ([]MemberBinding, string) {
	wantedSlots := tenant.Spec.Pipelines.EffectiveMembers()
	wantedStandby := tenant.Spec.Pipelines.EffectiveStandby()

	surviving, standbys := survivingMembers(in, tenant, status, nowT)
	held := map[string]bool{}
	var out []MemberBinding

	for slot := int32(0); slot < wantedSlots; slot++ {
		bound := bindSlot(in, tenant, slot, slotZero, surviving, &standbys, held)
		if bound == nil {
			continue
		}
		held[memberKey(bound.Namespace, bound.Cell)] = true
		out = append(out, *bound)
	}

	// The surviving standbys keep their cells, and the pass fills the rest
	// from the pool. A scale-down of the standby count releases a stale
	// standby first, then the highest cell names, because the surviving
	// list is sorted that way.
	var keptStandby []MemberBinding
	for _, s := range standbys {
		if int32(len(keptStandby)) >= wantedStandby {
			break
		}
		if held[memberKey(s.Namespace, s.Cell)] {
			continue
		}
		wasDraining := s.Draining
		s.Draining = false
		if s.Stale {
			// A stale standby keeps its cell until a fresh candidate exists,
			// and its cell has drained. The candidate binds on the drained tick.
			candidate := selectCandidateExcluding(in, tenant, held)
			switch {
			case !memberCellDrained(in, tenant, s):
				s.Draining = wasDraining || candidate != nil
			case candidate != nil:
				held[memberKey(candidate.Namespace, candidate.Name)] = true
				keptStandby = append(keptStandby, newMember(in, tenant, *candidate,
					v1alpha1.TenantMemberRoleStandby, nil, nowT))
				continue
			}
		}
		held[memberKey(s.Namespace, s.Cell)] = true
		s.Role = v1alpha1.TenantMemberRoleStandby
		s.Slot = nil
		keptStandby = append(keptStandby, s)
	}
	for int32(len(keptStandby)) < wantedStandby {
		candidate := selectCandidateExcluding(in, tenant, held)
		if candidate == nil {
			break
		}
		held[memberKey(candidate.Namespace, candidate.Name)] = true
		keptStandby = append(keptStandby, newMember(in, tenant, *candidate,
			v1alpha1.TenantMemberRoleStandby, nil, nowT))
	}
	out = append(out, keptStandby...)
	out = append(out, departingMembers(in, tenant, surviving, standbys, held)...)

	return out, memberReason(out, wantedSlots, wantedStandby)
}

// bindSlot answers the member that holds slot this tick. Slot 0 is the
// scalar binding. Every other slot keeps its live holder, else takes the
// newest applied standby, else takes a fresh candidate from the pool.
func bindSlot(in Inputs, tenant *v1alpha1.Tenant, slot int32, slotZero *CellCandidate,
	surviving map[int32]MemberBinding, standbys *[]MemberBinding, held map[string]bool) *MemberBinding {
	nowT := placementNow(in)
	if slot == 0 {
		if slotZero == nil {
			return nil
		}
		zero := newMember(in, tenant, *slotZero, v1alpha1.TenantMemberRoleActive, &slot, nowT)
		return &zero
	}
	if existing, ok := surviving[slot]; ok && !held[memberKey(existing.Namespace, existing.Cell)] {
		refreshed := existing
		if cell, found := lookupCell(in.Cells, existing.Cell, existing.Namespace); found {
			refreshed = newMember(in, tenant, cell, v1alpha1.TenantMemberRoleActive, &slot, nowT)
			refreshed.AppliedAt = keepAppliedAt(existing.AppliedAt, refreshed.Applied, nowT)
		}
		refreshed.Draining = false
		if !refreshed.Stale {
			return &refreshed
		}
		// A stale holder keeps the slot until its cell has drained and a
		// replacement exists. The cell drains first, so a build that still
		// runs there keeps its cloud, its credential and its agent Service.
		if !memberCellDrained(in, tenant, refreshed) {
			refreshed.Draining = existing.Draining || replacementExists(in, tenant, *standbys, held)
			return &refreshed
		}
		// A stale holder keeps the slot until a replacement exists, so the
		// slot never stands empty during a profile or pool change.
		if replacement := replaceSlotHolder(in, tenant, slot, standbys, held, nowT); replacement != nil {
			return replacement
		}
		return &refreshed
	}
	return replaceSlotHolder(in, tenant, slot, standbys, held, nowT)
}

// replaceSlotHolder answers a new holder for slot: the newest applied
// standby, else a fresh candidate from the pool. It answers nil when
// neither exists.
func replaceSlotHolder(in Inputs, tenant *v1alpha1.Tenant, slot int32, standbys *[]MemberBinding,
	held map[string]bool, nowT time.Time) *MemberBinding {
	if promoted := takePromotionSource(standbys, held); promoted != nil {
		promoted.Role = v1alpha1.TenantMemberRoleActive
		s := slot
		promoted.Slot = &s
		return promoted
	}
	candidate := selectCandidateExcluding(in, tenant, held)
	if candidate == nil {
		return nil
	}
	fresh := newMember(in, tenant, *candidate, v1alpha1.TenantMemberRoleActive, &slot, nowT)
	return &fresh
}

// takePromotionSource removes and answers the standby that should take a
// slot: the applied standby with the newest applied time, tie-broken by
// cell name ascending. A newer applied time means a fresher folder. It
// answers nil when no applied standby remains. A stale standby is never a
// promotion source, because it would bind the slot to a cell of the old
// profile or pool. A draining standby is never a promotion source either.
func takePromotionSource(standbys *[]MemberBinding, held map[string]bool) *MemberBinding {
	best := -1
	for i, s := range *standbys {
		if !s.Applied || s.Stale || s.Draining || held[memberKey(s.Namespace, s.Cell)] {
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

// memberCellDrained reports whether a member's cell holds no running build
// of the tenant: the cell is gone, evacuates, runs no pod, or has dropped
// the tenant's folder. A cell with no pod holds nothing, because its builds
// stopped with the pod.
func memberCellDrained(in Inputs, tenant *v1alpha1.Tenant, m MemberBinding) bool {
	cell, found := lookupCell(in.Cells, m.Cell, m.Namespace)
	if !found || cell.Evacuate || cell.Instance == "" {
		return true
	}
	return cellDrainedTenant(cell, tenant.Name)
}

// TenantDrainsFromCell reports whether the tenant's folder leaves the cell
// namespace/name (tenant-isolation spec). The controller reconciler omits
// the folder from that cell's items while it answers true. A voluntary move
// drains slot 0's cell and each stale member's cell. A member marked
// draining drains its own cell. Every other member cell keeps the folder:
// the standby that takes slot 0 is never stale or draining, so it stays
// applied through the drain.
func TenantDrainsFromCell(status *v1alpha1.TenantStatus, namespace, name string) bool {
	p := status.Placement
	if p == nil {
		return false
	}
	moving := false
	for _, c := range status.Conditions {
		if c.Type == v1alpha1.TenantConditionPlaced {
			moving = c.Status == metav1.ConditionFalse && c.Reason == v1alpha1.TenantReasonTenantMoving
			break
		}
	}
	if moving && p.Cell == name && p.CellNamespace == namespace {
		return true
	}
	for _, m := range p.Members {
		if m.Cell == name && m.CellNamespace == namespace {
			return m.Draining || (moving && m.Stale)
		}
	}
	return false
}

// replacementExists reports whether a stale slot holder has a replacement:
// an applied standby that is not stale and not draining, or a candidate of
// the selection rule. It changes nothing.
func replacementExists(in Inputs, tenant *v1alpha1.Tenant, standbys []MemberBinding, held map[string]bool) bool {
	for _, s := range standbys {
		if s.Applied && !s.Stale && !s.Draining && !held[memberKey(s.Namespace, s.Cell)] {
			return true
		}
	}
	return selectCandidateExcluding(in, tenant, held) != nil
}

// departingMembers answers each surviving member that holds no place this
// tick and whose cell has not drained. Each keeps its role and slot, marked
// Draining, so the pass releases its cell only after the cell drops the
// tenant's folder. A scale-down reaches this path.
func departingMembers(in Inputs, tenant *v1alpha1.Tenant, surviving map[int32]MemberBinding,
	standbys []MemberBinding, held map[string]bool) []MemberBinding {
	slots := make([]int32, 0, len(surviving))
	for slot := range surviving {
		slots = append(slots, slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i] < slots[j] })
	var out []MemberBinding
	keep := func(m MemberBinding) {
		key := memberKey(m.Namespace, m.Cell)
		if held[key] || memberCellDrained(in, tenant, m) {
			return
		}
		held[key] = true
		m.Draining = true
		out = append(out, m)
	}
	for _, slot := range slots {
		keep(surviving[slot])
	}
	for _, s := range standbys {
		keep(s)
	}
	return out
}

// survivingMembers answers the tenant's current active members by slot and
// its current standbys, dropping every member whose cell left the
// candidate set or is evacuating. A member whose cell fails the keep rule
// stays, marked Stale through newMember. Slot 0 is excluded: the keep and
// move path owns it. Stale standbys sort last.
func survivingMembers(in Inputs, tenant *v1alpha1.Tenant, status *v1alpha1.TenantStatus,
	nowT time.Time) (map[int32]MemberBinding, []MemberBinding) {
	active := map[int32]MemberBinding{}
	var standbys []MemberBinding
	if status.Placement == nil {
		return active, standbys
	}

	for _, m := range status.Placement.Members {
		cell, found := lookupCell(in.Cells, m.Cell, m.CellNamespace)
		if !found || cell.Evacuate {
			continue
		}
		role := m.Role
		if role == "" {
			role = v1alpha1.TenantMemberRoleActive
		}
		bound := newMember(in, tenant, cell, role, m.Slot, nowT)
		bound.AppliedAt = keepAppliedAt(memberAppliedAt(in, m.CellNamespace, m.Cell), bound.Applied, nowT)
		bound.Draining = m.Draining
		if role == v1alpha1.TenantMemberRoleStandby {
			standbys = append(standbys, bound)
			continue
		}
		if m.Slot != nil && *m.Slot > 0 {
			active[*m.Slot] = bound
		}
	}
	sort.Slice(standbys, func(i, j int) bool {
		if standbys[i].Stale != standbys[j].Stale {
			return !standbys[i].Stale
		}
		return standbys[i].Cell < standbys[j].Cell
	})
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
func newMember(in Inputs, tenant *v1alpha1.Tenant, cell CellCandidate, role string,
	slot *int32, nowT time.Time) MemberBinding {
	m := MemberBinding{
		Cell:       cell.Name,
		Namespace:  cell.Namespace,
		Role:       role,
		Instance:   cell.Instance,
		Applied:    containsString(cell.AppliedTenants, tenant.Name),
		LastSeenAt: cell.MiteLastSeen,
		Stale:      !cellKeepsTenant(in, tenant, cell),
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
func promotionCellForSlotZero(in Inputs, tenant *v1alpha1.Tenant, status *v1alpha1.TenantStatus) *CellCandidate {
	if status.Placement == nil || tenant.Spec.Pipelines == nil {
		return nil
	}
	_, standbys := survivingMembers(in, tenant, status, placementNow(in))
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

// cellKeepsTenant is the keep rule for a bound cell. The cell is enabled,
// serves the tenant's profile, and passes the membership test. Slot 0 and
// every other member read this one rule, so a profile change or a pool
// change reaches every member.
func cellKeepsTenant(in Inputs, tenant *v1alpha1.Tenant, c CellCandidate) bool {
	return c.Enabled && c.ResolvedProfile == tenant.Spec.Pipelines.Profile && keepsMembership(in, tenant, c)
}

// selectCandidateExcluding is selectCandidate with one added test: the
// cell must not already hold a member of this tenant.
func selectCandidateExcluding(in Inputs, tenant *v1alpha1.Tenant, held map[string]bool) *CellCandidate {
	narrowed := in
	narrowed.Cells = nil
	for _, c := range in.Cells {
		if held[memberKey(c.Namespace, c.Name)] {
			continue
		}
		narrowed.Cells = append(narrowed.Cells, c)
	}
	return selectCandidate(narrowed, tenant)
}

// memberReason answers the Placed reason the member set implies. The
// rank is SlotPending, then StandbyPending, then MembersStale, then
// MembersApplied. An empty answer means the member set adds no reason,
// and the caller's own reason stands.
func memberReason(members []MemberBinding, wantedSlots, wantedStandby int32) string {
	activeCount := int32(0)
	standbyCount := int32(0)
	applied := true
	stale := false
	for _, m := range members {
		if m.Role == v1alpha1.TenantMemberRoleStandby {
			standbyCount++
		} else {
			activeCount++
		}
		if !m.Applied {
			applied = false
		}
		if m.Stale {
			stale = true
		}
	}
	switch {
	case activeCount < wantedSlots:
		return v1alpha1.TenantReasonSlotPending
	case standbyCount < wantedStandby:
		return v1alpha1.TenantReasonStandbyPending
	case stale && wantedSlots+wantedStandby > 1:
		return v1alpha1.TenantReasonMembersStale
	case applied && wantedSlots+wantedStandby > 1:
		// A tenant that asks for one member keeps today's TenantPlaced
		// reason, so every earlier rule is unchanged for it.
		return v1alpha1.TenantReasonMembersApplied
	default:
		return ""
	}
}

// memberStatuses turns the decided member list into the status shape.
func memberStatuses(members []MemberBinding) []v1alpha1.TenantMemberStatus {
	if len(members) == 0 {
		return nil
	}
	out := make([]v1alpha1.TenantMemberStatus, 0, len(members))
	for _, m := range members {
		entry := v1alpha1.TenantMemberStatus{
			Cell: m.Cell, CellNamespace: m.Namespace, Role: m.Role, Applied: m.Applied,
			Stale: m.Stale, Draining: m.Draining,
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
