package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEffectiveBundleRef(t *testing.T) {
	cases := []struct {
		name        string
		cr          *Controller
		opNS        string
		wantName    string
		wantNS      string
		explanation string
	}{
		{
			name:        "nil ref resolves to the starter in the operator namespace",
			cr:          &Controller{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "team-a"}},
			opNS:        "varroa-system",
			wantName:    StarterBundleName,
			wantNS:      "varroa-system",
			explanation: "a zero-config controller is not unconfigured",
		},
		{
			name: "named ref without namespace defaults to the controller's",
			cr: &Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "team-a"},
				Spec:       ControllerSpec{ComposedBundleRef: &ComposedBundleRef{Name: "b"}},
			},
			opNS:     "varroa-system",
			wantName: "b",
			wantNS:   "team-a",
		},
		{
			name: "explicit namespace wins",
			cr: &Controller{
				ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "team-a"},
				Spec:       ControllerSpec{ComposedBundleRef: &ComposedBundleRef{Name: "b", Namespace: "shared"}},
			},
			opNS:     "varroa-system",
			wantName: "b",
			wantNS:   "shared",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, ns := EffectiveBundleRef(tc.cr, tc.opNS)
			if name != tc.wantName || ns != tc.wantNS {
				t.Errorf("got %s/%s, want %s/%s (%s)", ns, name, tc.wantNS, tc.wantName, tc.explanation)
			}
		})
	}

	if name, ns := EffectiveBundleRef(nil, "varroa-system"); name != "" || ns != "" {
		t.Errorf("nil controller should resolve to nothing, got %s/%s", ns, name)
	}
}
