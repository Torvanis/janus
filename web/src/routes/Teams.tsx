import { useState } from 'react';
import { Link, useParams, useNavigate, useLocation } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, ApiError, qs } from '../lib/api';
import { useDebounced, useUrlState } from '../lib/hooks';
import { useSession } from '../app/session';
import { AsyncSection, Badge, ConfirmDialog, Field } from '../components/ui';
import { TeamPage, TeamQuotaManagement } from './Team';
import './teams.css';
import { TeamAdminActions } from './TeamAdminActions';
import { TeamAdminSettings } from './TeamAdminSettings';
import { RetainedPanel, RouteTopNav } from '../components/RouteTopNav';

type Role = 'member' | 'moderator' | 'leader';
type Team = {
  id: string;
  name: string;
  listed: boolean;
  member_count: number;
  my_role?: Role;
  can_manage?: boolean;
  pending_request_count?: number;
  lead_can_edit_quotas?: boolean;
};
type Member = {
  id?: string;
  user_id: string;
  email: string;
  name?: string;
  role: Role;
  sources?: { source_type: string; source_id: string }[];
};
type Request = {
  id: string;
  user_id: string;
  team_id?: string;
  team_name?: string;
  email?: string;
  user_email?: string;
  user_name?: string;
  status: string;
  reason?: string;
  decision_reason?: string;
};
type Detail = {
  team: Team;
  members?: Member[];
  requests?: Request[];
  my_role?: Role;
  membership_role?: string;
  actor_role?: string;
  can_manage?: boolean;
  pending_request_count?: number;
  group_ids?: string[];
};
const root = '/api/v1/teams';

export { default } from './PersonalTeams';

