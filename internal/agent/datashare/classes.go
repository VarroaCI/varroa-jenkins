package datashare

// DataClass is the data class of one read tool. The class decides the level
// that the tool's result needs.
type DataClass string

// The data classes.
const (
	ClassMetadata         DataClass = "metadata"
	ClassLogs             DataClass = "logs"
	ClassMetadataFiltered DataClass = "metadata-filtered"
)

// classes maps every live read tool to its data class. A read tool that is
// missing here is treated as unclassified and needs the logs level.
var classes = map[string]DataClass{
	"get_run_log":                  ClassLogs,
	"get_team_cell":                ClassMetadata,
	"read_repo_file":               ClassMetadata,
	"get_controller_logs":          ClassLogs,
	"get_run":                      ClassMetadataFiltered,
	"list_controllers":             ClassMetadata,
	"get_controller":               ClassMetadata,
	"list_varroa_roles":            ClassMetadata,
	"get_varroa_role":              ClassMetadata,
	"list_varroa_role_bindings":    ClassMetadata,
	"get_varroa_role_binding":      ClassMetadata,
	"list_jenkins_roles":           ClassMetadata,
	"get_jenkins_role":             ClassMetadata,
	"list_jenkins_role_bindings":   ClassMetadata,
	"get_jenkins_role_binding":     ClassMetadata,
	"list_catalog_sources":         ClassMetadata,
	"get_catalog_source":           ClassMetadata,
	"list_catalog_items":           ClassMetadata,
	"get_catalog_item":             ClassMetadata,
	"list_composed_bundles":        ClassMetadata,
	"get_composed_bundle":          ClassMetadata,
	"validate_composed_bundle":     ClassMetadata,
	"preview_composed_bundle":      ClassMetadata,
	"get_provisioning_defaults":    ClassMetadata,
	"list_jenkins_version_profile": ClassMetadata,
	"get_jenkins_version_profile":  ClassMetadata,
	"list_version_candidates":      ClassMetadata,
	"get_version_candidate":        ClassMetadata,
	"list_users":                   ClassMetadata,
	"get_user":                     ClassMetadata,
	"list_groups":                  ClassMetadata,
	"list_activity":                ClassMetadata,
	"search":                       ClassMetadata,
	"get_me":                       ClassMetadata,
	"get_my_permissions":           ClassMetadata,
	"list_jenkins_controllers":     ClassMetadata,
	"list_runs":                    ClassMetadata,
	"list_job_branches":            ClassMetadata,
	"list_fleet_locks":             ClassMetadata,
	"list_cell_pools":              ClassMetadata,
	"get_cell_pool":                ClassMetadata,
	"get_cell_usage":               ClassMetadata,
}

// ClassOf returns the data class of a read tool, and false when the tool
// has no class.
func ClassOf(tool string) (DataClass, bool) {
	c, ok := classes[tool]
	return c, ok
}
