package v1alpha1_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel/model"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/cel/common"
	"sigs.k8s.io/yaml"
)

// crdDir holds the generated CRD manifests the apiserver validates.
const crdDir = "../../charts/varroa/crds"

// celFixture is one testdata/cel file: a base object and the cases that
// run against it. The file name equals the CRD file name.
type celFixture struct {
	Base  map[string]interface{} `json:"base"`
	Cases []celCase              `json:"cases"`
}

// celCase is one admission check. Old is nil for a create.
type celCase struct {
	Name  string                 `json:"name"`
	Rule  string                 `json:"rule"`  // exact rule text from the CRD (whitespace-normalised)
	Field string                 `json:"field"` // e.g. "spec", "spec.pipelines.exports[0]", "" for a root rule
	Old   map[string]interface{} `json:"old"`   // JSON merge patch over base; nil = create
	New   map[string]interface{} `json:"new"`   // JSON merge patch over base
	Want  string                 `json:"want"`  // "accept" | "reject"
}

// ruleRef names one x-kubernetes-validations rule in a CRD schema.
type ruleRef struct {
	Path string
	Rule string
}

func normalize(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// loadCRD reads one CRD manifest and applies the apiserver defaults.
func loadCRD(t *testing.T, file string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(b, &crd); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("%s: want one version, got %d", file, len(crd.Spec.Versions))
	}
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(&crd)
	return &crd
}

func internalSchema(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition) *apiextensions.JSONSchemaProps {
	t.Helper()
	var in apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(crd.Spec.Versions[0].Schema.OpenAPIV3Schema, &in, nil); err != nil {
		t.Fatal(err)
	}
	return &in
}

func structural(t *testing.T, props *apiextensions.JSONSchemaProps) *structuralschema.Structural {
	t.Helper()
	s, err := structuralschema.NewStructural(props)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// collectRules lists every rule of the schema with the path it applies to.
func collectRules(s *structuralschema.Structural, path string, out *[]ruleRef) {
	for _, r := range s.XValidations {
		*out = append(*out, ruleRef{Path: path, Rule: normalize(r.Rule)})
	}
	for k, v := range s.Properties {
		p := k
		if path != "" {
			p = path + "." + k
		}
		collectRules(&v, p, out)
	}
	if s.Items != nil {
		collectRules(s.Items, path+"[*]", out)
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.Structural != nil {
		collectRules(s.AdditionalProperties.Structural, path+"[*]", out)
	}
}

// stripMessages clears each rule's message. A rule that fails then reports
// "failed rule: <rule text>", which the test compares exactly.
func stripMessages(s *structuralschema.Structural) {
	for i := range s.XValidations {
		s.XValidations[i].Message = ""
		s.XValidations[i].MessageExpression = ""
	}
	for k, v := range s.Properties {
		stripMessages(&v)
		s.Properties[k] = v
	}
	if s.Items != nil {
		stripMessages(s.Items)
	}
	if s.AdditionalProperties != nil && s.AdditionalProperties.Structural != nil {
		stripMessages(s.AdditionalProperties.Structural)
	}
}

// harness holds the apiserver validators for one CRD.
type harness struct {
	s  *structuralschema.Structural
	sv apiservervalidation.SchemaValidator
	cv *cel.Validator
}

func newHarness(t *testing.T, file string) *harness {
	t.Helper()
	crd := loadCRD(t, file)
	props := internalSchema(t, crd)
	s := structural(t, props)
	sv, _, err := apiservervalidation.NewSchemaValidator(props)
	if err != nil {
		t.Fatal(err)
	}
	stripped := structural(t, props)
	stripMessages(stripped)
	// cv is nil when the CRD has no rules; the apiserver skips CEL then too.
	return &harness{s: s, sv: sv, cv: cel.NewValidator(stripped, true, celconfig.PerCallLimit)}
}

// materialize returns base with the JSON merge patch applied. A nil patch
// returns base unchanged.
func materialize(t *testing.T, base, patch map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if patch != nil {
		p, err := json.Marshal(patch)
		if err != nil {
			t.Fatal(err)
		}
		b, err = jsonpatch.MergePatch(b, p)
		if err != nil {
			t.Fatal(err)
		}
	}
	var out map[string]interface{}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// admit runs one object through the apiserver's CR strategy: prune, default,
// schema-validate, then CEL. It returns the CEL errors only. A fixture that
// the schema rejects fails the test.
func (h *harness) admit(t *testing.T, obj, old map[string]interface{}) field.ErrorList {
	t.Helper()
	opts := structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}
	if unknown := pruning.PruneWithOptions(obj, h.s, true, opts); len(unknown) > 0 {
		t.Fatalf("fixture carries unknown fields %v", unknown)
	}
	defaulting.Default(obj, h.s)
	if old != nil {
		if unknown := pruning.PruneWithOptions(old, h.s, true, opts); len(unknown) > 0 {
			t.Fatalf("fixture carries unknown fields in old object %v", unknown)
		}
		defaulting.Default(old, h.s)
	}

	var schemaErrs field.ErrorList
	if old == nil {
		schemaErrs = apiservervalidation.ValidateCustomResource(nil, obj, h.sv)
	} else {
		schemaErrs = apiservervalidation.ValidateCustomResourceUpdate(nil, obj, old, h.sv)
	}
	if len(schemaErrs) > 0 {
		t.Fatalf("fixture is not schema-valid: %v", schemaErrs)
	}

	if h.cv == nil {
		return nil
	}
	ctx := context.Background()
	var errs field.ErrorList
	if old == nil {
		errs, _ = h.cv.Validate(ctx, nil, h.s, obj, nil, celconfig.RuntimeCELCostBudget)
	} else {
		correlation := common.NewCorrelatedObject(obj, old, &model.Structural{Structural: h.s})
		errs, _ = h.cv.Validate(ctx, nil, h.s, obj, old, celconfig.RuntimeCELCostBudget, cel.WithRatcheting(correlation))
	}
	return errs
}

func loadFixture(t *testing.T, file string) celFixture {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var fx celFixture
	if err := yaml.UnmarshalStrict(b, &fx); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	return fx
}

// fieldOf maps the apiserver's "<nil>" root path to the fixture's "".
func fieldOf(e *field.Error) string {
	if e.Field == "<nil>" {
		return ""
	}
	return e.Field
}

// TestCRDManifestsPassAPIServerValidation runs the apiserver's own CRD
// validation on every generated manifest. It catches a CEL rule that does
// not compile and a rule over the cost budget, without a cluster.
func TestCRDManifestsPassAPIServerValidation(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no CRD manifests under %s", crdDir)
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			crd := loadCRD(t, file)
			var in apiextensions.CustomResourceDefinition
			if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(crd, &in, nil); err != nil {
				t.Fatal(err)
			}
			if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &in); len(errs) > 0 {
				t.Errorf("apiserver rejects the CRD: %v", errs)
			}
		})
	}
}

