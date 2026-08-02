// Package hibernation provides shared constants and types for the controller
// hibernation feature, imported by both the mite sidecar and the BFF handler.
package hibernation

// ReplayPathAllowlist is the set of path prefixes that the webhook replay
// endpoint (BFF ingestion and mite replay) will accept. Only paths matching
// one of these prefixes are forwarded to Jenkins.
var ReplayPathAllowlist = []string{
	"github-webhook/",
	"generic-webhook-trigger/invoke",
}
