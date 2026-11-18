package v1alpha1_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/listtype"
	schemaobjectmeta "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/objectmeta"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metavalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// localdevManifestDir holds the manifests that hack/localdev/localdev.sh applies.
// The kubeconform step of the Checks job skips their Varroa kinds. This test is the offline check of those kinds.
const localdevManifestDir = "../../hack/localdev/manifests"

// varroaAPIGroup is the API group of every Varroa kind.
const varroaAPIGroup = "varroa.dev"

// crdValidator holds the apiserver validators of one generated CRD.
type crdValidator struct {
	namespaced bool
	s          *structuralschema.Structural
	sv         apiservervalidation.SchemaValidator
	cv         *cel.Validator
}

func newCRDValidator(t *testing.T, file string) (string, *crdValidator) {
	t.Helper()
	crd := loadCRD(t, file)
	props := internalSchema(t, crd)
	sv, _, err := apiservervalidation.NewSchemaValidator(props)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	s := structural(t, props)
	key := crd.Spec.Group + "/" + crd.Spec.Versions[0].Name + "/" + crd.Spec.Names.Kind
	return key, &crdValidator{
		namespaced: crd.Spec.Scope == apiextensionsv1.NamespaceScoped,
		s:          s,
		sv:         sv,
		// cv is nil when the CRD has no rules. The apiserver skips CEL then too.
		cv: cel.NewValidator(s, true, celconfig.PerCallLimit),
	}
}

// crdValidators maps "<group>/<version>/<kind>" to the validator of each generated CRD.
// The key has the apiVersion/Kind form of the kubeconform -skip list.
func crdValidators(t *testing.T) map[string]*crdValidator {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(crdDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no CRD manifests under %s", crdDir)
	}
	out := make(map[string]*crdValidator, len(files))
	for _, file := range files {
		key, v := newCRDValidator(t, file)
		out[key] = v
	}
	return out
}

// validateCreate runs obj through the create path of the apiserver custom
// resource strategy: prune, default, object metadata, schema, embedded
// metadata, list types, then CEL. kubectl applies with strict field
// validation, so a pruned field is a problem too. The function changes obj in
// place. It returns one line for each problem.
func (v *crdValidator) validateCreate(obj map[string]interface{}) []string {
	opts := structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}
	unknown := pruning.PruneWithOptions(obj, v.s, true, opts)
	defaulting.Default(obj, v.s)

	u := &unstructured.Unstructured{Object: obj}
	var errs field.ErrorList
	errs = append(errs, metavalidation.ValidateObjectMetaAccessor(u, v.namespaced, metavalidation.NameIsDNSSubdomain, field.NewPath("metadata"))...)
	errs = append(errs, apiservervalidation.ValidateCustomResource(nil, obj, v.sv)...)
	errs = append(errs, schemaobjectmeta.Validate(nil, obj, v.s, false)...)
	errs = append(errs, listtype.ValidateListSetsAndMaps(nil, v.s, obj)...)
	// The CEL rules assume a schema-valid object, so a schema error skips them.
	if len(errs) == 0 && v.cv != nil {
		celErrs, _ := v.cv.Validate(context.Background(), nil, v.s, obj, nil, celconfig.RuntimeCELCostBudget)
		errs = append(errs, celErrs...)
	}
	problems := make([]string, 0, len(unknown)+len(errs))
	for _, path := range unknown {
		problems = append(problems, fmt.Sprintf("unknown field %q", path))
	}
	for _, e := range errs {
		problems = append(problems, e.Error())
	}
	return problems
}

// manifestObjects returns each YAML document of file that holds an object.
// A document with only comments returns nothing.
func manifestObjects(t *testing.T, file string) []map[string]interface{} {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(b)))
	var out []map[string]interface{}
	for {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		// YAMLToJSON plus the apimachinery JSON decoder gives int64 numbers, as the apiserver does.
		j, err := yaml.YAMLToJSON(doc)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(j, &obj); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if len(obj) > 0 {
			out = append(out, obj)
		}
	}
}

