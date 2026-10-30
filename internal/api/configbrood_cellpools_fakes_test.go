package api

import (
	"context"
	"errors"

	"github.com/varroaci/varroa-jenkins/api/v1alpha1"
	"github.com/varroaci/varroa-jenkins/internal/bus"
)

// errNoCellPools is what a test ConfigBrood that serves no member pool
// answers. None of these fakes backs a member cluster's pools.
var errNoCellPools = errors.New("this test ConfigBrood serves no cell pool")

func (f *stubConfigBrood) ListCellPools(context.Context, string) ([]v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *stubConfigBrood) GetCellPool(context.Context, string, string) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *stubConfigBrood) ScaleCellPool(context.Context, string, bus.CellPoolScaleRequest) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBrood) ListCellPools(context.Context, string) ([]v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBrood) GetCellPool(context.Context, string, string) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBrood) ScaleCellPool(context.Context, string, bus.CellPoolScaleRequest) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBroodForTest) ListCellPools(context.Context, string) ([]v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBroodForTest) GetCellPool(context.Context, string, string) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBroodForTest) ScaleCellPool(context.Context, string, bus.CellPoolScaleRequest) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *scopeTestConfigBrood) ListCellPools(context.Context, string) ([]v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *scopeTestConfigBrood) GetCellPool(context.Context, string, string) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *scopeTestConfigBrood) ScaleCellPool(context.Context, string, bus.CellPoolScaleRequest) (*v1alpha1.CellPool, error) {
	return nil, errNoCellPools
}

func (f *stubConfigBrood) CellGrants(context.Context, string, []string) ([]bus.CellTenantGrants, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBrood) CellGrants(context.Context, string, []string) ([]bus.CellTenantGrants, error) {
	return nil, errNoCellPools
}

func (f *fakeConfigBroodForTest) CellGrants(context.Context, string, []string) ([]bus.CellTenantGrants, error) {
	return nil, errNoCellPools
}

func (f *scopeTestConfigBrood) CellGrants(context.Context, string, []string) ([]bus.CellTenantGrants, error) {
	return nil, errNoCellPools
}

func (f *stubConfigBrood) CellLint(context.Context, string, bus.CellLintRequest) (bus.CellLintResponse, error) {
	return bus.CellLintResponse{}, errNoCellPools
}

func (f *fakeConfigBrood) CellLint(context.Context, string, bus.CellLintRequest) (bus.CellLintResponse, error) {
	return bus.CellLintResponse{}, errNoCellPools
}

func (f *fakeConfigBroodForTest) CellLint(context.Context, string, bus.CellLintRequest) (bus.CellLintResponse, error) {
	return bus.CellLintResponse{}, errNoCellPools
}

func (f *scopeTestConfigBrood) CellLint(context.Context, string, bus.CellLintRequest) (bus.CellLintResponse, error) {
	return bus.CellLintResponse{}, errNoCellPools
}