// TestCRDValidationRules evaluates every fixture case against the CRD's CEL
// rules through the apiserver's validator.
func TestCRDValidationRules(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "cel", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fxFile := range fixtures {
		file := filepath.Base(fxFile)
		t.Run(file, func(t *testing.T) {
			h := newHarness(t, filepath.Join(crdDir, file))
			fx := loadFixture(t, fxFile)
			t.Run("base", func(t *testing.T) {
				if errs := h.admit(t, materialize(t, fx.Base, nil), nil); len(errs) > 0 {
					t.Fatalf("base object fails a rule on create: %v", errs)
				}
			})
			for _, c := range fx.Cases {
				t.Run(c.Name, func(t *testing.T) {
					if c.Want != "accept" && c.Want != "reject" {
						t.Fatalf("want must be accept or reject, got %q", c.Want)
					}
					newObj := materialize(t, fx.Base, c.New)
					var oldObj map[string]interface{}
					if c.Old != nil {
						oldObj = materialize(t, fx.Base, c.Old)
					}
					errs := h.admit(t, newObj, oldObj)
					for _, e := range errs {
						if strings.Contains(e.Detail, " evaluating rule: ") || strings.Contains(e.Detail, "cost") {
							t.Errorf("rule raised a runtime error; guard every optional field read with has(): %v", e)
						}
					}
					switch c.Want {
					case "accept":
						if len(errs) > 0 {
							t.Errorf("want accept, got %v", errs)
						}
					case "reject":
						// Rules can fire together on one input. The named rule
						// must fire at the named field; other errors may appear.
						want := "failed rule: " + normalize(c.Rule)
						for _, e := range errs {
							if e.Type == field.ErrorTypeInvalid && normalize(e.Detail) == want && fieldOf(e) == c.Field {
								return
							}
						}
						t.Errorf("want rejection at %q with %q, got %v", c.Field, want, errs)
					}
				})
			}
		})
	}
}

// schemaShadowed lists rules whose reject case the schema refuses first.
// A keyword such as minimum: 5 fails before CEL runs. The key names the CRD
// file and the rule text, so the exemption cannot spread to another CRD. The
// rule keeps its accept case.
var schemaShadowed = map[string]bool{
	"varroa.dev_controllers.yaml|!has(self.gracePeriodMinutes) || self.gracePeriodMinutes >= 5": true,
}

// TestCRDValidationRuleCoverage keeps the fixtures in step with the CRDs:
// every CRD with rules has a fixture, and every rule has both an accepting
// and a rejecting case.
func TestCRDValidationRuleCoverage(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	rulesByFile := map[string][]ruleRef{}
	for _, file := range files {
		crd := loadCRD(t, file)
		var rules []ruleRef
		collectRules(structural(t, internalSchema(t, crd)), "", &rules)
		if len(rules) > 0 {
			rulesByFile[filepath.Base(file)] = rules
		}
	}

	fixtures, err := filepath.Glob(filepath.Join("testdata", "cel", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureFiles := map[string]bool{}
	for _, fxFile := range fixtures {
		file := filepath.Base(fxFile)
		fixtureFiles[file] = true
		if _, err := os.Stat(filepath.Join(crdDir, file)); err != nil {
			t.Errorf("fixture %s names no CRD manifest", file)
			continue
		}
		if _, ok := rulesByFile[file]; !ok {
			t.Errorf("fixture %s names a CRD with no rules", file)
		}
	}

	for file, rules := range rulesByFile {
		if !fixtureFiles[file] {
			t.Errorf("CRD %s has %d rules and no fixture under testdata/cel", file, len(rules))
			continue
		}
		known := map[string]bool{}
		for _, r := range rules {
			known[r.Rule] = true
		}
		accept := map[string]bool{}
		reject := map[string]bool{}
		for _, c := range loadFixture(t, filepath.Join("testdata", "cel", file)).Cases {
			rule := normalize(c.Rule)
			if !known[rule] {
				t.Errorf("%s case %q names rule %q, which %s does not define", file, c.Name, rule, file)
				continue
			}
			switch c.Want {
			case "accept":
				accept[rule] = true
			case "reject":
				reject[rule] = true
			}
		}
		for _, r := range rules {
			if !accept[r.Rule] {
				t.Errorf("%s: rule %q at %q has no accept case", file, r.Rule, r.Path)
			}
			if schemaShadowed[file+"|"+r.Rule] {
				continue
			}
			if !reject[r.Rule] {
				t.Errorf("%s: rule %q at %q has no reject case", file, r.Rule, r.Path)
			}
		}
	}
}
