package v1alpha1

// Labels and annotations applied to Varroa custom resources.
const (
	// LabelManagedBy marks a User CRD's provisioning source or a generated
	// JenkinsRoleBinding's ownership. Values include ManagedByIDP (OIDC),
	// ManagedByLocal (local auth), or ManagedByOperator (varroa-operator).
	LabelManagedBy = "varroa.dev/managed-by"

	// ManagedByIDP indicates the User is provisioned by the OIDC identity
	// provider. The UI treats idp users as read-only (except delete).
	ManagedByIDP = "idp"

	// ManagedByLocal indicates the User is provisioned by Varroa local auth.
	// The UI allows full CRUD.
	ManagedByLocal = "local"

	// ManagedByOperator indicates the resource is managed by the Varroa operator.
	ManagedByOperator = "varroa-operator"

	// AnnotationOIDCSubject stores the raw OIDC subject claim on a User CRD,
	// since the CRD object name is a deterministic hash.
	AnnotationOIDCSubject = "varroa.dev/oidc-subject"

	// AnnotationOIDCPreferredUsername stores the OIDC preferred_username claim
	// on a User CRD. RoleBinding subjects may reference an OIDC user by this
	// value (the resolver's default user claims are preferred_username,sub),
	// so deprovisioning must be able to recover it. A login without the claim
	// deletes the value and moves it to AnnotationOIDCFormerIdentifiers.
	AnnotationOIDCPreferredUsername = "varroa.dev/oidc-preferred-username"

	// AnnotationOIDCFormerIdentifiers stores a JSON array of the preferred
	// usernames, emails, and replaced custom user claim values that a User
	// object held before a later login. Deprovisioning strips them from
	// bindings. The member role never reads them.
	AnnotationOIDCFormerIdentifiers = "varroa.dev/oidc-former-identifiers"

	// AnnotationOIDCUserClaims stores a JSON object of the configured user
	// claims outside sub, email, name, and preferred_username. The BFF writes
	// it at each login, and a login without the claims deletes it. The member
	// role and deprovisioning read it.
	AnnotationOIDCUserClaims = "varroa.dev/oidc-user-claims"

	// LabelBuiltin marks a VarroaRole or JenkinsRole as a built-in role
	// managed by the RoleReconciler.
	LabelBuiltin = "varroa.dev/builtin"

	// LabelControllerName marks the owning controller name on generated
	// JenkinsRoleBindings and the Controller CR itself.
	LabelControllerName = "varroa.dev/controller-name"

	// LabelControllerNamespace marks the owning controller namespace on
	// generated JenkinsRoleBindings.
	LabelControllerNamespace = "varroa.dev/controller-namespace"

	// AnnotationRolloutOverride forces a controller past the wave rollout gate.
	// Set to "true" to bypass; persists until removed by the operator.
	AnnotationRolloutOverride = "varroa.dev/rollout-override"

	// LabelTeamName marks a resource as belonging to a specific Team.
	LabelTeamName = "varroa.dev/team-name"
)
