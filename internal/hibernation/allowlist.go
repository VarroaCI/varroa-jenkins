// Package hibernation provides shared constants and types for the controller
// hibernation feature, imported by both the mite sidecar and the BFF handler.
package hibernation

// GenericTriggerPath is the replay path prefix of the generic webhook trigger
// plugin. The mite sends no Authorization header on this path.
const GenericTriggerPath = "generic-webhook-trigger/invoke"

// ReplayPathAllowlist is the set of path prefixes that the webhook replay
// endpoint (BFF ingestion and mite replay) will accept. Only paths matching
// one of these prefixes are forwarded to Jenkins.
var ReplayPathAllowlist = []string{
	"github-webhook/",
	"gitea-webhook/",
	GenericTriggerPath,
}
