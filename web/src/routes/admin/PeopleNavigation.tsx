import { useLocation } from 'react-router-dom';
import { RouteTopNav } from '../../components/RouteTopNav';

/** Shared People navigation; team forms remain behind the admin route guard. */
export function PeopleNavigation({ active }: { active: 'users' | 'groups' | 'teams' }) {
  const location = useLocation();
  return (
    <RouteTopNav
      label="People sections"
      active={active}
      items={[
        { id: 'users', label: 'Users' },
        { id: 'groups', label: 'Groups' },
        { id: 'teams', label: 'Teams' },
      ].map((item) => {
        const params = new URLSearchParams(location.search);
        // Do not carry team-detail or user-directory filters into a different inventory.
        if (item.id !== active) {
          for (const key of ['q', 'team', 'section', 'view', 'page', 'role', 'active', 'group_id', 'team_id', 'sort'])
            params.delete(key);
        }
        if (item.id === 'teams') params.delete('tab');
        else params.set('tab', item.id);
        return { ...item, to: { pathname: item.id === 'teams' ? '/admin/teams' : '/admin/users', search: params.toString() } };
      })}
    />
  );
}
