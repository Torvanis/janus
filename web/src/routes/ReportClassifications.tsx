import { useState, type ReactNode } from 'react';
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router-dom';
import { api } from '../lib/api';
import { useSession } from '../app/session';
import { Collection } from '../components/Collection';
import { AsyncSection, Modal, Field } from '../components/ui';
interface Classification {
  model_id: string;
  model_name: string;
  family: string;
  provider: string;
  hosting: string;
}
export function ReportClassifications(): ReactNode {
  const { me } = useSession();
  const client = useQueryClient();
  const [draft, setDraft] = useState<Classification | null>(null);
  const [message, setMessage] = useState('');
  const query = useQuery({
    queryKey: ['reports', 'classifications'],
    queryFn: () => api.get<{ classifications: Classification[] }>('/api/v1/reports/classifications'),
    enabled: me?.role === 'admin',
  });
  const save = useMutation({
    mutationFn: (row: Classification) =>
      api.put(`/api/v1/reports/classifications/${encodeURIComponent(row.model_id)}`, {
        family: row.family,
        provider: row.provider,
        hosting: row.hosting,
      }),
    onSuccess: () => {
      setDraft(null);
      setMessage('Labels saved for future requests.');
      void client.invalidateQueries({ queryKey: ['reports', 'classifications'] });
    },
  });
  if (me?.role !== 'admin')
    return (
      <section className="page">
        <h1>Model reporting labels</h1>
        <p>Administrator access required.</p>
        <Link to="/reports">Back to reports</Link>
      </section>
    );
  return (
    <section className="page">
      <Link to="/reports">← Back to reports</Link>
      <h1>Model reporting labels</h1>
      <p>Changes apply to future requests; historical snapshots are never rewritten. Blank fields remain unknown.</p>
      {message && <p role="status">{message}</p>}
      <AsyncSection query={query}>
        {(data) => (
          <Collection
            name="Model labels"
            rows={data.classifications ?? []}
            rowKey={(r) => r.model_id}
            columns={[
              { id: 'model', label: 'Model', value: (r) => r.model_name, render: (r) => r.model_name || r.model_id },
              ...(['family', 'provider', 'hosting'] as const).map((key) => ({
                id: key,
                label: key,
                value: (r: Classification) => r[key],
                render: (r: Classification) => r[key] || 'Unknown',
              })),
              {
                id: 'actions',
                label: 'Actions',
                render: (r) => (
                  <button
                    className="btn btn-sm"
                    onClick={() => {
                      save.reset();
                      setDraft({ ...r });
                    }}
                  >
                    Edit {r.model_name}
                  </button>
                ),
              },
            ]}
          />
        )}
      </AsyncSection>
      <Modal open={!!draft} onClose={() => setDraft(null)} title={`Edit ${draft?.model_name ?? 'model labels'}`}>
        {draft && (
          <form
            className="stack"
            onSubmit={(e) => {
              e.preventDefault();
              save.mutate(draft);
            }}
          >
            {(['family', 'provider'] as const).map((key) => (
              <Field key={key} label={key}>
                <input
                  className="input"
                  aria-label={key}
                  value={draft[key]}
                  maxLength={100}
                  onChange={(e) => setDraft({ ...draft, [key]: e.target.value })}
                />
              </Field>
            ))}
            <Field label="Hosting">
              <select
                className="select"
                aria-label="Hosting"
                value={draft.hosting}
                onChange={(e) => setDraft({ ...draft, hosting: e.target.value })}
              >
                <option value="">Unknown</option>
                <option value="self_hosted">Self-hosted</option>
                <option value="external">External</option>
                <option value="hybrid">Hybrid</option>
              </select>
            </Field>
            {save.error && <p role="alert">{save.error.message}</p>}
            <button className="btn btn-primary" disabled={save.isPending}>
              Save labels
            </button>
          </form>
        )}
      </Modal>
    </section>
  );
}
