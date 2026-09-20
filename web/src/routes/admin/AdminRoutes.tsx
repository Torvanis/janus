import type { ReactNode } from 'react';
import { Route, Routes } from 'react-router-dom';
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
import { AlertsPage } from './Alerts';
import { AuditPage } from './Audit';
import { AdminRequestsPage } from './Requests';
import { NotFoundPage } from '../Errors';

export default function AdminRoutes(): ReactNode {
  return (
    <Routes>
      <Route index element={<AdminOverview />} />
      <Route path="upstreams" element={<UpstreamsPage />} />
      <Route path="upstreams/:id" element={<UpstreamsPage />} />
      <Route path="models" element={<AdminModelsPage />} />
      <Route path="managed-models" element={<ManagedModelsPage />} />
      <Route path="grants" element={<GrantsPage />} />
      <Route path="service-tokens" element={<ServiceTokensPage />} />
      <Route path="service-tokens/:id" element={<ServiceTokensPage />} />
      <Route path="users" element={<PeoplePage />} />
      <Route path="teams" element={<PeopleTeamsPage />} />
      <Route path="teams/import" element={<TeamImportPage />} />
      <Route path="teams/:id" element={<PeopleTeamsPage />} />
      <Route path="team-import" element={<LegacyWorkspaceRedirect />} />
      <Route path="provisioning" element={<LegacyWorkspaceRedirect />} />
      <Route path="users/:id" element={<PeoplePage />} />
      <Route path="quotas" element={<AdminQuotasPage />} />
      <Route path="rules" element={<RulesPage />} />
      <Route path="security" element={<SecurityPage />} />
      <Route path="security/:tab" element={<SecurityPage />} />
      <Route path="alerts" element={<AlertsPage />} />
      <Route path="requests" element={<AdminRequestsPage />} />
      <Route path="audit" element={<AuditPage />} />
      <Route path="system" element={<LegacyWorkspaceRedirect />} />
      <Route path="settings" element={<LegacyWorkspaceRedirect />} />
      <Route path="settings/:tab" element={<AdminSettingsPage />} />
      <Route path="*" element={<NotFoundPage />} />
    </Routes>
  );
}
