import { Link, useLocation, useParams } from 'react-router-dom';
import { RetainedPanel, RouteTopNav } from '../../components/RouteTopNav';
import { SystemPage } from './System';
import { ProvisioningPage } from './Provisioning';
import { LicenseCard } from './LicenseCard';
import { NotFoundPage } from '../Errors';

export const adminSettingsTabs = [
  { id: 'general', label: 'General' },
  { id: 'sign-in', label: 'Sign-in & provisioning' },
  { id: 'license', label: 'License & updates' },
  { id: 'troubleshooting', label: 'Troubleshooting' },
  { id: 'status', label: 'System status' },
] as const;

export function AdminSettingsPage() {
  const { tab = 'general' } = useParams();
  const location = useLocation();
  if (!adminSettingsTabs.some((item) => item.id === tab)) return <NotFoundPage />;
  const systemSection = tab === 'general' || tab === 'status' || tab === 'troubleshooting' ? tab : 'hidden';
  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">Gateway settings</h1>
          <p className="page-subtitle">
            Instance-wide administration. Each section keeps its own save and confirmation controls.
          </p>
        </div>
        <Link className="btn btn-ghost" to="/settings">
          Personal settings
        </Link>
      </header>
      <RouteTopNav
        label="Gateway settings"
        active={tab}
        items={adminSettingsTabs.map((item) => ({
          ...item,
          to: { pathname: `/admin/settings/${item.id}`, search: location.search },
        }))}
      />
      <RetainedPanel active={systemSection !== 'hidden'}>
        <SystemPage section={systemSection} />
      </RetainedPanel>
      <RetainedPanel active={tab === 'sign-in'}>
        <ProvisioningPage embedded />
      </RetainedPanel>
      <RetainedPanel active={tab === 'license'}>
        <div id="license">
          <LicenseCard />
        </div>
      </RetainedPanel>
    </div>
  );
}
