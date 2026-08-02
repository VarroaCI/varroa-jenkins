package rbac

// MiteMinimalPermissions returns the least-privilege Jenkins permission set the
// varroa:system-mite role needs. After PR #172 the mite applies JCasC via the
// MANAGE-gated /configuration-as-code/reload endpoint (write files + reload), so
// it no longer requires Hudson.Administer.
//
//   - Manage      -> /configuration-as-code/reload, quietDown
//   - SystemRead  -> /configuration-as-code/export, snapshots
//
// SystemRead is opt-in: it only takes effect when the controller JVM runs with
// -Djenkins.security.SystemReadPermission=true (set in JAVA_OPTS by the
// StatefulSet, see internal/controller/clientset_client.go). Without it the
// permission is disabled and this grant is inert, 403ing the export (#189).
//   - Read        -> Overall/Read (info endpoints)
//   - Item.*      -> item CRUD the mite performs on behalf of the bundle
//   - View.*      -> view CRUD
//
// Plugin install (/pluginManager/installNecessaryPlugins) and safeRestart remain
// ADMINISTER-only in upstream Jenkins; those are being moved off the mite's
// request path (the #166 follow-ups) rather than granted here.
func MiteMinimalPermissions() []string {
	return []string{
		"hudson.model.Hudson.Read",
		"hudson.model.Hudson.Manage",
		"hudson.model.Hudson.SystemRead",
		"hudson.model.Item.Read",
		"hudson.model.Item.Discover",
		"hudson.model.Item.Configure",
		"hudson.model.Item.Create",
		"hudson.model.Item.Delete",
		"hudson.model.View.Read",
		"hudson.model.View.Configure",
		"hudson.model.View.Create",
		"hudson.model.View.Delete",
	}
}
