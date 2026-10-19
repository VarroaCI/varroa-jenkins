import { useRef, useEffect } from "react";
import { BrowserRouter, Routes, Route, useLocation, useParams } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { trace } from "@opentelemetry/api";
import { ThemeProvider } from "./context/ThemeContext";
import { AuthProvider } from "./context/AuthContext";
import { ProtectedRoute } from "./components/ProtectedRoute";
import { ToastProvider } from "./components/Toast";
import Layout from "./components/Layout";
import ErrorBoundary from "./components/ErrorBoundary";
import Dashboard from "./pages/Dashboard";
import Controllers from "./pages/Controllers";
import ControllerDetail from "./pages/ControllerDetail";
import { JenkinsLinkRedirect } from "./pages/JenkinsLinkRedirect";
import ControllerWizard from "./pages/ControllerWizard";
import Clusters from "./pages/Clusters";
import Roles from "./pages/Roles";
import RoleForm from "./pages/RoleForm";
import RoleBindings from "./pages/RoleBindings";
import RoleBindingForm from "./pages/RoleBindingForm";
import Teams from "./pages/Teams";
import TeamForm from "./pages/TeamForm";
import JenkinsRoles from "./pages/JenkinsRoles";
import JenkinsRoleBindings from "./pages/JenkinsRoleBindings";
import JenkinsRoleForm from "./pages/JenkinsRoleForm";
import JenkinsRoleBindingForm from "./pages/JenkinsRoleBindingForm";
import CatalogBrowser from "./pages/CatalogBrowser";
import CatalogItemDetail from "./pages/CatalogItemDetail";
import CatalogSources from "./pages/CatalogSources";
import ComposedBundles from "./pages/ComposedBundles";
import ComposedBundleDetail from "./pages/ComposedBundleDetail";
import ComposedBundleEdit from "./pages/ComposedBundleEdit";
import { ComposerProvider } from "./context/ComposerContext";
import Profile from "./pages/Profile";
import ApiKeys from "./pages/ApiKeys";
import ManagedJenkins from "./pages/ManagedJenkins";
import Activity from "./pages/Activity";
import BroodOperations from "./pages/BroodOperations";
import BroodOperationDetail from "./pages/BroodOperationDetail";
import BroodSchedules from "./pages/BroodSchedules";
import BroodScheduleDetail from "./pages/BroodScheduleDetail";
import FleetPlugins from "./pages/FleetPlugins";
import Cells from "./pages/Cells";
import Agents from "./pages/Agents";
import TeamAgentPage from "./pages/pipelines/TeamAgentPage";
import Locks from "./pages/Locks";
import PipelinesIndex from "./pages/pipelines/PipelinesIndex";
import TeamHome from "./pages/pipelines/TeamHome";
import PipelinePage from "./pages/pipelines/PipelinePage";
import BuildPage from "./pages/pipelines/BuildPage";
import TestCaseHistory from "./pages/pipelines/TestCaseHistory";

/**
 * Remounts BuildPage per run id. React Router reuses one element when only
 * the params change, and the page's log rows, terminal state, and resume
 * watermark belong to a single run.
 */
function BuildPageRoute() {
  const { run } = useParams<{ run: string }>();
  return <BuildPage key={run} />;
}
import CliAuth from "./pages/CliAuth";
import { LoginPage } from "./pages/LoginPage";
import NoAccessibleClusters from "./components/NoAccessibleClusters";
import { useConfigurationCluster } from "./hooks/useConfigurationCluster";
import { PermissionRoute } from "./components/PermissionRoute";
import { NotFoundPage } from "./components/RecoveryState";
import { AssistantAdmin } from "./pages/AssistantAdmin";
import { BuiltinRolesPage, GroupsPage, IdentityPage, ProvisioningPage, UpdateCenterPage, UsersPage, VersionsPage } from "./pages/AdministrationPages";
import { AreaShell } from "./components/AreaShell";
import { SETTINGS_ITEMS, CATALOG_ITEMS } from "./lib/navAreas";
import SettingsIndex from "./pages/SettingsIndex";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      staleTime: 10_000,
    },
  },
});

