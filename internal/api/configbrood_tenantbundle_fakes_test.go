package api

import (
	"context"
	"errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
)

// errNoTenantBundle is what a test ConfigBrood that serves no member Tenant
// answers. None of these fakes backs a member cluster's Tenant bundles.
var errNoTenantBundle = errors.New("this test ConfigBrood serves no member tenant bundle")

func (f *stubConfigBrood) GetTenantBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTenantBundle
}

func (f *fakeConfigBrood) GetTenantBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTenantBundle
}

func (f *fakeConfigBroodForTest) GetTenantBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTenantBundle
}

func (f *scopeTestConfigBrood) GetTenantBundleContent(context.Context, string, string) (*v1alpha1.ComposedBundle, map[string]string, bool, error) {
	return nil, nil, false, errNoTenantBundle
}
