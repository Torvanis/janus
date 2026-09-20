import { useRef, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { Field, Spinner } from '../../components/ui';

type Preview = {
  valid: boolean;
  rows: {
    row: number;
    team_name: string;
    team_admin: string;
    additional_members: string[];
    errors: string[];
    warnings?: string[];
  }[];
};
const template =
  'Team_name,Team_admin,Additional_members\nEngineering,leader@example.com,member@example.com;another@example.com\n';

export default function TeamImport({ onImported }: { onImported?: () => void } = {}) {
  const client = useQueryClient();
  const [csv, setCsv] = useState('');
  const [preview, setPreview] = useState<Preview | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [success, setSuccess] = useState('');
  const generation = useRef(0);
  const valid = preview?.valid && preview.rows.length > 0 && preview.rows.every((row) => !row.errors?.length);
  async function load(file?: File) {
    const version = ++generation.current;
    setCsv('');
    setPreview(null);
    setError('');
    setSuccess('');
    if (!file) return;
    if (file.size > 2 * 1024 * 1024) {
      setError('CSV exceeds the 2 MiB limit. Choose a smaller file.');
      return;
    }
    setBusy(true);
    try {
      const text = await new Promise<string>((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(String(reader.result));
        reader.onerror = () => reject(new Error('Could not read the file. Choose it again.'));
        reader.readAsText(file);
      });
      if (generation.current === version) setCsv(text);
    } catch (e) {
      if (generation.current === version) setError(e instanceof Error ? e.message : 'Could not read CSV.');
    } finally {
      if (generation.current === version) setBusy(false);
    }
  }
  async function submit(apply: boolean) {
    if (busy || !csv || (apply && !valid)) return;
    setBusy(true);
    setError('');
    setSuccess('');
    try {
      if (!apply) {
        setPreview(null);
        setPreview(await api.post<Preview>('/api/v1/admin/teams/import/preview', { csv }));
      } else {
        const result = await api.post<{ created: number; teams: { id: string }[] }>('/api/v1/admin/teams/import', { csv });
        setSuccess(`Created ${result.created} ${result.created === 1 ? 'team' : 'teams'}.`);
        setPreview(null);
        setCsv('');
        await Promise.all([
          client.invalidateQueries({ queryKey: ['admin', 'teams'] }),
          client.invalidateQueries({ queryKey: ['teams'] }),
        ]);
        onImported?.();
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Import failed. Correct the CSV and preview again.');
      if (apply) setPreview(null);
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="card stack" aria-label="CSV team import">
      <h2>Import teams from CSV</h2>
      <p>
        Use the exact headers Team_name,Team_admin,Additional_members. Team_admin is the leader’s email; separate additional
        member emails with semicolons. All emails must belong to active users. Maximum 2 MiB and 1,000 teams. All rows must pass
        validation; no teams are silently skipped.
      </p>
      <a className="btn" download="teams-template.csv" href={`data:text/csv;charset=utf-8,${encodeURIComponent(template)}`}>
        Download CSV template
      </a>
      <Field label="CSV file">
        <input type="file" accept=".csv,text/csv" disabled={busy} onChange={(e) => void load(e.target.files?.[0])} />
      </Field>
      {busy && <Spinner label="Processing CSV…" />}
      {error && (
        <div role="alert" className="banner banner-danger">
          {error}
        </div>
      )}
      {success && (
        <div role="status" className="banner banner-info">
          {success}
        </div>
      )}
      <button className="btn" disabled={busy || !csv} onClick={() => void submit(false)}>
        Preview import
      </button>
      {preview && (
        <>
          <p role="status">
            {valid
              ? `${preview.rows.length} rows ready to import.`
              : 'Correct every error and upload the CSV again. Nothing has been imported.'}
          </p>
          <div className="table-wrap">
            <table className="data">
              <thead>
                <tr>
                  <th>Row</th>
                  <th>Team</th>
                  <th>Leader email</th>
                  <th>Additional members</th>
                  <th>Validation</th>
                </tr>
              </thead>
              <tbody>
                {preview.rows.map((row) => (
                  <tr key={row.row}>
                    <td>{row.row}</td>
                    <td>{row.team_name}</td>
                    <td>{row.team_admin}</td>
                    <td>{row.additional_members?.join('; ') || 'None'}</td>
                    <td>
                      {!row.errors?.length && 'Ready'}
                      {row.errors?.map((message, i) => (
                        <p role="alert" key={i}>
                          {message}
                        </p>
                      ))}
                      {row.warnings?.map((message, i) => (
                        <p key={i}>Warning: {message}</p>
                      ))}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
      <button className="btn btn-primary" disabled={busy || !valid} onClick={() => void submit(true)}>
        Import teams
      </button>
    </section>
  );
}
