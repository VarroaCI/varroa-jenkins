import { Card } from "../components/Card";
import { SectionPage } from "../components/SectionPage";
import SettingsBuiltinRolesTab from "./SettingsBuiltinRolesTab";
import SettingsGroupsTab from "./SettingsGroupsTab";
import SettingsIdentityTab from "./SettingsIdentityTab";
import SettingsProvisioningTab from "./SettingsProvisioningTab";
import SettingsUpdateCenterTab from "./SettingsUpdateCenterTab";
import SettingsUsersTab from "./SettingsUsersTab";
import SettingsVersionsTab from "./SettingsVersionsTab";

function Page({ title, description, children, readOnly }: { title: string; description: string; children: React.ReactNode; readOnly?: boolean }) {
  return <SectionPage title={title} description={description} readOnly={readOnly}><Card>{children}</Card></SectionPage>;
}

export const ProvisioningPage = () => <Page title="Provisioning" description="Configure controller provisioning defaults."><SettingsProvisioningTab /></Page>;
export const VersionsPage = () => <Page title="Versions" description="Review Jenkins version profiles." readOnly><SettingsVersionsTab /></Page>;
export const IdentityPage = () => <Page title="Identity" description="Review identity provider configuration." readOnly><SettingsIdentityTab /></Page>;
export const UsersPage = () => <Page title="Users" description="Manage users and their access."><SettingsUsersTab /></Page>;
export const GroupsPage = () => <Page title="Groups" description="Manage identity groups."><SettingsGroupsTab /></Page>;
export const BuiltinRolesPage = () => <Page title="Built-in Roles" description="Review Varroa's built-in authorization roles." readOnly><SettingsBuiltinRolesTab /></Page>;
export const UpdateCenterPage = () => <Page title="Update Center" description="Review update center status and plugin inventory, and upload a plugin."><SettingsUpdateCenterTab /></Page>;
