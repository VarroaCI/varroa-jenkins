package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestEnsureWakeEndpointSlice(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Service{ObjectMeta: v1.ObjectMeta{Name: "ctrl-svc", Namespace: "ns", UID: "service-uid"}})
	c := &ClientsetClient{clientset: cs}
	ctx := context.Background()
	if err := c.EnsureWakeEndpointSlice(ctx, "ns", "ctrl-svc", []string{"10.0.0.2", "10.0.0.1", "2001:db8::1"}, 8082); err != nil {
		t.Fatal(err)
	}
	slice, err := cs.DiscoveryV1().EndpointSlices("ns").Get(ctx, "ctrl-svc-wake", v1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if slice.Labels["kubernetes.io/service-name"] != "ctrl-svc" || slice.Labels["endpointslice.kubernetes.io/managed-by"] != "varroa-operator" {
		t.Fatalf("unexpected labels: %#v", slice.Labels)
	}
	if slice.AddressType != discoveryv1.AddressTypeIPv4 || len(slice.Endpoints) != 2 || slice.Ports[0].Name == nil || *slice.Ports[0].Name != "http" || *slice.Ports[0].Port != 8082 {
		t.Fatalf("unexpected slice: %#v", slice)
	}
	if len(slice.OwnerReferences) != 1 || slice.OwnerReferences[0].UID != "service-uid" {
		t.Fatalf("missing service owner reference: %#v", slice.OwnerReferences)
	}

	before := len(cs.Actions())
	if err := c.EnsureWakeEndpointSlice(ctx, "ns", "ctrl-svc", []string{"10.0.0.1", "10.0.0.2"}, 8082); err != nil {
		t.Fatal(err)
	}
	for _, action := range cs.Actions()[before:] {
		if action.GetVerb() == "update" || action.GetVerb() == "create" {
			t.Fatalf("converged ensure wrote %s", action.GetVerb())
		}
	}
	if err := c.EnsureWakeEndpointSlice(ctx, "ns", "ctrl-svc", []string{"10.0.0.3"}, 8082); err != nil {
		t.Fatal(err)
	}
	updated, err := cs.DiscoveryV1().EndpointSlices("ns").Get(ctx, "ctrl-svc-wake", v1.GetOptions{})
	if err != nil || len(updated.Endpoints) != 1 || updated.Endpoints[0].Addresses[0] != "10.0.0.3" {
		t.Fatalf("slice did not converge: %v %#v", err, updated)
	}
}

// TestEnsureWakeEndpointSliceRepairsLabelDrift verifies the converge check does
// not skip a rewrite when the routable labels drift even though addresses and
// ports are unchanged — a wrong kubernetes.io/service-name silently unroutes the
// slice from the Service (ingress-nginx/kube-proxy key on that label).
func TestEnsureWakeEndpointSliceRepairsLabelDrift(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.Service{ObjectMeta: v1.ObjectMeta{Name: "ctrl-svc", Namespace: "ns", UID: "service-uid"}})
	c := &ClientsetClient{clientset: cs}
	if err := c.EnsureWakeEndpointSlice(ctx, "ns", "ctrl-svc", []string{"10.0.0.1"}, 8082); err != nil {
		t.Fatal(err)
	}
	// Corrupt the association label out-of-band.
	slice, _ := cs.DiscoveryV1().EndpointSlices("ns").Get(ctx, "ctrl-svc-wake", v1.GetOptions{})
	slice.Labels["kubernetes.io/service-name"] = "wrong-svc"
	if _, err := cs.DiscoveryV1().EndpointSlices("ns").Update(ctx, slice, v1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureWakeEndpointSlice(ctx, "ns", "ctrl-svc", []string{"10.0.0.1"}, 8082); err != nil {
		t.Fatal(err)
	}
	repaired, _ := cs.DiscoveryV1().EndpointSlices("ns").Get(ctx, "ctrl-svc-wake", v1.GetOptions{})
	if repaired.Labels["kubernetes.io/service-name"] != "ctrl-svc" {
		t.Fatalf("label drift not repaired: %#v", repaired.Labels)
	}
}

func TestDeleteWakeEndpointSliceIdempotent(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &ClientsetClient{clientset: cs}
	if err := c.DeleteWakeEndpointSlice(context.Background(), "ns", "ctrl-svc"); err != nil {
		t.Fatal(err)
	}
}

func TestListOperatorPodIPs(t *testing.T) {
	ready := corev1.ConditionTrue
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: v1.ObjectMeta{Name: "operator", Namespace: "varroa-system", Labels: map[string]string{"app.kubernetes.io/component": "varroa-operator"}}, Status: corev1.PodStatus{PodIP: "10.0.0.2", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}}}, &corev1.Pod{ObjectMeta: v1.ObjectMeta{Name: "not-ready", Namespace: "varroa-system", Labels: map[string]string{"app.kubernetes.io/component": "varroa-operator"}}, Status: corev1.PodStatus{PodIP: "10.0.0.3"}})
	c := &ClientsetClient{clientset: cs}
	ips, err := c.ListOperatorPodIPs(context.Background(), "varroa-system")
	if err != nil || len(ips) != 1 || ips[0] != "10.0.0.2" {
		t.Fatalf("ListOperatorPodIPs() = %#v, %v", ips, err)
	}
}
