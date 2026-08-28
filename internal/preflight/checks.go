package preflight

import (
	"context"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/controller"
	"github.com/varroaci/varroa-jenkins/internal/controller/pluginlock"
	"github.com/varroaci/varroa-jenkins/internal/crdstore"
	"github.com/varroaci/varroa-jenkins/internal/jenkinsver"
	"github.com/varroaci/varroa-jenkins/internal/tenancy"
)

func checkName(ctx context.Context, deps Deps, draft *v1alpha1.Controller, opts Options) Check {
	errs := validation.IsDNS1123Label(draft.Name)
	if len(errs) > 0 {
		return Check{
			ID:      "name",
			Status:  "fail",
			Message: "name is not a valid DNS-1123 label: " + strings.Join(errs, "; "),
		}
	}

	existing, err := crdstore.List[v1alpha1.Controller](ctx, deps, draft.Namespace, "")
	if err != nil {
		return Check{
			ID:      "name",
			Status:  "warn",
			Message: "unable to list controllers: " + err.Error(),
		}
	}
	for _, cr := range existing {
		if cr.Name == draft.Name {
			if opts.ForUpdate {
				return Check{ID: "name", Status: "pass", Message: "name unchanged (update)"}
			}
			return Check{
				ID:      "name",
				Status:  "fail",
				Message: "a controller named " + draft.Name + " already exists in namespace " + draft.Namespace,
			}
		}
	}

	return Check{ID: "name", Status: "pass", Message: "name is available"}
}

func checkVersion(ctx context.Context, deps Deps, draft *v1alpha1.Controller, opts Options) Check {
	B0 := pluginlock.Baseline()
	version := draft.Spec.Version

	// V1: empty version — will default.
	if version == "" {
		defaults, err := crdstore.Get[v1alpha1.ProvisioningDefaults](ctx, deps, "varroa-defaults", "")
		defaultVersion := ""
		if err == nil && defaults != nil {
			defaultVersion = defaults.Spec.DefaultVersion
		}
		msg := "version will default to configured default"
		if defaultVersion != "" {
			msg = "version will default to " + defaultVersion
		}
		return Check{ID: "version", Status: "pass", Message: msg}
	}

	// V2: lts — always passes.
	if version == "lts" {
		return Check{ID: "version", Status: "pass", Message: "lts uses embedded baseline; image floats forward (safe)"}
	}

	// Parse version and determine if below baseline.
	cv, parseable := jenkinsver.Core(version)
	bv, _ := jenkinsver.Core(B0)
	belowBaseline := parseable && jenkinsver.Compare(cv, bv) < 0

	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, deps, "", "")

	var check Check

	if err != nil {
		// V8: list error.
		if belowBaseline {
			check = Check{
				ID:      "version",
				Status:  "fail",
				Message: "core " + version + " is older than the plugin baseline " + B0 + "; install a JenkinsVersionProfile for " + version,
			}
		} else {
			check = Check{
				ID:      "version",
				Status:  "warn",
				Message: "unable to list version profiles; will use embedded baseline " + B0,
			}
		}
	} else if len(profiles) == 0 {
		// V9: zero profiles installed.
		if !parseable {
			check = Check{
				ID:      "version",
				Status:  "fail",
				Message: "version " + version + " is unparseable and no version profile vouches for it; install a JenkinsVersionProfile for " + version,
			}
		} else if belowBaseline {
			check = Check{
				ID:      "version",
				Status:  "fail",
				Message: "core " + version + " is older than the plugin baseline " + B0 + "; install a JenkinsVersionProfile for " + version,
			}
		} else {
			check = Check{
				ID:      "version",
				Status:  "pass",
				Message: "no version profiles configured; will use embedded baseline " + B0,
			}
		}
	} else if p, kind := controller.ResolveProfile(version, profiles); kind != controller.MatchBaseline {
		// V3/V4: exact or line match.
		check = Check{
			ID:      "version",
			Status:  "pass",
			Message: "version " + version + " matches profile " + p.Spec.Version,
		}
	} else {
		// No match, profiles exist.
		if !parseable {
			// V7: unparseable.
			check = Check{
				ID:      "version",
				Status:  "fail",
				Message: "version " + version + " is unparseable and no version profile vouches for it; install a JenkinsVersionProfile for " + version,
			}
		} else if belowBaseline {
			// V6: below baseline.
			check = Check{
				ID:      "version",
				Status:  "fail",
				Message: "core " + version + " is older than the plugin baseline " + B0 + "; install a JenkinsVersionProfile for " + version,
			}
		} else {
			// V5: unmatched >= baseline.
			check = Check{
				ID:      "version",
				Status:  "warn",
				Message: "no version profile for " + version + "; will use embedded baseline " + B0,
			}
		}
	}

	return applyUpdateLeniency(check, opts, version)
}

