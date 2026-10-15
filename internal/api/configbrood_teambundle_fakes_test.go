package api

import (
	"context"
	"errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// errNoTeamBundle is what a test ConfigBrood that serves no member Team
// answers. None of these fakes backs a member cluster's Team bundles.
var errNoTeamBundle = errors.New("this test ConfigBrood serves no member team bundle")

func (f *stubConfigBrood) GetTeamBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTeamBundle
}

func (f *fakeConfigBrood) GetTeamBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTeamBundle
}

func (f *fakeConfigBroodForTest) GetTeamBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTeamBundle
}

func (f *scopeTestConfigBrood) GetTeamBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTeamBundle
}
