package recordstore

import "time"

// Values of TenantSyncDeadInstance.Derived. The recorder sets them on an
// entry it derives from its own stored member list. An operator entry
// carries no value.
const (
	// DeadInstanceDerivedRolled names an instance that a member's cell
	// replaced. A StatefulSet never runs two pods of one name, so the
	// old pod is gone.
	DeadInstanceDerivedRolled = "rolled"
	// DeadInstanceDerivedDropped names the instance of a cell that the
	// tenant no longer holds. The cell can still be alive and still
	// serve the old build.
	DeadInstanceDerivedDropped = "dropped"
)

// deadInstanceDerivation holds the inputs deriveDeadInstances reads. prior
// and announced are member views resolved through the scalar columns, so a
// row with no member list compares as one slot-0 member.
type deadInstanceDerivation struct {
	prior     []member
	announced []member
	// requestInstance is the request's scalar cell_instance.
	requestInstance string
	// listed is the merge of the stored dead_instances list and the
	// request's own entries.
	listed []TenantSyncDeadInstance
	// stale is staleAnnounce's answer for the request.
	stale bool
	now   time.Time
}

// staleAnnounceInput holds the inputs staleAnnounce reads. prior is the
// stored member view, resolved through the scalar columns.
type staleAnnounceInput struct {
	takeOver bool
	// retired is the stored retired_incarnations list.
	retired          []string
	storedGeneration int64
	prior            []member
	stored           []TenantSyncDeadInstance
	req              TenantSyncRequest
}

// staleAnnounce reports whether an announcement describes an older state
// than the stored row. The recorder writes nothing for such a request.
//
// A request that names a retired incarnation is always stale, take-over or
// not. A take-over replaced that tenant object, and Kubernetes never gives
// its uid to another object.
//
// Any other take-over is never stale. Any other request is stale when it
// names a lower generation. It is also stale when it names an instance
// that stored proves replaced and that no stored member holds. Only a
// confirmed entry or a rolled entry proves a replacement: the pod of a
// dropped cell can still be live, and a tenant can return to it.
//
// A request that confirms a stored member's instance gone is never stale
// on the instance test, because it proves the stored row the older one.
func staleAnnounce(in staleAnnounceInput) bool {
	if retiredIncarnation(in.retired, in.req.Incarnation) {
		return true
	}
	if in.takeOver {
		return false
	}
	if in.req.Generation < in.storedGeneration {
		return true
	}
	held := map[string]bool{}
	for _, m := range in.prior {
		if m.Instance != "" {
			held[m.Instance] = true
		}
	}
	for _, d := range in.req.DeadInstances {
		if d.ConfirmedGoneAt != nil && held[d.Instance] {
			return false
		}
	}
	replaced := map[string]bool{}
	for _, d := range in.stored {
		if d.ConfirmedGoneAt != nil || d.Derived == DeadInstanceDerivedRolled {
			replaced[d.Instance] = true
		}
	}
	named := []string{in.req.CellInstance}
	for _, m := range in.req.Members {
		named = append(named, m.Instance)
	}
	for _, instance := range named {
		if instance != "" && replaced[instance] && !held[instance] {
			return true
		}
	}
	return false
}

// deriveDeadInstances answers the dead instances the recorder adds from a
// change of its own stored member list. It adds the old instance of a
// member whose cell now reports a different instance, tagged rolled, and
// the instance of a member whose cell the request drops, tagged dropped.
//
// Each entry carries now as its last_seen_at and no confirmed_gone_at. The
// recorder reads no pod, so the entry keeps the moved window and the
// anchor test.
//
// It adds no entry for an instance that listed already holds, or that the
// request names. A repeated announce therefore adds nothing.
//
// It adds nothing for a request staleAnnounce judges stale. Without that
// test a stale request would list the live instance as dead.
func deriveDeadInstances(in deadInstanceDerivation) []TenantSyncDeadInstance {
	if in.stale {
		return nil
	}
	named := announcedInstances(in.requestInstance, in.announced)
	announcedByCell := map[string]string{}
	for _, m := range in.announced {
		announcedByCell[memberKey(m.Namespace, m.Cell)] = m.Instance
	}
	listed := map[string]bool{}
	for _, d := range in.listed {
		listed[d.Instance] = true
	}

	var out []TenantSyncDeadInstance
	for _, before := range in.prior {
		if before.Instance == "" || listed[before.Instance] || named[before.Instance] {
			continue
		}
		derived := DeadInstanceDerivedDropped
		if _, kept := announcedByCell[memberKey(before.Namespace, before.Cell)]; kept {
			derived = DeadInstanceDerivedRolled
		}
		listed[before.Instance] = true
		out = append(out, TenantSyncDeadInstance{
			Instance:   before.Instance,
			LastSeenAt: in.now,
			Derived:    derived,
		})
	}
	return out
}

// memberKey answers the key of one member: a cell holds at most one
// member of a tenant, so the namespace and cell pair is unique.
func memberKey(namespace, cell string) string {
	return namespace + "/" + cell
}

// announcedInstances answers the set of instances an announcement names:
// its scalar cell_instance and the instance of each announced member. An
// empty instance is never in the set.
func announcedInstances(requestInstance string, announced []member) map[string]bool {
	named := map[string]bool{}
	if requestInstance != "" {
		named[requestInstance] = true
	}
	for _, m := range announced {
		if m.Instance != "" {
			named[m.Instance] = true
		}
	}
	return named
}

// pruneNamedUnconfirmed removes every entry with no confirmed_gone_at for
// an instance that named holds. The caller passes the instances of an
// accepted announcement, which proves each of them live. A dropped entry
// goes, because it names a pod the tenant returned to. A rolled entry
// goes too, because a later announcement corrected the order it came from.
//
// An entry with a confirmation stays, because it comes from a pod read
// and a pod identifier never comes back.
func pruneNamedUnconfirmed(listed []TenantSyncDeadInstance, named map[string]bool) []TenantSyncDeadInstance {
	out := make([]TenantSyncDeadInstance, 0, len(listed))
	for _, d := range listed {
		if d.ConfirmedGoneAt == nil && named[d.Instance] {
			continue
		}
		out = append(out, d)
	}
	return out
}

// maxRetiredIncarnations bounds the retired_incarnations list. The list
// needs no time limit, because a retired uid never comes back.
const maxRetiredIncarnations = 8

// retiredIncarnation reports whether incarnation is on the retired list.
// An empty incarnation is never retired: it means unknown.
func retiredIncarnation(retired []string, incarnation string) bool {
	if incarnation == "" {
		return false
	}
	for _, r := range retired {
		if r == incarnation {
			return true
		}
	}
	return false
}

// retireIncarnation answers the retired list with incarnation added first.
// An empty incarnation adds nothing, and a repeat moves no entry. The
// answer holds at most maxRetiredIncarnations entries, newest first.
func retireIncarnation(retired []string, incarnation string) []string {
	out := append([]string(nil), retired...)
	if incarnation != "" && !retiredIncarnation(out, incarnation) {
		out = append([]string{incarnation}, out...)
	}
	if len(out) > maxRetiredIncarnations {
		out = out[:maxRetiredIncarnations]
	}
	return out
}
