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
	// stored is the dead_instances list stored before this announce.
	stored []TenantSyncDeadInstance
	// listed is the merge of stored and the request's own entries.
	listed []TenantSyncDeadInstance
	// staleGeneration is true when the request names a lower generation
	// than the stored row and does not take the row over.
	staleGeneration bool
	now             time.Time
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
// It adds nothing for a stale request. A request is stale when it names a
// lower generation, or when it names an instance that stored lists dead
// and that no stored member holds. A pod identifier never comes back, so
// such a request describes an older state than the stored row. Without the
// second test a stale request would list the live instance as dead.
//
// The second test also holds once for a tenant that returns to a live pod
// it left. The stored members hold that pod after the return, so the next
// announce derives again.
func deriveDeadInstances(in deadInstanceDerivation) []TenantSyncDeadInstance {
	if in.staleGeneration {
		return nil
	}
	named := map[string]bool{}
	if in.requestInstance != "" {
		named[in.requestInstance] = true
	}
	announcedByCell := map[string]string{}
	for _, m := range in.announced {
		if m.Instance != "" {
			named[m.Instance] = true
		}
		announcedByCell[memberKey(m.Namespace, m.Cell)] = m.Instance
	}
	held := map[string]bool{}
	for _, m := range in.prior {
		if m.Instance != "" {
			held[m.Instance] = true
		}
	}
	for _, d := range in.stored {
		if named[d.Instance] && !held[d.Instance] {
			return nil
		}
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