/** Role-aware management UI, mounted only inside the admin People boundary. */
export function TeamManagementWorkspace() {
  const { me } = useSession();
  const [search, setSearch] = useUrlState('q', '');
  const [queryTeam] = useUrlState('team', '');
  const { id } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  const view = new URLSearchParams(location.search).get('view');
  const explicitTeam = id || queryTeam;
  const setSelected = (teamId: string) => {
    const params = new URLSearchParams(location.search);
    params.delete('team');
    params.delete('section');
    if (teamId) params.delete('view');
    else params.set('view', 'browse');
    // Keep directory query links usable; route selections must leave the old id.
    if (teamId && !id) params.set('team', teamId);
    navigate({
      pathname: teamId && id ? `/admin/teams/${encodeURIComponent(teamId)}` : '/admin/teams',
      search: params.toString(),
    });
  };
  const q = useDebounced(search);
  const teams = useQuery({
    queryKey: ['teams', 'directory', q],
    refetchInterval: 30_000,
    queryFn: () => api.get<{ teams: Team[] }>(`${root}${qs({ q })}`),
  });
  const history = useQuery({
    queryKey: ['teams', 'request-history'],
    queryFn: () => api.get<{ requests: Request[] }>('/api/v1/team-requests'),
  });
  const memberships = teams.data?.teams.filter((team) => team.my_role) ?? [];
  // Administration always opens the organization directory, never an implicit personal team.
  const selected = explicitTeam;
  if (selected && view !== 'browse') {
    return (
      <div className="page stack team-directory">
        <div className="row-between">
          {me?.role === 'admin' && <TeamAdminActions />}
          {memberships.length > 1 && (
            <Field label="Viewing team">
              <select className="select" value={selected} onChange={(e) => setSelected(e.target.value)}>
                {!memberships.some((team) => team.id === selected) && <option value={selected}>Selected team</option>}
                {memberships.map((team) => (
                  <option key={team.id} value={team.id}>
                    {team.name}
                  </option>
                ))}
              </select>
            </Field>
          )}
        </div>
        <TeamDetail key={selected} id={selected} onClose={() => setSelected('')} history={history.data?.requests ?? []} />
      </div>
    );
  }
  return (
    <div className="page stack team-directory">
      <header className="page-header">
        <div>
          <h1 className="page-title">Teams</h1>
          <p className="page-subtitle">Manage all organization teams, membership, roles, and settings.</p>
        </div>
        {me?.role === 'admin' && <TeamAdminActions />}
      </header>
      <Field label="Search teams">
        <input className="input" value={search} onChange={(e) => setSearch(e.target.value)} />
      </Field>
      <section className="card stack" aria-label="Team directory">
        <AsyncSection
          query={teams}
          empty={{
            when: (data) => !data.teams?.length,
            title: 'No teams found',
            body: 'Try another search. Unlisted teams are visible only to members and administrators.',
          }}
        >
          {(data) => (
            <div className="stack">
              {data.teams.map((team) => (
                <div className="row-between team-directory-row" key={team.id}>
                  <button className="btn btn-ghost" aria-pressed={selected === team.id} onClick={() => setSelected(team.id)}>
                    {team.name}
                  </button>
                  <div className="row">
                    <Badge>{team.listed ? 'Listed' : 'Unlisted'}</Badge>
                    <span>{team.member_count} members</span>
                    {team.my_role ? <Badge>{team.my_role}</Badge> : <Badge>Not a member</Badge>}
                    {!team.my_role && history.data?.requests.find((request) => request.team_id === team.id) && (
                      <Badge>Your request: {history.data.requests.find((request) => request.team_id === team.id)?.status}</Badge>
                    )}
                    {(team.can_manage ?? (me?.role === 'admin' || team.my_role === 'leader' || team.my_role === 'moderator')) && (
                      <>
                        <Link className="btn btn-ghost" to={`/admin/teams/${encodeURIComponent(team.id)}?view=manage`}>
                          Manage team
                        </Link>
                        {!!team.pending_request_count && (
                          <Link
                            className="btn team-request-callout"
                            to={`/admin/teams/${encodeURIComponent(team.id)}?view=manage&section=requests`}
                          >
                            {team.pending_request_count} {team.pending_request_count === 1 ? 'request' : 'requests'} waiting
                          </Link>
                        )}
                      </>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}
        </AsyncSection>
      </section>
      {selected && (
        <TeamDetail key={selected} id={selected} onClose={() => setSelected('')} history={history.data?.requests ?? []} />
      )}
      <details className="card team-request-history">
        <summary>My membership requests</summary>
        <AsyncSection
          query={history}
          empty={{
            when: (data) => !data.requests?.length,
            title: 'No membership requests',
            body: 'Select a listed team to request membership.',
          }}
        >
          {(data) => (
            <ul>
              {data.requests.map((request) => (
                <li key={request.id}>
                  {request.team_name || 'Team'} — <Badge>{request.status}</Badge>
                  {request.reason && <p>{request.reason}</p>}
                  {request.decision_reason && <p>Decision: {request.decision_reason}</p>}
                  {request.team_id && (
                    <button className="btn btn-ghost" onClick={() => setSelected(request.team_id!)}>
                      View team
                    </button>
                  )}
                </li>
              ))}
            </ul>
          )}
        </AsyncSection>
      </details>
    </div>
  );
}

function TeamDetail({ id, onClose, history }: { id: string; onClose: () => void; history: Request[] }) {
  const query = useQuery({
    queryKey: ['teams', 'detail', id],
    refetchInterval: 30_000,
    queryFn: () => api.get<Detail>(`${root}/${encodeURIComponent(id)}`),
  });
  return (
    <section className="stack team-detail">
      <button className="btn btn-ghost" onClick={onClose}>
        Close team
      </button>
      <AsyncSection query={query}>{(data) => <TeamManagement id={id} data={data} history={history} />}</AsyncSection>
    </section>
  );
}

function TeamManagement({ id, data, history }: { id: string; data: Detail; history: Request[] }) {
  const { me, refresh } = useSession();
  const client = useQueryClient();
  const admin = me?.role === 'admin';
  const role = data.membership_role ?? data.my_role ?? data.team.my_role;
  const manager = data.can_manage ?? (admin || role === 'leader' || role === 'moderator');
  const leader = manager && (admin || role === 'leader');
  const location = useLocation();
  const requestedSection = new URLSearchParams(location.search).get('section');
  const view = new URLSearchParams(location.search).get('view');
  const section =
    requestedSection === 'usage' && (role || admin)
      ? 'usage'
      : manager && (requestedSection === 'requests' || (requestedSection === 'settings' && leader))
        ? requestedSection
        : !requestedSection && view !== 'manage' && view !== 'details' && (role || admin)
          ? 'usage'
          : 'members';
  const pendingCount =
    data.pending_request_count ?? (data.requests ?? []).filter((request) => request.status === 'pending').length;
  const path = `${root}/${encodeURIComponent(id)}`;
  const [name, setName] = useState(data.team.name);
  const [listed, setListed] = useState(data.team.listed);
  const [reason, setReason] = useState('');
  const [decisionReasons, setDecisionReasons] = useState<Record<string, string>>({});
  const [email, setEmail] = useState('');
  const [person, setPerson] = useState<{ id: string; email: string } | null>(null);
  const [newRole, setNewRole] = useState<Role>('member');
  const [roleDrafts, setRoleDrafts] = useState<Record<string, Role>>({});
  // A null draft follows query data; unrelated refreshes never replace unsaved edits.
  const [groupDraft, setGroupIds] = useState<string[] | null>(null);
  const groupIds = groupDraft ?? data.group_ids ?? [];
  const [remove, setRemove] = useState<Member | null>(null);
  const [message, setMessage] = useState('');
  const debounced = useDebounced(email);
  const candidates = useQuery({
    queryKey: ['teams', 'candidates', id, debounced],
    queryFn: () =>
      api.get<{ users: { id: string; email: string; name?: string }[] }>(`${path}/candidates${qs({ q: debounced })}`),
    enabled: manager && !!debounced.trim(),
  });
  const groups = useQuery({
    queryKey: ['admin', 'groups'],
    queryFn: () => api.get<{ groups: { id: string; name: string }[] }>('/api/v1/admin/groups'),
    enabled: admin,
  });
  const action = useMutation({
    mutationFn: async ({ run, success }: { run: () => Promise<unknown>; success: string; onSaved?: () => void }) => {
      await run();
      return success;
    },
    onMutate: () => setMessage(''),
    onSuccess: async (success, variables) => {
      setMessage(success);
      setRemove(null);
      setPerson(null);
      await Promise.all([
        client.invalidateQueries({ queryKey: ['teams'] }),
        client.invalidateQueries({ queryKey: ['admin', 'teams'] }),
        client.invalidateQueries({ queryKey: ['me'] }),
      ]);
      variables.onSaved?.();
      refresh();
    },
  });
  const run = (operation: () => Promise<unknown>, success: string, onSaved?: () => void) =>
    action.mutate({ run: operation, success, onSaved });
  const members = data.members ?? [];
  const self = members.find((member) => (member.user_id || member.id) === me?.id);
  const pending = history.some((request) => request.team_id === id && request.status === 'pending');
  const retained = (member: Member) => member.sources?.some((source) => source.source_type === 'group');
  const canRemove = (member: Member) => manager && (leader || (role === 'moderator' && member.role === 'member'));
  return (
    <div className="stack team-management">
      <h2>{data.team.name}</h2>
      {manager && (
        <>
          <p className="muted">
            {admin ? 'Administrator' : role === 'leader' ? 'Team lead' : 'Moderator'} administration ·{' '}
            {leader
              ? 'Manage membership, roles, and team settings.'
              : 'Review requests and manage ordinary members. Only leads and administrators can change roles or settings.'}
          </p>
          {pendingCount > 0 && (
            <Link className="btn team-request-callout" to={`/admin/teams/${encodeURIComponent(id)}?view=manage&section=requests`}>
              {pendingCount} {pendingCount === 1 ? 'request' : 'requests'} waiting
            </Link>
          )}
        </>
      )}
      <RouteTopNav
        label={manager ? 'Team administration' : 'Team sections'}
        active={section}
        items={[
          ...(role || admin ? [{ id: 'usage', label: 'Usage and quotas' }] : []),
          { id: 'members', label: 'Members' },
          ...(manager ? [{ id: 'requests', label: `Requests (${pendingCount})` }] : []),
          ...(leader ? [{ id: 'settings', label: 'Settings' }] : []),
        ].map((item) => {
          const params = new URLSearchParams(location.search);
          params.set('view', 'manage');
          params.set('section', item.id);
          params.delete('team');
          return { ...item, to: { pathname: `/admin/teams/${encodeURIComponent(id)}`, search: params.toString() } };
        })}
      />
      {(role || admin) && (
        <RetainedPanel active={section === 'usage'}>
          <TeamPage teamId={id} workspace="administration" />
        </RetainedPanel>
      )}
      <div className="row">
        <Badge>{data.team.listed ? 'Listed' : 'Unlisted'}</Badge>
        {role && <Badge>Your role: {role}</Badge>}
      </div>
      {!data.team.listed && <p>Unlisted teams are direct-add only. Join requests and approvals are disabled.</p>}
      {message && (
        <p className="banner banner-info" role="status">
          {message}
        </p>
      )}
      {action.error && (
        <p className="banner banner-danger" role="alert">
          {action.error.message}{' '}
          {action.error instanceof ApiError && action.error.payload.reason && <span>{action.error.payload.reason} </span>}
          Please correct the action and try again.
        </p>
      )}
      {action.isPending && <p role="status">Saving changes…</p>}
      {leader && section === 'settings' && (
        <form
          className="stack"
          onSubmit={(e) => {
            e.preventDefault();
            run(() => api.patch(path, { name: name.trim(), listed }), 'Team settings saved.');
          }}
        >
          <h3>Team settings</h3>
          <Field label="Team name">
            <input className="input" value={name} onChange={(e) => setName(e.target.value)} required />
          </Field>
          <label>
            <input type="checkbox" checked={listed} onChange={(e) => setListed(e.target.checked)} /> Listed in the directory
          </label>
          <p>Unlisting disables requests; existing members retain access.</p>
          <button className="btn" disabled={action.isPending || !name.trim()}>
            Save team
          </button>
        </form>
      )}
      {!role && data.team.listed && (
        <div className="stack">
          <h3>Join this team</h3>
          {pending ? (
            <>
              <p>A membership request is pending.</p>
              <button
                className="btn"
                disabled={action.isPending}
                onClick={() => run(() => api.del(`${path}/join-requests`), 'Membership request cancelled.')}
              >
                Cancel request
              </button>
            </>
          ) : (
            <>
              <Field label="Why would you like to join?">
                <textarea className="input" value={reason} onChange={(e) => setReason(e.target.value)} />
              </Field>
              <button
                className="btn btn-primary"
                disabled={action.isPending}
                onClick={() => run(() => api.post(`${path}/join-requests`, { reason }), 'Membership request submitted.')}
              >
                Request to join
              </button>
            </>
          )}
        </div>
      )}
      {(role || admin) && section === 'members' && (
        <>
          <h3>Members</h3>
          {!members.length ? (
            <p>No members yet.</p>
          ) : (
            <div className="table-wrap">
              <table className="data">
                <thead>
                  <tr>
                    <th>Person</th>
                    <th>Role</th>
                    <th>Membership source</th>
                    <th>Actions</th>
                  </tr>
                </thead>
                <tbody>
                  {members.map((member) => (
                    <tr key={member.user_id || member.id}>
                      <td>
                        {member.name && <div>{member.name}</div>}
                        {member.email}
                      </td>
                      <td>
                        {leader ? (
                          <div className="stack">
                            <select
                              className="select"
                              aria-label={`Role for ${member.email}`}
                              value={roleDrafts[member.user_id || member.id!] ?? member.role}
                              disabled={action.isPending}
                              onChange={(e) =>
                                setRoleDrafts({ ...roleDrafts, [member.user_id || member.id!]: e.currentTarget.value as Role })
                              }
                            >
                              <option value="member">Member</option>
                              <option value="moderator">Moderator</option>
                              <option value="leader">Leader</option>
                            </select>
                            {roleDrafts[member.user_id || member.id!] &&
                              roleDrafts[member.user_id || member.id!] !== member.role && (
                                <button
                                  className="btn"
                                  disabled={action.isPending}
                                  aria-label={`Save role for ${member.email} as ${roleDrafts[member.user_id || member.id!]}`}
                                  onClick={() => {
                                    const memberId = member.user_id || member.id!;
                                    const nextRole = roleDrafts[memberId];
                                    run(
                                      () => api.patch(`${path}/members/${encodeURIComponent(memberId)}`, { role: nextRole }),
                                      'Member role updated.',
                                      () =>
                                        setRoleDrafts((drafts) => {
                                          const next = { ...drafts };
                                          delete next[memberId];
                                          return next;
                                        }),
                                    );
                                  }}
                                >
                                  Save role as {roleDrafts[member.user_id || member.id!]}
                                </button>
                              )}
                          </div>
                        ) : (
                          <Badge>{member.role}</Badge>
                        )}
                      </td>
                      <td>
                        {member.sources
                          ?.map((source) => (source.source_type === 'group' ? 'Group-derived' : source.source_type))
                          .join(', ') || 'Direct membership'}
                        {retained(member) && (
                          <p className="small muted">Group-derived membership remains after removing direct membership.</p>
                        )}
                      </td>
                      <td>
                        {canRemove(member) && (member.user_id || member.id) !== me?.id && (
                          <button
                            className="btn btn-ghost"
                            disabled={action.isPending}
                            onClick={() => setRemove(member)}
                            aria-label={`Remove ${member.email}`}
                          >
                            Remove
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {self && (
            <button className="btn" disabled={action.isPending} onClick={() => setRemove(self)}>
              Leave team
            </button>
          )}
          {leader && (
            <p className="small muted">
              A team must keep at least one leader. Assign another leader before leaving or demoting the last leader.
            </p>
          )}
        </>
      )}
      {manager && section === 'members' && (
        <section className="stack">
          <h3>Add a member</h3>
          <Field label="Find people by email">
            <input
              className="input"
              type="search"
              value={email}
              onChange={(e) => {
                setEmail(e.target.value);
                setPerson(null);
              }}
            />
          </Field>
          {!!debounced.trim() && (
            <AsyncSection
              query={candidates}
              empty={{
                when: (result) => !result.users?.length,
                title: 'No people found',
                body: 'Try another email address. The person must have an active account.',
              }}
            >
              {(result) => (
                <div className="stack">
                  {result.users.map((user) => (
                    <button
                      className="btn btn-ghost"
                      key={user.id}
                      onClick={() => setPerson(user)}
                      aria-label={`Select ${user.email}`}
                      aria-pressed={person?.id === user.id}
                    >
                      {user.email}
                      {user.name ? ` — ${user.name}` : ''}
                    </button>
                  ))}
                </div>
              )}
            </AsyncSection>
          )}
          {person && <p>Selected: {person.email}</p>}
          {leader ? (
            <Field label="New member role">
              <select className="select" value={newRole} onChange={(e) => setNewRole(e.target.value as Role)}>
                <option value="member">Member</option>
                <option value="moderator">Moderator</option>
                <option value="leader">Leader</option>
              </select>
            </Field>
          ) : (
            <p>Moderators can add ordinary members, but cannot promote users.</p>
          )}
          <button
            className="btn btn-primary"
            disabled={!person || action.isPending}
            onClick={() =>
              person &&
              run(() => api.post(`${path}/members`, { user_id: person.id, role: leader ? newRole : 'member' }), 'Member added.')
            }
          >
            Add member
          </button>
        </section>
      )}
      {manager && section === 'requests' && (
        <section className="stack">
          <h3>Pending join requests</h3>
          {!(data.requests ?? []).some((request) => request.status === 'pending') && <p>No pending join requests.</p>}
          {(data.requests ?? [])
            .filter((request) => request.status === 'pending')
            .map((request) => (
              <div className="card stack" key={request.id}>
                {request.user_name && <div>{request.user_name}</div>}
                <strong>
                  {request.email ||
                    request.user_email ||
                    members.find((member) => member.user_id === request.user_id)?.email ||
                    'Membership applicant'}
                </strong>
                <p>{request.reason || 'No reason provided.'}</p>
                <Field
                  label={`Decision reason for ${request.user_email || request.email || request.user_name || 'membership applicant'}`}
                >
                  <input
                    className="input"
                    value={decisionReasons[request.id] ?? ''}
                    onChange={(e) => setDecisionReasons({ ...decisionReasons, [request.id]: e.target.value })}
                  />
                </Field>
                <div className="row">
                  {(data.team.listed ? [true, false] : [false]).map((approve) => (
                    <button
                      className="btn"
                      key={String(approve)}
                      disabled={action.isPending}
                      onClick={() =>
                        run(
                          () =>
                            api.post(`${path}/requests/${encodeURIComponent(request.id)}/decision`, {
                              approve,
                              reason: decisionReasons[request.id] ?? '',
                            }),
                          approve ? 'Membership request approved.' : 'Membership request denied.',
                        )
                      }
                    >
                      {approve ? 'Approve' : 'Deny'}
                    </button>
                  ))}
                </div>
              </div>
            ))}
        </section>
      )}
      {leader && (
        <RetainedPanel active={section === 'settings'}>
          <TeamQuotaManagement teamId={id} />
        </RetainedPanel>
      )}
      {admin && manager && (
        <RetainedPanel active={section === 'settings'}>
          <TeamAdminSettings team={{ ...data.team, member_count: members.length }} />
        </RetainedPanel>
      )}
      {admin && manager && section === 'settings' && (
        <section className="stack">
          <h3>Group mappings</h3>
          <p>
            Mapped identity groups grant membership automatically. Removing a direct membership does not remove group access;
            update the mapping or identity group instead.
          </p>
          <AsyncSection query={groups}>
            {(result) => (
              <>
                <fieldset disabled={action.isPending}>
                  <legend>Groups granting membership</legend>
                  {!result.groups?.length && <p>No groups available.</p>}
                  {(result.groups ?? []).map((group) => (
                    <label className="row" key={group.id}>
                      <input
                        type="checkbox"
                        checked={groupIds.includes(group.id)}
                        onChange={(e) =>
                          setGroupIds(e.target.checked ? [...groupIds, group.id] : groupIds.filter((value) => value !== group.id))
                        }
                      />
                      {group.name}
                    </label>
                  ))}
                </fieldset>
                <button
                  className="btn"
                  disabled={action.isPending}
                  onClick={() =>
                    run(
                      () => api.put(`${path}/groups`, { group_ids: groupIds }),
                      'Group mappings saved.',
                      () => setGroupIds(null),
                    )
                  }
                >
                  Save group mappings
                </button>
              </>
            )}
          </AsyncSection>
        </section>
      )}
      <ConfirmDialog
        open={!!remove}
        onClose={() => {
          if (!action.isPending) setRemove(null);
        }}
        title={remove?.user_id === me?.id ? 'Leave this team?' : `Remove ${remove?.email ?? 'member'}?`}
        consequence={
          remove && retained(remove)
            ? 'Only direct membership will be removed. Group-derived membership will remain until the group mapping or identity group is changed.'
            : 'Direct membership will be removed. The last leader cannot be removed.'
        }
        confirmLabel="Remove direct membership"
        busy={action.isPending}
        onConfirm={() =>
          remove &&
          run(
            () => api.del(`${path}/members/${encodeURIComponent(remove.user_id || remove.id!)}`),
            retained(remove) ? 'Direct membership removed. Group-derived membership remains.' : 'Membership removed.',
          )
        }
      />
    </div>
  );
}