func checkBundle(ctx context.Context, deps Deps, draft *v1alpha1.Controller, inlineBundle *v1alpha1.ComposedBundleSpec, opts Options) Check {
	if inlineBundle != nil {
		if len(inlineBundle.Inputs) == 0 {
			return Check{
				ID:      "bundle",
				Status:  "fail",
				Message: "inline bundle has no inputs",
			}
		}
		for _, input := range inlineBundle.Inputs {
			if input.ItemRef != nil && input.GitSource != nil {
				return Check{
					ID:      "bundle",
					Status:  "fail",
					Message: "inline bundle input has both itemRef and gitSource (must be exactly one)",
				}
			}
			if input.ItemRef == nil && input.GitSource == nil {
				return Check{
					ID:      "bundle",
					Status:  "fail",
					Message: "inline bundle input has neither itemRef nor gitSource",
				}
			}
			if input.ItemRef != nil && input.ItemRef.Name == "" {
				return Check{
					ID:      "bundle",
					Status:  "fail",
					Message: "inline bundle itemRef has empty name",
				}
			}
			if input.GitSource != nil {
				if input.GitSource.RepoURL == "" {
					return Check{
						ID:      "bundle",
						Status:  "fail",
						Message: "inline bundle gitSource has empty repoURL",
					}
				}
				if input.GitSource.Path == "" {
					return Check{
						ID:      "bundle",
						Status:  "fail",
						Message: "inline bundle gitSource has empty path",
					}
				}
			}
		}
		return Check{ID: "bundle", Status: "pass", Message: "inline bundle provided"}
	}
	// A nil composedBundleRef is not "no bundle": the controller will use the
	// built-in starter. Reporting a bare pass here would hide the one thing that
	// can actually go wrong on the zero-config path — an operator that has not
	// finished seeding the starter, or a starter whose compose failed.
	named := draft.Spec.ComposedBundleRef != nil
	bundleName, lookupNS := v1alpha1.EffectiveBundleRef(draft, opts.OperatorNamespace)
	label := "referenced bundle " + bundleName
	if !named {
		label = "built-in starter bundle " + lookupNS + "/" + bundleName
	}

	cb, err := crdstore.Get[v1alpha1.ComposedBundle](ctx, deps, bundleName, lookupNS)
	if err != nil {
		msg := label + " not found in namespace " + lookupNS + " (may exist in another namespace)"
		if !named {
			msg = label + " is not available yet; the operator seeds it shortly after startup"
		}
		return Check{ID: "bundle", Status: "warn", Message: msg}
	}
	if cb.Status.Phase != v1alpha1.ComposedBundleReady || cb.Status.ResolvedHash == "" {
		return Check{ID: "bundle", Status: "warn", Message: label + " is not Ready"}
	}
	if !named {
		return Check{ID: "bundle", Status: "pass", Message: "using the built-in starter bundle"}
	}
	return Check{ID: "bundle", Status: "pass", Message: "referenced bundle is ready"}
}

