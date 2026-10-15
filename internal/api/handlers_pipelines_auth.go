package api

import (
	"github.com/varroaci/varroa-jenkins/internal/auth"
)

// canReadPipeline reports whether claims may read ref's tenant, narrowed to
// jobPath when jobPath is not empty. It is the one call site the team
// page, the job page, the run list, the job test case history, the job
// search rows, and the list_runs MCP tool use. A nil Authorizer denies.
func (s *Server) canReadPipeline(claims *auth.Claims, ref TenantRef, jobPath string) bool {
	return s.deps.Authorizer.CanReadPipeline(claims, ref, jobPath)
}

// canWritePipeline reports whether claims may start, cancel, rerun, or
// answer an input of a build on ref's tenant at jobPath. It is the one
// call site the run request, cancel, rerun, the two input routes, the
// run_pipeline MCP tool, the two run input MCP tools, and the Run drawer
// control use. A nil Authorizer denies.
func (s *Server) canWritePipeline(claims *auth.Claims, ref TenantRef, jobPath string) bool {
	return s.deps.Authorizer.CanWritePipeline(claims, ref, jobPath)
}

// canReadRun reports whether claims may read one stored run, given the
// run row's own tenant and job_path columns. It builds Authorizer's small
// value itself, so no caller passes a record-store row type or a
// TenantRef. It is the one call site the run record, the artifacts, tests
// and changes tabs, the artifact download, the log stream, the run:
// ticket, get_run, get_run_log, and the run search rows use. A nil
// Authorizer denies.
func (s *Server) canReadRun(claims *auth.Claims, tenant, jobPath string) bool {
	return s.deps.Authorizer.CanReadRun(claims, RunTenantRow{Tenant: tenant, JobPath: jobPath})
}