function CatalogComposerRoute() {
  const { cluster, ready } = useConfigurationCluster();
  if (ready && !cluster) return <NoAccessibleClusters />;
  if (!cluster) return null;
  const storageKey = `varroa_composer_draft:${cluster}`;
  return <ComposerProvider key={storageKey} storageKey={storageKey}><CatalogBrowser /></ComposerProvider>;
}

function useRouteChangeSpan() {
  const location = useLocation();
  const prevRoute = useRef<string | undefined>(undefined);
  useEffect(() => {
    const tracer = trace.getTracer("varroa-frontend");
    const span = tracer.startSpan("frontend.routeChange", {
      attributes: {
        "route.from": prevRoute.current || "/",
        "route.to": location.pathname,
      },
    });
    prevRoute.current = location.pathname;
    return () => span.end();
  }, [location]);
}

function AppRoutes() {
  useRouteChangeSpan();
  return (
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <ToastProvider>
          <AuthProvider>
            <Routes>
              <Route path="cli-auth" element={<CliAuth />} />
              <Route path="login" element={<LoginPage />} />
              <Route element={<ProtectedRoute />}>
                <Route element={<Layout />}>
                  <Route index element={<Dashboard />} />
                  <Route path="controllers" element={<Controllers />} />
                  <Route path="controllers/create" element={<ControllerWizard />} />
                  <Route path="controllers/:cluster/:namespace/:name" element={<ControllerDetail />} />
                  <Route path="controllers/:cluster/:namespace/:name/jenkins" element={<ManagedJenkins />} />
                  <Route path="headless/:cluster/:namespace/:controller/*" element={<JenkinsLinkRedirect />} />
                  <Route
                    path="cells"
                    element={
                      <PermissionRoute resource="cellpools" verb="read" globalOnly>
                        <Cells />
                      </PermissionRoute>
                    }
                  />
                  <Route path="clusters" element={<Clusters />} />
                  {/* The lock page carries no sidebar row: a caller reaches
                      it by its own URL. */}
                  <Route
                    path="locks"
                    element={
                      <PermissionRoute resource="fleetlocks" verb="read" globalOnly>
                        <Locks />
                      </PermissionRoute>
                    }
                  />

                  {/* Settings area shell — list routes only */}
                  <Route element={<AreaShell items={SETTINGS_ITEMS} title="Admin & access" />}>
                    <Route path="settings" element={<SettingsIndex />} />
                    <Route path="access/users" element={<PermissionRoute admin><UsersPage /></PermissionRoute>} />
                    <Route path="access/groups" element={<PermissionRoute admin><GroupsPage /></PermissionRoute>} />
                    <Route path="access/builtin-roles" element={<PermissionRoute admin><BuiltinRolesPage /></PermissionRoute>} />
                    <Route path="access/roles" element={<PermissionRoute resource="roles" globalOnly><Roles /></PermissionRoute>} />
                    <Route path="access/role-bindings" element={<PermissionRoute resource="rolebindings" globalOnly><RoleBindings /></PermissionRoute>} />
                    <Route path="access/teams" element={<PermissionRoute resource="roles" globalOnly><Teams /></PermissionRoute>} />
                    <Route path="access/jenkins-roles" element={<PermissionRoute resource="jenkinsroles" globalOnly><JenkinsRoles /></PermissionRoute>} />
                    <Route path="access/jenkins-role-bindings" element={<PermissionRoute resource="jenkinsrolebindings" globalOnly><JenkinsRoleBindings /></PermissionRoute>} />
                    <Route path="administration/provisioning" element={<PermissionRoute resource="provisioningdefaults" verb="update" globalOnly><ProvisioningPage /></PermissionRoute>} />
                    <Route path="administration/versions" element={<PermissionRoute resource="provisioningdefaults" verb="update" globalOnly><VersionsPage /></PermissionRoute>} />
                    <Route path="administration/identity" element={<PermissionRoute admin><IdentityPage /></PermissionRoute>} />
                    <Route path="administration/update-center" element={<PermissionRoute admin><UpdateCenterPage /></PermissionRoute>} />
                    <Route path="administration/assistant" element={<PermissionRoute resource="assistant" verb="admin" globalOnly><AssistantAdmin /></PermissionRoute>} />
                  </Route>

                  {/* Create/edit routes stay outside the settings shell */}
                  <Route path="access/roles/create" element={<PermissionRoute resource="roles" verb="create" globalOnly><RoleForm /></PermissionRoute>} />
                  <Route path="access/roles/:name/edit" element={<PermissionRoute resource="roles" verb="update" globalOnly><RoleForm /></PermissionRoute>} />
                  <Route path="access/role-bindings/create" element={<PermissionRoute resource="rolebindings" verb="create" globalOnly><RoleBindingForm /></PermissionRoute>} />
                  <Route path="access/role-bindings/:name/edit" element={<PermissionRoute resource="rolebindings" verb="update" globalOnly><RoleBindingForm /></PermissionRoute>} />
                  <Route path="access/teams/create" element={<PermissionRoute resource="roles" verb="create" globalOnly><TeamForm /></PermissionRoute>} />
                  <Route path="access/teams/:name/edit" element={<PermissionRoute resource="roles" verb="update" globalOnly><TeamForm /></PermissionRoute>} />
                  <Route path="access/jenkins-roles/create" element={<PermissionRoute resource="jenkinsroles" verb="create" globalOnly><JenkinsRoleForm /></PermissionRoute>} />
                  <Route path="access/jenkins-roles/:name/edit" element={<PermissionRoute resource="jenkinsroles" verb="update" globalOnly><JenkinsRoleForm /></PermissionRoute>} />
                  <Route path="access/jenkins-role-bindings/create" element={<PermissionRoute resource="jenkinsrolebindings" verb="create" globalOnly><JenkinsRoleBindingForm /></PermissionRoute>} />
                  <Route path="access/jenkins-role-bindings/:name/edit" element={<PermissionRoute resource="jenkinsrolebindings" verb="update" globalOnly><JenkinsRoleBindingForm /></PermissionRoute>} />

                  {/* Catalog area shell — list routes only */}
                  <Route element={<AreaShell items={CATALOG_ITEMS} title="Catalog" />}>
                    <Route path="catalog" element={<CatalogComposerRoute />} />
                    <Route path="catalog/sources" element={<CatalogSources />} />
                    <Route path="catalog/bundles" element={<ComposedBundles />} />
                  </Route>

                  {/* Detail/create/edit routes stay outside the catalog shell */}
                  <Route path="catalog/items/:namespace/:name" element={<CatalogItemDetail />} />
                  <Route path="catalog/bundles/:namespace/:name" element={<ComposerProvider><ComposedBundleDetail /></ComposerProvider>} />
                  <Route path="catalog/bundles/:namespace/:name/edit" element={<ComposedBundleEdit />} />

                  <Route path="profile" element={<Profile />} />
                  <Route path="api-keys" element={<ApiKeys />} />
                  <Route path="activity" element={<Activity />} />
                  <Route path="brood-operations" element={<BroodOperations />} />
                  <Route path="brood-operations/:namespace/:name" element={<BroodOperationDetail />} />
                  <Route path="brood-schedules" element={<BroodSchedules />} />
                  <Route path="brood-schedules/:namespace/:name" element={<BroodScheduleDetail />} />
                  <Route path="plugins" element={<PermissionRoute resource="controllers" verb="read"><FleetPlugins /></PermissionRoute>} />
                  <Route path="pipelines" element={<PipelinesIndex />} />
                  <Route path="pipelines/:team" element={<TeamHome />} />
                  <Route path="pipelines/:team/agent" element={<TeamAgentPage />} />
                  <Route
                    path="agents"
                    element={
                      <PermissionRoute resource="agentsessions" verb="read" globalOnly>
                        <Agents />
                      </PermissionRoute>
                    }
                  />
                  <Route path="pipelines/:team/:job" element={<PipelinePage />} />
                  <Route path="pipelines/:team/:job/:run" element={<BuildPageRoute />} />
                  {/* The case route holds five segments and the run route four, so the two never match one URL. */}
                  <Route path="pipelines/:team/:job/tests/:case" element={<TestCaseHistory />} />
                  <Route path="*" element={<NotFoundPage />} />
                </Route>
              </Route>
            </Routes>
          </AuthProvider>
        </ToastProvider>
      </ThemeProvider>
    </QueryClientProvider>
  );
}

export default function App() {
  return (
    <ErrorBoundary>
      <BrowserRouter>
        <AppRoutes />
      </BrowserRouter>
    </ErrorBoundary>
  );
}