func checkPluginCoreCompat(ctx context.Context, deps Deps, draft *v1alpha1.Controller, opts Options) Check {
	B0 := pluginlock.Baseline()
	version := draft.Spec.Version

	// P1: floating tags.
	if version == "" || version == "lts" {
		return Check{ID: "pluginCoreCompat", Status: "pass", Message: "floats forward; compatible"}
	}

	profiles, err := crdstore.List[v1alpha1.JenkinsVersionProfile](ctx, deps, "", "")

	if err != nil {
		// P8: list error.
		cv, parseable := jenkinsver.Core(version)
		bv, _ := jenkinsver.Core(B0)
		var c Check
		if parseable && jenkinsver.Compare(cv, bv) < 0 {
			c = Check{
				ID:      "pluginCoreCompat",
				Status:  "fail",
				Message: "core " + version + " is older than the plugin baseline " + B0 + "; install a JenkinsVersionProfile for " + version,
			}
		} else {
			c = Check{
				ID:      "pluginCoreCompat",
				Status:  "warn",
				Message: "unable to list version profiles; assuming embedded baseline " + B0,
			}
		}
		return applyUpdateLeniency(c, opts, version)
	}

	profile, kind := controller.ResolveProfile(version, profiles)
	ready := controller.ProfilePluginSetReady(profile)
	res := controller.EvaluateCoreCompat(version, profile, kind, ready, B0)

	var c Check
	switch res.Verdict {
	case controller.CompatOK:
		if profile != nil && kind != controller.MatchBaseline && !ready {
			// P3: matched but not materialized.
			c = Check{
				ID:      "pluginCoreCompat",
				Status:  "warn",
				Message: "profile matched but plugin set not materialized; baseline in use",
			}
		} else {
			// P2/P5: compatible.
			c = Check{
				ID:      "pluginCoreCompat",
				Status:  "pass",
				Message: "effective plugin set is compatible with core " + version,
			}
		}
	case controller.CompatUnsafe:
		// P4/P6.
		c = Check{
			ID:      "pluginCoreCompat",
			Status:  "fail",
			Message: res.Message,
		}
	case controller.CompatUnknown:
		// P7.
		c = Check{
			ID:      "pluginCoreCompat",
			Status:  "warn",
			Message: "core " + version + " is indeterminate for compat; see version check",
		}
	}

	return applyUpdateLeniency(c, opts, version)
}

// applyUpdateLeniency downgrades a fail to warn when the version is unchanged
// by an update operation.
func applyUpdateLeniency(c Check, opts Options, version string) Check {
	if opts.ForUpdate && version == opts.PriorVersion && c.Status == "fail" {
		c.Status = "warn"
		c.Message += " (pre-existing version, unchanged by this update)"
	}
	return c
}

func checkTargetNamespace(ctx context.Context, deps Deps, draft *v1alpha1.Controller, opts Options, set tenancy.ManagedSet) Check {
	state, err := tenancy.Classify(ctx, deps, set, draft.Namespace) // deps satisfies NamespaceReader
	if err != nil {
		return Check{ID: "namespace", Status: "warn", Message: "unable to check namespace " + draft.Namespace + ": " + err.Error()}
	}
	switch state {
	case tenancy.NamespaceReady:
		return Check{ID: "namespace", Status: "pass", Message: "namespace " + draft.Namespace + " is ready"}
	case tenancy.NamespaceMissing:
		st := failOrWarn(opts.ForUpdate)
		return Check{ID: "namespace", Status: st, Message: "TargetNamespaceMissing: namespace " + draft.Namespace + " does not exist — create it first"}
	case tenancy.NamespaceUnmanaged:
		st := failOrWarn(opts.ForUpdate)
		return Check{ID: "namespace", Status: st, Message: "TargetNamespaceUnmanaged: namespace " + draft.Namespace + " is not in managedNamespaces — add it and run `helm upgrade`"}
	}
	return Check{ID: "namespace", Status: "warn", Message: "unknown namespace state"}
}

// failOrWarn returns "warn" on update (don't block an edit) else "fail".
func failOrWarn(forUpdate bool) string {
	if forUpdate {
		return "warn"
	}
	return "fail"
}
