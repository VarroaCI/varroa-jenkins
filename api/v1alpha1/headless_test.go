package v1alpha1

import "testing"

func TestIsHeadless(t *testing.T) {
	cases := []struct {
		name string
		cr   *Controller
		want bool
	}{
		{
			name: "no cell, headless false",
			cr:   &Controller{Spec: ControllerSpec{Headless: false}},
			want: false,
		},
		{
			name: "no cell, headless true",
			cr:   &Controller{Spec: ControllerSpec{Headless: true}},
			want: true,
		},
		{
			name: "cell enabled, headless false",
			cr:   &Controller{Spec: ControllerSpec{Cell: &CellSpec{Enabled: true}, Headless: false}},
			want: true,
		},
		{
			name: "cell enabled, headless true",
			cr:   &Controller{Spec: ControllerSpec{Cell: &CellSpec{Enabled: true}, Headless: true}},
			want: true,
		},
		{
			name: "cell present but disabled, headless false",
			cr:   &Controller{Spec: ControllerSpec{Cell: &CellSpec{Enabled: false}, Headless: false}},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHeadless(tc.cr); got != tc.want {
				t.Errorf("IsHeadless() = %v, want %v", got, tc.want)
			}
		})
	}

	if got := IsHeadless(nil); got != false {
		t.Errorf("IsHeadless(nil) = %v, want false", got)
	}
}
