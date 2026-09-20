import { TeamManagementWorkspace } from '../Teams';
import { PeopleNavigation } from './PeopleNavigation';

export default function PeopleTeamsPage() {
  return (
    <div className="page stack">
      <header className="page-header">
        <div>
          <h1 className="page-title">People</h1>
          <p className="page-subtitle">Manage organization teams, membership, roles, group mappings, and quotas.</p>
        </div>
      </header>
      <PeopleNavigation active="teams" />
      <TeamManagementWorkspace />
    </div>
  );
}
