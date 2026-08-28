package controller

import (
	"context"
	"net"
	"sort"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const operatorPodLabel = "app.kubernetes.io/component=varroa-operator"

func wakeSliceName(serviceName string) string {
	return serviceName + "-wake"
}

func ipFamily(ip string) discoveryv1.AddressType {
	parsed := net.ParseIP(ip)
	if parsed != nil && parsed.To4() == nil {
		return discoveryv1.AddressTypeIPv6
	}
	return discoveryv1.AddressTypeIPv4
}

// EnsureWakeEndpointSlice creates or converges the custom wake EndpointSlice for
// serviceName so the controller's Service routes to the operator wake server while
// hibernated. Writes are skipped when the live slice already matches (D2).
func (c *ClientsetClient) EnsureWakeEndpointSlice(ctx context.Context, namespace, serviceName string, podIPs []string, port int32) error {
	if len(podIPs) == 0 {
		return nil
	}
	family := ipFamily(podIPs[0])
	addresses := make([]string, 0, len(podIPs))
	for _, ip := range podIPs {
		if ipFamily(ip) == family {
			addresses = append(addresses, ip)
		}
	}
	sort.Strings(addresses)
	ready := true
	protocol := corev1.ProtocolTCP
	portName := "http"
	desired := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name: wakeSliceName(serviceName), Namespace: namespace,
			Labels: map[string]string{
				"kubernetes.io/service-name":             serviceName,
				"endpointslice.kubernetes.io/managed-by": "varroa-operator",
			},
		},
		AddressType: family,
		Endpoints:   make([]discoveryv1.Endpoint, 0, len(addresses)),
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Port: &port, Protocol: &protocol}},
	}
	for _, address := range addresses {
		desired.Endpoints = append(desired.Endpoints, discoveryv1.Endpoint{Addresses: []string{address}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}})
	}
	slices := c.clientset.DiscoveryV1().EndpointSlices(namespace)
	existing, err := slices.Get(ctx, desired.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		svc, getErr := c.clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		desired.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(svc, corev1.SchemeGroupVersion.WithKind("Service"))}
		_, err = slices.Create(ctx, desired, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.AddressType == desired.AddressType && labelsEqual(existing.Labels, desired.Labels) && portsEqual(existing.Ports, desired.Ports) && endpointsEqual(existing.Endpoints, desired.Endpoints) {
		return nil
	}
	existing.Labels = desired.Labels
	existing.AddressType = desired.AddressType
	existing.Endpoints = desired.Endpoints
	existing.Ports = desired.Ports
	_, err = slices.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// labelsEqual compares the labels that make the wake slice routable — the
// kubernetes.io/service-name association key and the managed-by marker — so a
// drift in either forces a rewrite even when addresses and ports are unchanged.
func labelsEqual(a, b map[string]string) bool {
	for _, k := range []string{discoveryv1.LabelServiceName, discoveryv1.LabelManagedBy} {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}

func portsEqual(a, b []discoveryv1.EndpointPort) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name == nil || b[i].Name == nil || *a[i].Name != *b[i].Name || a[i].Port == nil || b[i].Port == nil || *a[i].Port != *b[i].Port || a[i].Protocol == nil || b[i].Protocol == nil || *a[i].Protocol != *b[i].Protocol {
			return false
		}
	}
	return true
}

func endpointsEqual(a, b []discoveryv1.Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i].Addresses) != len(b[i].Addresses) || len(a[i].Addresses) == 0 || len(b[i].Addresses) == 0 || a[i].Addresses[0] != b[i].Addresses[0] || a[i].Conditions.Ready == nil || b[i].Conditions.Ready == nil || *a[i].Conditions.Ready != *b[i].Conditions.Ready {
			return false
		}
	}
	return true
}

// DeleteWakeEndpointSlice removes the wake EndpointSlice for serviceName.
// NotFound is success so callers can treat nil as "confirmed absent" (D2).
func (c *ClientsetClient) DeleteWakeEndpointSlice(ctx context.Context, namespace, serviceName string) error {
	err := c.clientset.DiscoveryV1().EndpointSlices(namespace).Delete(ctx, wakeSliceName(serviceName), metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// ListOperatorPodIPs returns the sorted pod IPs of Ready operator pods in the
// operator namespace; these become the wake EndpointSlice addresses.
func (c *ClientsetClient) ListOperatorPodIPs(ctx context.Context, namespace string) ([]string, error) {
	pods, err := c.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: operatorPodLabel})
	if err != nil {
		return nil, err
	}
	var ips []string
	for _, pod := range pods.Items {
		ready := false
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				ready = true
				break
			}
		}
		if ready && pod.Status.PodIP != "" {
			ips = append(ips, pod.Status.PodIP)
		}
	}
	sort.Strings(ips)
	return ips, nil
}