// TestLocaldevManifestsMatchCRDs validates each Varroa resource under
// hack/localdev/manifests against the generated CRDs in charts/varroa/crds,
// with the apiserver validators. A Varroa kind with no CRD fails the test.
// The kubeconform step of the Checks job validates the core resources.
func TestLocaldevManifestsMatchCRDs(t *testing.T) {
	validators := crdValidators(t)
	files, err := filepath.Glob(filepath.Join(localdevManifestDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatalf("no manifests under %s", localdevManifestDir)
	}
	checked := 0
	for _, file := range files {
		for _, obj := range manifestObjects(t, file) {
			apiVersion, _ := obj["apiVersion"].(string)
			if !strings.HasPrefix(apiVersion, varroaAPIGroup+"/") {
				continue
			}
			kind, _ := obj["kind"].(string)
			name := (&unstructured.Unstructured{Object: obj}).GetName()
			key := apiVersion + "/" + kind
			checked++
			t.Run(filepath.Base(file)+"/"+kind+"/"+name, func(t *testing.T) {
				v, ok := validators[key]
				if !ok {
					t.Fatalf("no CRD under %s serves %s", crdDir, key)
				}
				for _, p := range v.validateCreate(obj) {
					t.Error(p)
				}
			})
		}
	}
	if checked == 0 {
		t.Fatalf("no %s resource under %s", varroaAPIGroup, localdevManifestDir)
	}
}

// TestLocaldevManifestCheckRefusesBadObjects proves that validateCreate
// refuses each class of defect that TestLocaldevManifestsMatchCRDs must catch.
func TestLocaldevManifestCheckRefusesBadObjects(t *testing.T) {
	validators := crdValidators(t)
	cases := []struct {
		name string
		key  string
		obj  string
		want string // substring of one problem line
	}{
		{
			name: "unknown spec field",
			key:  "varroa.dev/v1alpha1/Controller",
			obj:  "metadata: {name: c, namespace: varroa}\nspec: {version: \"\", noSuchField: true}",
			want: "spec.noSuchField",
		},
		{
			name: "wrong field type",
			key:  "varroa.dev/v1alpha1/Controller",
			obj:  "metadata: {name: c, namespace: varroa}\nspec: {version: 3}",
			want: "spec.version",
		},
		{
			name: "namespace on a cluster-scoped kind",
			key:  "varroa.dev/v1alpha1/VarroaRoleBinding",
			obj:  "metadata: {name: b, namespace: varroa}\nspec: {roleRef: admin, subjects: [{kind: User, name: admin}]}",
			want: "metadata.namespace",
		},
		{
			name: "no namespace on a namespaced kind",
			key:  "varroa.dev/v1alpha1/User",
			obj:  "metadata: {name: admin}\nspec: {email: admin@varroa.local}",
			want: "metadata.namespace",
		},
		{
			name: "invalid object name",
			key:  "varroa.dev/v1alpha1/User",
			obj:  "metadata: {name: Admin_User, namespace: varroa-system}\nspec: {email: admin@varroa.local}",
			want: "metadata.name",
		},
		{
			name: "failed CEL rule",
			key:  "varroa.dev/v1alpha1/VarroaRoleBinding",
			obj:  "metadata: {name: b}\nspec: {roleRef: admin, subjects: [{kind: User, name: admin}], scope: {}}",
			want: "spec.scope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := validators[tc.key]
			if !ok {
				t.Fatalf("no CRD serves %s", tc.key)
			}
			j, err := yaml.YAMLToJSON([]byte(tc.obj))
			if err != nil {
				t.Fatal(err)
			}
			var obj map[string]interface{}
			if err := json.Unmarshal(j, &obj); err != nil {
				t.Fatal(err)
			}
			slash := strings.LastIndex(tc.key, "/")
			obj["apiVersion"] = tc.key[:slash]
			obj["kind"] = tc.key[slash+1:]
			problems := v.validateCreate(obj)
			for _, p := range problems {
				if strings.Contains(p, tc.want) {
					return
				}
			}
			t.Fatalf("want a problem at %q, got %v", tc.want, problems)
		})
	}
}
