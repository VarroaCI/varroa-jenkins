package controller

import (
	"time"

	"github.com/varroaci/varroa-jenkins/internal/transport"
)

// presenceFaultKeep is how long a failed presence read stays on record. A
// fault moves the roll clock of its own cell only while it is younger than
// the grace of the cell, and it holds a miss only while it is younger than
// staleMiteThreshold.
const presenceFaultKeep = cellMiteRollHoldCap

// readPresence reads the presence record of the mite of ns/name. Every
// presence read of the reconciler goes through this method. A read error
// is a doubtful read: it records a presence fault for ns/name alone, never
// for another controller.
func (r *Reconciler) readPresence(ns, name string) (transport.MiteInfo, bool, error) {
	info, ok, err := r.miteTransport.Info(ns, name)
	if err != nil {
		r.notePresenceFault(ns+"/"+name, time.Now())
	}
	return info, ok, err
}

// notePresenceFault records at as the newest failed presence read of key.
// An older time never replaces a newer one. It also drops every entry older
// than presenceFaultKeep, so the map holds only recent faults.
func (r *Reconciler) notePresenceFault(key string, at time.Time) {
	r.presenceFaultsMu.Lock()
	defer r.presenceFaultsMu.Unlock()
	if r.presenceFaults == nil {
		r.presenceFaults = make(map[string]time.Time)
	}
	if prev, ok := r.presenceFaults[key]; !ok || at.After(prev) {
		r.presenceFaults[key] = at
	}
	for k, t := range r.presenceFaults {
		if at.Sub(t) > presenceFaultKeep {
			delete(r.presenceFaults, k)
		}
	}
}

// lastPresenceFault returns the time of the newest failed presence read of
// key on this replica, or the zero time.
func (r *Reconciler) lastPresenceFault(key string) time.Time {
	r.presenceFaultsMu.Lock()
	defer r.presenceFaultsMu.Unlock()
	return r.presenceFaults[key]
}
