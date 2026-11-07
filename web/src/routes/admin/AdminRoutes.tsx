import { ChatAdminPage } from './ChatAdmin';
import type { ReactNode } from 'react';
import { AdminOverview } from './Overview';
import { UpstreamsPage } from './Upstreams';
import { AdminModelsPage } from './Models';
import { GrantsPage } from './Grants';
import { ServiceTokensPage } from './ServiceTokens';
import { ManagedModelsPage } from './ManagedModels';
import { PeoplePage } from './People';
import PeopleTeamsPage from './PeopleTeamsPage';
import TeamImportPage from './TeamImportPage';
import { AdminSettingsPage } from './Settings';
import { LegacyWorkspaceRedirect } from '../../app/LegacyWorkspaceRedirect';
import { AdminQuotasPage } from './Quotas';
import { RulesPage } from './Rules';
import { SecurityPage } from './Security';
import { ChildSafetyPage } from './ChildSafety';
import { Navigate, Route, Routes } from 'react-router-dom';
import { RoutingPage } from './Routing';
import { AlertsPage } from './Alerts';
import { AuditPage } from './Audit';
import { AdminRequestsPage } from './Requests';
import { InsightsPage } from './Insights';
import { NotFoundPage } from '../Errors';

export default function AdminRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<AdminOverview />} />
      <Route path="upstreams" element={<UpstreamsPage />} />
      {/* Header policies moved to a tab of Policy rules. */}
      <Route path="header-policies" element={<Navigate to="/admin/rules/headers" replace />} />
      <Route path="upstreams/:id" element={<UpstreamsPage />} />
      <Route path="models" element={<AdminModelsPage />} />
      <Route path="managed-models" element={<ManagedModelsPage />} />
      <Route path="grants" element={<GrantsPage />} />
      <Route path="chat" element={<ChatAdminPage />} />
      <Route path="chat/:tab" element={<ChatAdminPage />} />
      <Route path="service-tokens" element={<ServiceTokensPage />} />
      <Route path="service-tokens/:id" element={<ServiceTokensPage />} />
      <Route path="subscriptions" element={<LegacyWorkspaceRedirect />} />
      <Route path="users" element={<PeoplePage />} />
      <Route path="teams" element={<PeopleTeamsPage />} />
      <Route path="teams/import" element={<TeamImportPage />} />
      <Route path="teams/:id" element={<PeopleTeamsPage />} />
      <Route path="team-import" element={<LegacyWorkspaceRedirect />} />
      <Route path="provisioning" element={<LegacyWorkspaceRedirect />} />
      <Route path="users/:id" element={<PeoplePage />} />
      <Route path="quotas" element={<AdminQuotasPage />} />
      <Route path="rules" element={<RulesPage />} />
      <Route path="rules/:tab" element={<RulesPage />} />
      <Route path="routing" element={<RoutingPage />} />
      <Route path="routing/:tab" element={<RoutingPage />} />
      <Route path="routing/:tab/:id" element={<RoutingPage />} />
      <Route path="child-safety" element={<ChildSafetyPage />} />
      {/* Family safety moved out of Security into Child Safety. */}
      <Route path="security/family" element={<Navigate to="/admin/child-safety" replace />} />
      <Route path="security" element={<SecurityPage />} />
      <Route path="security/:tab" element={<SecurityPage />} />
      <Route path="alerts" element={<AlertsPage />} />
      <Route path="requests" element={<AdminRequestsPage />} />
      <Route path="insights" element={<InsightsPage />} />
      <Route path="insights/:tab" element={<InsightsPage />} />
      <Route path="audit" element={<AuditPage />} />
      <Route path="system" element={<LegacyWorkspaceRedirect />} />
      <Route path="settings" element={<LegacyWorkspaceRedirect />} />
      <Route path="settings/:tab" element={<AdminSettingsPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
