package controller

import (
	"context"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller/cells"
	"github.com/varroaci/varroa-jenkins/internal/controller/runners"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
)

// poolLibraryLookupHost resolves a library remote host to its addresses.
// Tests replace it with a fixed answer. The reconciler calls it on every
// tick, so a host with rotating addresses rewrites the policy often.
var poolLibraryLookupHost func(ctx context.Context, host string) ([]string, error) = net.DefaultResolver.LookupHost

// poolLibraryRemoteHostPort returns the host and TCP port a library remote
// dials. It accepts the https and ssh URL forms and the scp-like
// user@host:path form. The https port defaults to 443, and the ssh and
// scp-like ports default to 22. It returns ok false for a remote that
// holds a variable token, a remote with no usable host, or a remote in any
// other form.
func poolLibraryRemoteHostPort(remote string) (host string, port int, ok bool) {
	if strings.Contains(remote, unresolvedVarToken) {
		return "", 0, false
	}

	if strings.HasPrefix(remote, "https://") || strings.HasPrefix(remote, "ssh://") {
		u, err := url.Parse(remote)
		if err != nil {
			return "", 0, false
		}
		host = u.Hostname()
		if host == "" {
			return "", 0, false
		}
		port = 443
		if u.Scheme == "ssh" {
			port = 22
		}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return "", 0, false
			}
			port = n
		}
		return host, port, true
	}

	at := strings.Index(remote, "@")
	if at < 0 {
		return "", 0, false
	}
	rest := remote[at+1:]
	colon := strings.Index(rest, ":")
	if colon <= 0 {
		return "", 0, false
	}
	host = rest[:colon]
	if strings.Contains(host, "/") {
		return "", 0, false
	}
	return host, 22, true
}

// poolLibraryEgressPeers resolves each pool library remote into the cell
// egress peers it needs. A library whose remote does not parse adds no
// peer. An IP literal host adds one peer and needs no lookup. A hostname
// is resolved on every call. A lookup error or an empty answer skips only
// that library. The result holds no duplicate (IP, port) pair and is
// sorted by IP, then by port.
func poolLibraryEgressPeers(ctx context.Context, libs []v1alpha1.CellPoolLibrary) []runners.RunnerEgressPeer {
	seen := make(map[runners.RunnerEgressPeer]bool)
	var peers []runners.RunnerEgressPeer
	add := func(p runners.RunnerEgressPeer) {
		if !seen[p] {
			seen[p] = true
			peers = append(peers, p)
		}
	}

	for _, lib := range libs {
		host, port, ok := poolLibraryRemoteHostPort(lib.Remote)
		if !ok {
			continue
		}
		if net.ParseIP(host) != nil {
			add(runners.RunnerEgressPeer{IP: host, Port: port})
			continue
		}
		addrs, err := poolLibraryLookupHost(ctx, host)
		if err != nil || len(addrs) == 0 {
			continue
		}
		for _, addr := range addrs {
			add(runners.RunnerEgressPeer{IP: addr, Port: port})
		}
	}

	sort.Slice(peers, func(i, j int) bool {
		if peers[i].IP != peers[j].IP {
			return peers[i].IP < peers[j].IP
		}
		return peers[i].Port < peers[j].Port
	})
	return peers
}

// cellPoolLibraries reads the libraries the cell's owning pool declares. It
// returns no libraries when the cell carries no pool label, when the store
// is unset, or when the pool does not exist. A read failure returns an
// error, so the caller never mistakes it for a pool with no libraries.
func (r *Reconciler) cellPoolLibraries(ctx context.Context, cr *v1alpha1.Controller) ([]v1alpha1.CellPoolLibrary, error) {
	name := cr.Labels[cells.LabelCellPool]
	if name == "" || r.store == nil {
		return nil, nil
	}
	pool, err := crdstore.Get[v1alpha1.CellPool](ctx, r.store, name, "")
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return pool.Spec.Libraries, nil
}
