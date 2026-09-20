import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../lib/api';
import { ConfirmDialog } from '../components/ui';

/** Keep the legacy admin-only metadata/delete writes, not the legacy bulk roster editor. */
export function TeamAdminSettings({
  team,
}: {
  team: { id: string; name: string; lead_can_edit_quotas?: boolean; member_count: number };
}) {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [deleting, setDeleting] = useState(false);
  const invalidate = () =>
    Promise.all([
      client.invalidateQueries({ queryKey: ['teams'] }),
      client.invalidateQueries({ queryKey: ['admin', 'teams'] }),
      client.invalidateQueries({ queryKey: ['me'] }),
    ]);
  const delegate = useMutation({
    mutationFn: (enabled: boolean) =>
      api.patch(`/api/v1/admin/teams/${encodeURIComponent(team.id)}`, { lead_can_edit_quotas: enabled }),
    onSuccess: invalidate,
  });
  const remove = useMutation({
    mutationFn: () => api.del(`/api/v1/admin/teams/${encodeURIComponent(team.id)}`),
    onSuccess: async () => {
      setDeleting(false);
      navigate('/admin/teams?view=browse');
      await invalidate();
    },
  });
  return (
    <section className="card stack">
      <h3>Administrator controls</h3>
      <label className="row">
        <input
          type="checkbox"
          checked={team.lead_can_edit_quotas ?? false}
          disabled={delegate.isPending}
          onChange={(event) => delegate.mutate(event.target.checked)}
        />
        Team leads may edit quotas
      </label>
      {delegate.error && <p role="alert">{delegate.error.message}</p>}
      {remove.error && <p role="alert">{remove.error.message}</p>}
      <button className="btn btn-danger" onClick={() => setDeleting(true)}>
        Delete team
      </button>
      <ConfirmDialog
        open={deleting}
        onClose={() => {
          if (!remove.isPending) setDeleting(false);
        }}
        onConfirm={() => remove.mutate()}
        title={`Delete ${team.name}?`}
        consequence={`The team and its ${team.member_count} memberships will be removed. This cannot be undone.`}
        confirmLabel="Delete team"
        busy={remove.isPending}
      />
    </section>
  );
}
