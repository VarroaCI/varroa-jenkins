package api

import (
	"net/http"
)

func init() {
	// ── ops (2 cases) ─────────────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "getHealthz", Method: "GET", Path: "/healthz", WantStatus: http.StatusOK},
		contractCase{Name: "getVersion", Method: "GET", Path: "/version", WantStatus: http.StatusOK},
	)

	// ── auth & session (7 cases) ─────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "getAuthConfig", Method: "GET", Path: "/api/v1/auth-config", WantStatus: http.StatusOK},
		contractCase{Name: "login-501", Method: "POST", Path: "/api/v1/login", Body: map[string]string{"username": "admin", "password": "pass"}, WantStatus: http.StatusNotImplemented},
		contractCase{Name: "logout", Method: "POST", Path: "/api/v1/logout", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getMe", Method: "GET", Path: "/api/v1/me", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateMyPreferences", Method: "PUT", Path: "/api/v1/me/preferences", Claims: adminClaims, Body: map[string]string{}, WantStatus: http.StatusOK},
		contractCase{Name: "changeMyPassword-501", Method: "PUT", Path: "/api/v1/me/password", Claims: adminClaims, Body: map[string]string{"oldPassword": "x", "newPassword": "short"}, WantStatus: http.StatusNotImplemented},
		contractCase{Name: "getMyPermissions", Method: "GET", Path: "/api/v1/me/permissions", Claims: adminClaims, WantStatus: http.StatusOK},
	)

	// ── users (5 cases) ──────────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listUsers", Method: "GET", Path: "/api/v1/users", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "listUsers-403-nonadmin", Method: "GET", Path: "/api/v1/users", Claims: operatorClaims, WantStatus: http.StatusForbidden},
		contractCase{Name: "createUser", Method: "POST", Path: "/api/v1/users", Claims: adminClaims, Body: map[string]string{"username": "newuser", "password": "password123"}, WantStatus: http.StatusCreated},
		contractCase{Name: "deleteUser-404", Method: "DELETE", Path: "/api/v1/users/nonexistent", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "setUserPassword-501", Method: "PUT", Path: "/api/v1/users/testuser/password", Claims: adminClaims, Body: map[string]string{"newPassword": "password123"}, WantStatus: http.StatusNotImplemented},
		contractCase{Name: "updateUser-404", Method: "PUT", Path: "/api/v1/users/nonexistent", Claims: adminClaims, Body: map[string]string{"email": "test@test.com"}, WantStatus: http.StatusNotFound},
	)

	// ── identity (3 cases) ───────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "getIdentitySettings", Method: "GET", Path: "/api/v1/identity-settings", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getIdentitySettings-403-nonadmin", Method: "GET", Path: "/api/v1/identity-settings", Claims: operatorClaims, WantStatus: http.StatusForbidden},
		contractCase{Name: "listBuiltinRoles", Method: "GET", Path: "/api/v1/builtin-roles", Claims: adminClaims, WantStatus: http.StatusOK},
	)

	// ── groups (3 cases) ─────────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listGroups", Method: "GET", Path: "/api/v1/groups", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "listGroups-403-nonadmin", Method: "GET", Path: "/api/v1/groups", Claims: operatorClaims, WantStatus: http.StatusForbidden},
		contractCase{Name: "createGroup", Method: "POST", Path: "/api/v1/groups", Claims: adminClaims, Body: map[string]string{"name": "test-group"}, WantStatus: http.StatusCreated},
		contractCase{Name: "zz-deleteGroup", Method: "DELETE", Path: "/api/v1/groups/any-group", Claims: adminClaims, WantStatus: http.StatusNoContent},
	)

	// ── teams (5 cases) ──────────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listTeams", Method: "GET", Path: "/api/v1/teams", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "listTeams-403-nonadmin", Method: "GET", Path: "/api/v1/teams", Claims: operatorClaims, WantStatus: http.StatusForbidden},
		contractCase{Name: "createTeam-400-validation", Method: "POST", Path: "/api/v1/teams", Claims: adminClaims, Body: map[string]interface{}{"name": "test-team", "namespaces": []string{"ns1"}}, WantStatus: http.StatusBadRequest},
		contractCase{Name: "zz-deleteTeam", Method: "DELETE", Path: "/api/v1/teams/any-team", Claims: adminClaims, WantStatus: http.StatusNoContent},
		contractCase{Name: "getTeam-404", Method: "GET", Path: "/api/v1/teams/nonexistent", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "updateTeam", Method: "PUT", Path: "/api/v1/teams/new-team", Claims: adminClaims, Body: map[string]interface{}{"name": "new-team", "members": []string{"user"}, "namespaces": []string{"ns"}}, WantStatus: http.StatusOK},
	)

	// ── controllers (19 cases) ───────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listControllers", Method: "GET", Path: "/api/v1/controllers", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "listClusters", Method: "GET", Path: "/api/v1/clusters", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getController", Method: "GET", Path: "/api/v1/clusters/core/controllers/test-ns/test-ctrl", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "deleteController-200", Method: "DELETE", Path: "/api/v1/clusters/core/controllers/test-ns/any-ctrl", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "patchController-200", Method: "PATCH", Path: "/api/v1/clusters/core/controllers/test-ns/test-ctrl", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"version": "2.0"}}, WantStatus: http.StatusOK},
		contractCase{Name: "createController-400", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns", Claims: adminClaims, Body: map[string]interface{}{}, WantStatus: http.StatusBadRequest},
		contractCase{Name: "preflightController-400", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/preflight", Claims: adminClaims, Body: map[string]interface{}{}, WantStatus: http.StatusBadRequest},
		contractCase{Name: "renderController-400", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/render", Claims: adminClaims, Body: map[string]interface{}{}, WantStatus: http.StatusBadRequest},
		contractCase{Name: "getControllerYaml-404", Method: "GET", Path: "/api/v1/clusters/core/controllers/test-ns/missing/yaml", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "reconcileController-503", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/missing/reconcile", Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "approveController-503", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/missing/approve", Claims: adminClaims, Body: map[string]string{"action": "restart"}, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "approveControllerDeletion-503", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/missing/approve-deletion", Claims: adminClaims, Body: map[string]string{"path": "some/path"}, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "reprovisionController-503", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/missing/reprovision", Claims: adminClaims, WantStatus: http.StatusServiceUnavailable},
		contractCase{Name: "restartController-202", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/missing/restart", Claims: adminClaims, WantStatus: http.StatusAccepted},
		contractCase{Name: "previewController-200", Method: "POST", Path: "/api/v1/clusters/core/controllers/test-ns/missing/preview", Claims: adminClaims, Body: map[string]string{"baseline": "base"}, WantStatus: http.StatusOK},
		contractCase{Name: "getControllerLogs-200", Method: "GET", Path: "/api/v1/clusters/core/controllers/test-ns/missing/logs", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getControllerDiff-404", Method: "GET", Path: "/api/v1/clusters/core/controllers/test-ns/missing/diff", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "getControllerPlugins-404", Method: "GET", Path: "/api/v1/clusters/core/controllers/test-ns/missing/plugins", Claims: adminClaims, WantStatus: http.StatusNotFound},
		// Drain operations
		contractCase{Name: "drainCluster-400-core", Method: "POST", Path: "/api/v1/clusters/core/drain", Claims: adminClaims, Body: map[string]string{"confirm": "core"}, WantStatus: http.StatusBadRequest},
		contractCase{Name: "drainCluster-403-nonadmin", Method: "POST", Path: "/api/v1/clusters/dev-cluster/drain", Claims: operatorClaims, Body: map[string]string{"confirm": "dev-cluster"}, WantStatus: http.StatusForbidden},
		contractCase{Name: "cancelClusterDrain-403-nonadmin", Method: "DELETE", Path: "/api/v1/clusters/dev-cluster/drain", Claims: operatorClaims, WantStatus: http.StatusForbidden},
		contractCase{Name: "cancelClusterDrain-409-active", Method: "DELETE", Path: "/api/v1/clusters/dev-cluster/drain", Claims: adminClaims, WantStatus: http.StatusConflict},
	)

	// ── RBAC (16 cases) ──────────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listVarroaRoles", Method: "GET", Path: "/api/v1/roles", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createVarroaRole", Method: "POST", Path: "/api/v1/roles", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"apiRules": []map[string]interface{}{{"resources": []string{"*"}, "verbs": []string{"*"}}}}}, WantStatus: http.StatusCreated},
		contractCase{Name: "getVarroaRole", Method: "GET", Path: "/api/v1/roles/admin", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateVarroaRole", Method: "PUT", Path: "/api/v1/roles/admin", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"jenkinsPermissions": []string{"Overall.Administer"}}}, WantStatus: http.StatusOK},
		contractCase{Name: "zz-deleteVarroaRole", Method: "DELETE", Path: "/api/v1/roles/unknown", Claims: adminClaims, WantStatus: http.StatusNoContent},
		contractCase{Name: "listVarroaRoleBindings", Method: "GET", Path: "/api/v1/rolebindings", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createVarroaRoleBinding", Method: "POST", Path: "/api/v1/rolebindings", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"subjects": []map[string]string{{"kind": "User", "name": "test"}}, "roleRef": "admin"}}, WantStatus: http.StatusCreated},
		contractCase{Name: "getVarroaRoleBinding", Method: "GET", Path: "/api/v1/rolebindings/admin-binding", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateVarroaRoleBinding", Method: "PUT", Path: "/api/v1/rolebindings/admin-binding", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"subjects": []map[string]string{{"kind": "User", "name": "test"}}, "roleRef": "operator"}}, WantStatus: http.StatusOK},
		contractCase{Name: "zz-deleteVarroaRoleBinding", Method: "DELETE", Path: "/api/v1/rolebindings/unknown", Claims: adminClaims, WantStatus: http.StatusNoContent},
		contractCase{Name: "listJenkinsRoles", Method: "GET", Path: "/api/v1/clusters/core/jenkinsroles", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createJenkinsRole", Method: "POST", Path: "/api/v1/clusters/core/jenkinsroles", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"permissions": []string{"Overall.Administer"}}}, WantStatus: http.StatusCreated},
		contractCase{Name: "getJenkinsRole", Method: "GET", Path: "/api/v1/clusters/core/jenkinsroles/global-admin", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateJenkinsRole", Method: "PUT", Path: "/api/v1/clusters/core/jenkinsroles/global-admin", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"permissions": []string{"Overall.Administer"}, "description": "updated"}}, WantStatus: http.StatusOK},
		contractCase{Name: "zz-deleteJenkinsRole", Method: "DELETE", Path: "/api/v1/clusters/core/jenkinsroles/unknown", Claims: adminClaims, WantStatus: http.StatusNoContent},
		contractCase{Name: "listJenkinsRoleBindings", Method: "GET", Path: "/api/v1/clusters/core/jenkinsrolebindings", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createJenkinsRoleBinding", Method: "POST", Path: "/api/v1/clusters/core/jenkinsrolebindings", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"subjects": []map[string]string{{"kind": "User", "name": "test"}}, "roleRef": "admin"}}, WantStatus: http.StatusCreated},
		contractCase{Name: "getJenkinsRoleBinding", Method: "GET", Path: "/api/v1/clusters/core/jenkinsrolebindings/global-admin-binding", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateJenkinsRoleBinding", Method: "PUT", Path: "/api/v1/clusters/core/jenkinsrolebindings/global-admin-binding", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"subjects": []map[string]string{{"kind": "User", "name": "test"}}, "roleRef": "viewer"}}, WantStatus: http.StatusOK},
		contractCase{Name: "zz-deleteJenkinsRoleBinding", Method: "DELETE", Path: "/api/v1/clusters/core/jenkinsrolebindings/unknown", Claims: adminClaims, WantStatus: http.StatusNoContent},
	)

	// ── catalog (6 cases) ────────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listCatalogSources", Method: "GET", Path: "/api/v1/clusters/core/catalogsources", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "listCatalogItems", Method: "GET", Path: "/api/v1/clusters/core/catalogitems", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getCatalogItem", Method: "GET", Path: "/api/v1/clusters/core/catalogitems/test-ns/test-item", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getCatalogSource", Method: "GET", Path: "/api/v1/clusters/core/catalogsources/test-ns/test-src", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createCatalogSource", Method: "POST", Path: "/api/v1/clusters/core/catalogsources/test-ns", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"repoURL": "https://example.com/repo", "path": "catalog"}}, WantStatus: http.StatusCreated},
		contractCase{Name: "zz-deleteCatalogSource", Method: "DELETE", Path: "/api/v1/clusters/core/catalogsources/test-ns/test-src", Claims: adminClaims, WantStatus: http.StatusNoContent},
		contractCase{Name: "updateCatalogSource", Method: "PUT", Path: "/api/v1/clusters/core/catalogsources/test-ns/test-src", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"repoURL": "https://example.com/repo", "path": "new-path"}}, WantStatus: http.StatusOK},
		contractCase{Name: "syncCatalogSource-202", Method: "POST", Path: "/api/v1/clusters/core/catalogsources/test-ns/test-src/sync", Claims: adminClaims, WantStatus: http.StatusAccepted},
		contractCase{Name: "syncCatalogSource-colon-405", Method: "POST", Path: "/api/v1/clusters/core/catalogsources/test-ns/test-src:sync", Claims: adminClaims, WantStatus: http.StatusMethodNotAllowed, SkipSpecValidation: true},
	)

	// ── composed bundles (9 cases) ───────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listComposedBundles", Method: "GET", Path: "/api/v1/clusters/core/composedbundles", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createComposedBundle", Method: "POST", Path: "/api/v1/clusters/core/composedbundles/test-ns", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"inputs": []map[string]interface{}{}}}, WantStatus: http.StatusCreated},
		contractCase{Name: "getComposedBundle", Method: "GET", Path: "/api/v1/clusters/core/composedbundles/test-ns/test-bundle", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateComposedBundle", Method: "PUT", Path: "/api/v1/clusters/core/composedbundles/test-ns/test-bundle", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{"inputs": []map[string]interface{}{}, "displayName": "updated bundle"}}, WantStatus: http.StatusOK},
		contractCase{Name: "zz-deleteComposedBundle", Method: "DELETE", Path: "/api/v1/clusters/core/composedbundles/test-ns/unknown", Claims: adminClaims, WantStatus: http.StatusNoContent},
		contractCase{Name: "pauseComposedBundle-404", Method: "POST", Path: "/api/v1/clusters/core/composedbundles/test-ns/missing/pause", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "resumeComposedBundle-404", Method: "POST", Path: "/api/v1/clusters/core/composedbundles/test-ns/missing/resume", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "validateComposedBundle-503", Method: "POST", Path: "/api/v1/clusters/core/composedbundles/validate?namespace=test-ns", Claims: adminClaims, Body: map[string]interface{}{"inputs": []map[string]interface{}{}}, WantStatus: http.StatusOK},
		contractCase{Name: "previewComposedBundle-503", Method: "POST", Path: "/api/v1/clusters/core/composedbundles/test-ns/preview", Claims: adminClaims, Body: map[string]interface{}{"inputs": []map[string]interface{}{}}, WantStatus: http.StatusOK},
	)

	// ── provisioning (5 cases) ───────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "getProvisioningConfig", Method: "GET", Path: "/api/v1/clusters/core/provisioning/config", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "listDeployableNamespaces", Method: "GET", Path: "/api/v1/clusters/core/namespaces/deployable", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "listVersionProfiles", Method: "GET", Path: "/api/v1/clusters/core/version-profiles", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createVersionProfile", Method: "POST", Path: "/api/v1/clusters/core/version-profiles", Claims: adminClaims, Body: map[string]interface{}{"apiVersion": "varroa.dev/v1alpha1", "kind": "JenkinsVersionProfile", "metadata": map[string]interface{}{"name": "test-profile"}, "spec": map[string]interface{}{}}, WantStatus: http.StatusCreated},
		contractCase{Name: "getVersionProfile", Method: "GET", Path: "/api/v1/clusters/core/version-profiles/test-profile", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateVersionProfile", Method: "PUT", Path: "/api/v1/clusters/core/version-profiles/test-profile", Claims: adminClaims, Body: map[string]interface{}{"apiVersion": "varroa.dev/v1alpha1", "kind": "JenkinsVersionProfile", "metadata": map[string]interface{}{"name": "test-profile"}, "spec": map[string]interface{}{}}, WantStatus: http.StatusOK},
		contractCase{Name: "deleteVersionProfile", Method: "DELETE", Path: "/api/v1/clusters/core/version-profiles/test-profile", Claims: adminClaims, WantStatus: http.StatusNoContent},
		contractCase{Name: "listControllerClasses", Method: "GET", Path: "/api/v1/clusters/core/controller-classes", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getControllerClass", Method: "GET", Path: "/api/v1/clusters/core/controller-classes/test-class", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "createControllerClass", Method: "POST", Path: "/api/v1/clusters/core/controller-classes", Claims: adminClaims, SkipSpecValidation: true, WantStatus: http.StatusMethodNotAllowed},
		contractCase{Name: "putControllerClass", Method: "PUT", Path: "/api/v1/clusters/core/controller-classes/test-class", Claims: adminClaims, SkipSpecValidation: true, WantStatus: http.StatusMethodNotAllowed},
		contractCase{Name: "deleteControllerClass", Method: "DELETE", Path: "/api/v1/clusters/core/controller-classes/test-class", Claims: adminClaims, SkipSpecValidation: true, WantStatus: http.StatusMethodNotAllowed},
		contractCase{Name: "getProvisioningDefaults", Method: "GET", Path: "/api/v1/clusters/core/provisioningdefaults/varroa-defaults", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "updateProvisioningDefaults", Method: "PUT", Path: "/api/v1/clusters/core/provisioningdefaults/varroa-defaults", Claims: adminClaims, Body: map[string]interface{}{"spec": map[string]interface{}{}}, WantStatus: http.StatusOK},
	)

	// ── activity + streams (5 cases) ─────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listActivity", Method: "GET", Path: "/api/v1/activity", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "search", Method: "GET", Path: "/api/v1/search?q=test", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createStreamTicket", Method: "POST", Path: "/api/v1/stream/ticket", Claims: adminClaims, Body: map[string]string{"scope": "brood"}, WantStatus: http.StatusOK},
	)

	// ── brood operations (7 cases) ──────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listBroodOps", Method: "GET", Path: "/api/v1/brood-operations", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createBroodOp-400-tenancy", Method: "POST", Path: "/api/v1/brood-operations", Claims: adminClaims, Body: map[string]interface{}{"namespace": "team-ns", "spec": map[string]interface{}{"action": map[string]string{"verb": "reconcile"}, "targets": map[string]interface{}{"names": []string{"ctrl-a"}, "namespaces": []string{"other-ns"}}}}, WantStatus: http.StatusBadRequest},
		contractCase{Name: "previewBroodOp", Method: "POST", Path: "/api/v1/brood-operations/preview", Claims: adminClaims, Body: map[string]interface{}{"namespace": "team-ns", "spec": map[string]interface{}{"action": map[string]string{"verb": "reconcile"}, "targets": map[string]interface{}{"selector": map[string]interface{}{}}}}, WantStatus: http.StatusOK},
		contractCase{Name: "getBroodOp-404", Method: "GET", Path: "/api/v1/brood-operations/test-ns/missing", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "deleteBroodOp-404", Method: "DELETE", Path: "/api/v1/brood-operations/test-ns/missing", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "suspendBroodOp-404", Method: "POST", Path: "/api/v1/brood-operations/test-ns/missing/suspend", Claims: adminClaims, Body: map[string]bool{"suspend": true}, WantStatus: http.StatusNotFound},
		contractCase{Name: "streamBroodOp-404", Method: "GET", Path: "/api/v1/brood-operations/test-ns/missing/stream", Claims: adminClaims, WantStatus: http.StatusNotFound},
	)

	// ── brood schedules (5 cases) ──────────────────────────────────────
	registerContractCases(
		contractCase{Name: "createBroodSchedule", Method: "POST", Path: "/api/v1/brood-schedules", Claims: adminClaims, Body: map[string]interface{}{"namespace": "team-ns", "name": "test-sched", "spec": map[string]interface{}{"schedule": "*/5 * * * *", "template": map[string]interface{}{"targets": map[string]interface{}{"names": []string{"ctrl-1"}}, "action": map[string]interface{}{"verb": "reconcile"}}}}, WantStatus: http.StatusCreated},
		contractCase{Name: "listBroodSchedules", Method: "GET", Path: "/api/v1/brood-schedules", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "getBroodSchedule", Method: "GET", Path: "/api/v1/brood-schedules/test-ns/test-sched", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "deleteBroodSchedule", Method: "DELETE", Path: "/api/v1/brood-schedules/test-ns/test-sched", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "suspendBroodSchedule", Method: "POST", Path: "/api/v1/brood-schedules/test-ns/test-sched/suspend", Claims: adminClaims, Body: map[string]bool{"suspend": true}, WantStatus: http.StatusOK},
	)

	// ── apikeys (6 cases) ────────────────────────────────────────────────
	registerContractCases(
		contractCase{Name: "listMyApiKeys", Method: "GET", Path: "/api/v1/me/apikeys", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "createApiKey", Method: "POST", Path: "/api/v1/me/apikeys", Claims: adminClaims, WantStatus: http.StatusCreated},
		contractCase{Name: "revokeApiKey-404", Method: "DELETE", Path: "/api/v1/me/apikeys/abcdefg", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "rotateApiKey-404", Method: "POST", Path: "/api/v1/me/apikeys/abcdefg/rotate", Claims: adminClaims, WantStatus: http.StatusNotFound},
		contractCase{Name: "listUserApiKeys", Method: "GET", Path: "/api/v1/users/admin/apikeys", Claims: adminClaims, WantStatus: http.StatusOK},
		contractCase{Name: "revokeUserApiKey-404", Method: "DELETE", Path: "/api/v1/users/admin/apikeys/abcdefg", Claims: adminClaims, WantStatus: http.StatusNotFound},
	)
}
