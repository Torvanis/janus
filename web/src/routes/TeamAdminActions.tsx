import { useState } from 'react';
import { Link, useNavigate } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api, qs } from '../lib/api';
import { useDebounced } from '../lib/hooks';
import { Field, Modal } from '../components/ui';

/** Admin-only entry point, shared by the People team directory and detail. */
export function TeamAdminActions() {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [open, setOpen] = useState(false);
  const [name, setName] = useState('');
  const [search, setSearch] = useState('');
  const [lead, setLead] = useState<{ id: string; email: string } | null>(null);
  const q = useDebounced(search);
  const users = useQuery({
    queryKey: ['admin', 'team-create-candidates', q],
    queryFn: () =>
      api.get<{ users: { id: string; email: string; name?: string }[] }>(
        `/api/v1/admin/users${qs({ search: q, active: 'true', limit: 20 })}`,
      ),
    enabled: open && !!q.trim(),
  });
  const create = useMutation({
    mutationFn: () =>
      api.post<{ team: { id: string } }>('/api/v1/admin/teams', { name: name.trim(), lead_user_id: lead?.id ?? '' }),
    onSuccess: async ({ team }) => {
      await Promise.all([
        client.invalidateQueries({ queryKey: ['teams'] }),
        client.invalidateQueries({ queryKey: ['admin', 'teams'] }),
      ]);
      setOpen(false);
      setName('');
      setSearch('');
      setLead(null);
      navigate(`/admin/teams/${encodeURIComponent(team.id)}?view=manage&section=members`);
    },
  });
  return (
    <>
      <div className="row">
        <button className="btn btn-primary" onClick={() => setOpen(true)}>
          Create team
        </button>
        <Link className="btn" to="/admin/teams/import">
          Import teams
        </Link>
      </div>
      <Modal
        open={open}
        onClose={() => {
          if (!create.isPending) setOpen(false);
        }}
        title="Create team"
      >
        <form
          className="stack"
          onSubmit={(event) => {
            event.preventDefault();
            if (name.trim() && !create.isPending) create.mutate();
          }}
        >
          <Field label="Team name" required>
            <input
              className="input"
              aria-label="Team name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              required
            />
          </Field>
          <Field
            label="Find initial team lead"
            hint="Optional. Search active accounts by name or email; showing up to 20 matches."
          >
            <input
              className="input"
              aria-label="Find initial team lead"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
            />
          </Field>
          {users.isFetching && <p role="status">Searching accounts…</p>}
          {users.error && <p role="alert">{users.error.message}</p>}
          {!!q.trim() && users.data && (
            <Field label="Initial team lead">
              <select
                className="select"
                aria-label="Initial team lead"
                value={lead?.id ?? ''}
                onChange={(event) => setLead(users.data.users.find((user) => user.id === event.target.value) ?? null)}
              >
                <option value="">No initial lead</option>
                {lead && !users.data.users.some((user) => user.id === lead.id) && <option value={lead.id}>{lead.email}</option>}
                {users.data.users.map((user) => (
                  <option key={user.id} value={user.id}>
                    {user.name || user.email} · {user.email}
                  </option>
                ))}
              </select>
            </Field>
          )}
          {lead && (
            <p>
              Selected lead: {lead.email}{' '}
              <button type="button" className="btn btn-ghost" onClick={() => setLead(null)}>
                Clear lead
              </button>
            </p>
          )}
          {create.error && <p role="alert">{create.error.message}</p>}
          <button className="btn btn-primary" disabled={!name.trim() || create.isPending}>
            {create.isPending ? 'Creating…' : 'Create team'}
          </button>
        </form>
      </Modal>
    </>
  );
}
