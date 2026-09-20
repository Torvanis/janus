import { useLocation } from 'react-router-dom';
import { RouteTopNav } from '../../components/RouteTopNav';
import TeamImport from './TeamImport';
import { PeopleNavigation } from './PeopleNavigation';

export default function TeamImportPage() {
  const location = useLocation();
  const params = new URLSearchParams(location.search);
  params.set('view', 'browse');
  return (
    <div className="page stack">
      <header className="page-header">
        <div>
          <h1 className="page-title">People</h1>
          <p className="page-subtitle">Import teams with validated, all-or-nothing CSV.</p>
        </div>
      </header>
      <PeopleNavigation active="teams" />
      <RouteTopNav
        label="Teams"
        active="import"
        items={[
          { id: 'browse', label: 'Team directory', to: { pathname: '/admin/teams', search: params.toString() } },
          { id: 'import', label: 'Import teams', to: { pathname: '/admin/teams/import', search: location.search } },
        ]}
      />
      <TeamImport />
    </div>
  );
}
