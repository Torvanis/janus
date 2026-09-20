import { Children, cloneElement, isValidElement, useState, useEffect, type ComponentProps, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, useSearchParams } from 'react-router-dom';
import { ReportResultView } from './ReportResultView';
import { useMe, useLocalOnly } from '../app/session';
import { api, ApiError } from '../lib/api';
import { AsyncSection, Field as BaseField } from '../components/ui';
import {
  activeRun,
  type ReportBudget,
  type ReportSchedule,
  type ReportRun,
  isCostMetric,
  type Catalog,
  type ReportDefinition,
  type SavedReport,
  type ReportOptions,
} from '../lib/reports';
import './reports.css';
import { ConfirmAction } from '../components/ConfirmAction';
import { Collection } from '../components/Collection';
// Keep native control names independent of wrapped options and hint text.
function Field({ children, label, ...props }: ComponentProps<typeof BaseField>) {
  return (
    <BaseField {...props} label={label}>
      {Children.map(children, (child) =>
        typeof label === 'string' &&
        isValidElement<{ 'aria-label'?: string }>(child) &&
        (child.type === 'select' || child.type === 'input' || child.type === 'textarea') &&
        child.props['aria-label'] === undefined
          ? cloneElement(child, { 'aria-label': label })
          : child,
      )}
    </BaseField>
  );
}
const ROOT = '/api/v1/reports';
// Template guidance is copy, not report data; definitions still come from the catalog.
const templateQuestions: Record<string, string> = {
  executive: 'What changed in activity, adoption, and service health?',
  usage: 'How is usage changing, and which models are driving it?',
  adoption: 'Who is using the service, and where is engagement growing?',
  portfolio: 'Which models are being used, and how does their performance compare?',
  quotas: 'Where is usage approaching the limits you have set?',
  reliability: 'Where are errors and slow responses affecting the experience?',
  efficiency: 'Where could token usage and caching be more efficient?',
  integrations: 'Which applications and integrations are driving activity?',
  governance: 'Where do access and policy signals need attention?',
  data_quality: 'Which records have gaps that could affect your conclusions?',
};
const runStatus: Record<ReportRun['status'], string> = {
  queued: 'Queued',
  running: 'Generating',
  complete: 'Ready',
  failed: 'Failed',
  cancelled: 'Cancelled',
  expired: 'Expired',
};
function readableDate(value?: string) {
  return value && Number.isFinite(Date.parse(value))
    ? new Date(value).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })
    : 'Date unavailable';
}
const zone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || 'UTC';
function localDate(iso?: string) {
  if (!iso) return '';
  const d = new Date(iso);
  if (!Number.isFinite(d.getTime())) return '';
  return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 16);
}
function errorMessage(error: unknown) {
  return error instanceof ApiError
    ? `${error.message}${error.payload.reason ? ` — ${error.payload.reason}` : ''}`
    : error instanceof Error
      ? error.message
      : 'Request failed. Please try again.';
}
function Timezone({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <Field label="Timezone">
      <input className="input" list="report-timezones" value={value} onChange={(e) => onChange(e.target.value)} />
      <datalist id="report-timezones">
        {[
          'UTC',
          'America/New_York',
          'America/Chicago',
          'America/Los_Angeles',
          'Europe/London',
          'Europe/Paris',
          'Asia/Tokyo',
          'Asia/Kolkata',
          'Australia/Sydney',
        ].map((z) => (
          <option key={z} value={z} />
        ))}
      </datalist>
    </Field>
  );
}
function Multi({
  label,
  values,
  options,
  onChange,
  size,
}: {
  label: string;
  values: string[];
  options: { id: string; label: string }[];
  onChange: (v: string[]) => void;
  size?: number;
}) {
  const unknown = values.filter((v) => !options.some((o) => o.id === v));
  return (
    <Field label={label}>
      <select
        className="input"
        multiple
        size={size}
        value={values}
        onChange={(e) => onChange(Array.from(e.target.selectedOptions, (o) => o.value))}
      >
        {options.map((o) => (
          <option key={o.id} value={o.id}>
            {o.label}
          </option>
        ))}
        {unknown.map((v) => (
          <option key={v} value={v}>
            Unavailable or deleted ({v || 'none'}) — preserved
          </option>
        ))}
      </select>
    </Field>
  );
}
function validateDefinition(d: ReportDefinition) {
  if (!d.name.trim()) throw new Error('Enter a report name.');
  if (!d.metrics.length) throw new Error('Select at least one metric.');
  if (d.dimensions.length > 3) throw new Error('Choose at most three groupings.');
  try {
    new Intl.DateTimeFormat('en', { timeZone: d.timezone });
  } catch {
    throw new Error('Enter a valid IANA timezone.');
  }
  if (
    d.period === 'custom' &&
    (!d.start ||
      !d.end ||
      !Number.isFinite(Date.parse(d.start)) ||
      !Number.isFinite(Date.parse(d.end)) ||
      Date.parse(d.start) >= Date.parse(d.end))
  )
    throw new Error('Start must be before end.');
  if (!Number.isFinite(d.scenario_discount_percent) || d.scenario_discount_percent < 0 || d.scenario_discount_percent > 100)
    throw new Error('Hypothetical discount must be between 0 and 100.');
}
function useReportTeams() {
  const me = useMe();
  const admin = me?.role === 'admin';
  const query = useQuery({
    queryKey: ['reports', 'admin-teams'],
    queryFn: () => api.get<{ teams: { id: string; name: string }[] }>('/api/v1/admin/teams'),
    enabled: admin,
  });
  const permitted = (me?.teams ?? []).filter((t) => admin || t.my_role === 'leader');
  const teams = admin
    ? Array.from(new Map([...permitted, ...(query.data?.teams ?? [])].map((t) => [t.id, t])).values())
    : permitted;
  return { teams, error: query.error, reload: query.refetch };
}
function nanoToDollars(value: number) {
  const n = BigInt(value);
  return `${n / 1000000000n}.${(n % 1000000000n).toString().padStart(9, '0')}`;
}
function dollarsToNano(value: string) {
  if (!/^\d+(?:\.\d{1,9})?$/.test(value)) throw new Error('Enter a non-negative USD amount with at most 9 decimal places.');
  const [whole, fraction = ''] = value.split('.');
  const nano = BigInt(whole!) * 1000000000n + BigInt(fraction.padEnd(9, '0'));
  if (nano > BigInt(Number.MAX_SAFE_INTEGER)) throw new Error('Amount exceeds the safely supported budget limit.');
  return Number(nano);
}
function Budgets() {
  const me = useMe();
  const admin = me?.role === 'admin';
  const { teams, error: teamError, reload: reloadTeams } = useReportTeams();
  const client = useQueryClient();
  const [editing, setEditing] = useState('');
  const [form, setForm] = useState({
    name: '',
    scope: 'user' as ReportBudget['scope'],
    subject_id: me?.id ?? '',
    amount: '',
    start: '',
    end: '',
  });
  const [notice, setNotice] = useState('');
  const query = useQuery({
    queryKey: ['reports', 'budgets'],
    queryFn: () => api.get<{ budgets: ReportBudget[] }>(`${ROOT}/budgets`),
  });
  const write = useMutation({
    mutationFn: async () => {
      if (!form.name.trim()) throw new Error('Enter a budget name.');
      const amount_nanousd = dollarsToNano(form.amount);
      if (
        !form.start ||
        !form.end ||
        !Number.isFinite(Date.parse(form.start)) ||
        !Number.isFinite(Date.parse(form.end)) ||
        Date.parse(form.start) >= Date.parse(form.end)
      )
        throw new Error('Budget start must be before budget end.');
      if (form.scope === 'organization' && !admin) throw new Error('Organization budgets require an administrator.');
      if (form.scope === 'team' && !teams.some((t) => t.id === form.subject_id)) throw new Error('Choose a team you lead.');
      const body = {
        name: form.name.trim(),
        scope: form.scope,
        subject_id: form.scope === 'user' ? (me?.id ?? '') : form.scope === 'organization' ? '' : form.subject_id,
        amount_nanousd,
        start: new Date(form.start).toISOString(),
        end: new Date(form.end).toISOString(),
      };
      return editing
        ? api.put<{ budget: ReportBudget }>(`${ROOT}/budgets/${encodeURIComponent(editing)}`, body)
        : api.post<{ budget: ReportBudget }>(`${ROOT}/budgets`, body);
    },
    onSuccess: () => {
      setNotice('Budget saved.');
      setEditing('');
      void client.invalidateQueries({ queryKey: ['reports', 'budgets'] });
    },
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.del(`${ROOT}/budgets/${encodeURIComponent(id)}`),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['reports', 'budgets'] });
    },
  });
  return (
    <section className="card stack">
      <h2>Budgets</h2>
      {teamError && (
        <div role="alert">
          {errorMessage(teamError)}
          <button className="btn" onClick={() => void reloadTeams()}>
            Reload teams
          </button>
        </div>
      )}
      <p className="secondary">
        Non-enforcing planning targets, independent of quotas. Budgets do not block requests or change billing.
      </p>
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          write.mutate();
        }}
      >
        <fieldset disabled={write.isPending} className="report-form-grid">
          <Field label="Budget name">
            <input className="input" value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
          </Field>
          <Field label="Budget scope">
            <select
              className="input"
              value={form.scope}
              onChange={(e) =>
                setForm({
                  ...form,
                  scope: e.target.value as ReportBudget['scope'],
                  subject_id: e.target.value === 'user' ? (me?.id ?? '') : '',
                })
              }
            >
              <option value="user">Only me</option>
              {teams.length > 0 && <option value="team">Team</option>}
              {admin && <option value="organization">Organization</option>}
            </select>
          </Field>
          {form.scope === 'team' && (
            <Field label="Budget team">
              <select
                className="input"
                value={form.subject_id}
                onChange={(e) => setForm({ ...form, subject_id: e.target.value })}
              >
                <option value="">Choose a team</option>
                {teams.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name}
                  </option>
                ))}
                {form.subject_id && !teams.some((t) => t.id === form.subject_id) && (
                  <option value={form.subject_id} disabled>
                    Unavailable team
                  </option>
                )}
              </select>
            </Field>
          )}
          <Field label="Amount (USD)">
            <input
              className="input"
              inputMode="decimal"
              value={form.amount}
              onChange={(e) => setForm({ ...form, amount: e.target.value })}
            />
          </Field>
          <Field label="Budget start">
            <input
              className="input"
              type="datetime-local"
              value={form.start}
              onChange={(e) => setForm({ ...form, start: e.target.value })}
            />
          </Field>
          <Field label="Budget end">
            <input
              className="input"
              type="datetime-local"
              value={form.end}
              onChange={(e) => setForm({ ...form, end: e.target.value })}
            />
          </Field>
        </fieldset>
        <p className="field-hint">Date inputs use your device timezone ({zone()}).</p>
        {(write.error || remove.error) && <p role="alert">{errorMessage(write.error || remove.error)}</p>}
        {notice && <p role="status">{notice}</p>}
        <div className="row">
          <button className="btn btn-primary" disabled={write.isPending}>
            {editing ? 'Update budget' : 'Create budget'}
          </button>
          {editing && (
            <button
              className="btn"
              type="button"
              onClick={() => {
                setEditing('');
                setForm({ ...form, name: '', amount: '' });
              }}
            >
              Cancel budget edit
            </button>
          )}
        </div>
      </form>
      <AsyncSection query={query}>
        {(data) => (
          <>
            {!data.budgets.length && <p>No budgets yet.</p>}
            <Collection
              name="Planning budgets"
              rows={data.budgets}
              rowKey={(b) => b.id}
              columns={[
                {
                  id: 'name',
                  label: 'Name / details',
                  value: (b) => b.name,
                  render: (b) => (
                    <div>
                      <h3>{b.name}</h3>
                      <p>
                        {b.scope === 'user'
                          ? 'Personal'
                          : b.scope === 'organization'
                            ? 'Organization'
                            : (teams.find((t) => t.id === b.subject_id)?.name ?? 'Unavailable team')}{' '}
                        ·{' '}
                        {(b.amount_nanousd / 1e9).toLocaleString(undefined, {
                          style: 'currency',
                          currency: 'USD',
                          maximumFractionDigits: 9,
                        })}
                      </p>
                      <p>
                        {b.start} — {b.end}
                      </p>
                    </div>
                  ),
                },
                {
                  id: 'amount',
                  label: 'Amount (USD)',
                  value: (b) => b.amount_nanousd,
                  render: (b) => nanoToDollars(b.amount_nanousd),
                },
                {
                  id: 'actions',
                  label: 'Actions',
                  render: (b) => (
                    <div className="row wrap">
                      <div className="row">
                        <button
                          className="btn"
                          disabled={write.isPending}
                          onClick={() => {
                            setEditing(b.id);
                            setNotice('');
                            setForm({
                              name: b.name,
                              scope: b.scope,
                              subject_id: b.subject_id,
                              amount: nanoToDollars(b.amount_nanousd),
                              start: localDate(b.start),
                              end: localDate(b.end),
                            });
                          }}
                        >
                          Edit budget
                        </button>
                        <ConfirmAction
                          consequence="Delete this planning budget? This does not change quotas."
                          busy={remove.isPending}
                          onConfirm={() => remove.mutate(b.id)}
                        >
                          Delete budget
                        </ConfirmAction>
                      </div>
                    </div>
                  ),
                },
              ]}
            />
          </>
        )}
      </AsyncSection>
    </section>
  );
}
type ScheduleInput = Pick<ReportSchedule, 'report_id' | 'frequency' | 'timezone' | 'at' | 'weekday' | 'monthday' | 'enabled'>;
function scheduleInput(s: ScheduleInput): ScheduleInput {
  return {
    report_id: s.report_id,
    frequency: s.frequency,
    timezone: s.timezone,
    at: s.at,
    weekday: s.weekday,
    monthday: s.monthday,
    enabled: s.enabled,
  };
}
function Schedules({
  reports,
  timezone,
  initialReport = '',
}: {
  reports: SavedReport[];
  timezone: string;
  initialReport?: string;
}) {
  const client = useQueryClient();
  const [editing, setEditing] = useState('');
  const [form, setForm] = useState<ScheduleInput>({
    report_id: initialReport,
    frequency: 'daily',
    timezone,
    at: '09:00',
    weekday: 1,
    monthday: 1,
    enabled: true,
  });
  const [notice, setNotice] = useState('');
  const query = useQuery({
    queryKey: ['reports', 'schedules'],
    queryFn: () => api.get<{ schedules: ReportSchedule[] }>(`${ROOT}/schedules`),
  });
  const write = useMutation({
    mutationFn: async ({ id, value }: { id?: string; value: ScheduleInput }) => {
      if (!reports.some((r) => r.id === value.report_id)) throw new Error('Choose a saved report that you own.');
      try {
        new Intl.DateTimeFormat('en', { timeZone: value.timezone });
      } catch {
        throw new Error('Enter a valid IANA timezone.');
      }
      if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(value.at)) throw new Error('Choose a valid schedule time.');
      if (
        !Number.isInteger(value.weekday) ||
        value.weekday < 0 ||
        value.weekday > 6 ||
        !Number.isInteger(value.monthday) ||
        value.monthday < 1 ||
        value.monthday > 28
      )
        throw new Error('Choose a weekday or a month day from 1 to 28.');
      return id
        ? api.put<{ schedule: ReportSchedule }>(`${ROOT}/schedules/${encodeURIComponent(id)}`, scheduleInput(value))
        : api.post<{ schedule: ReportSchedule }>(`${ROOT}/schedules`, scheduleInput(value));
    },
    onSuccess: () => {
      setNotice('Schedule saved.');
      setEditing('');
      void client.invalidateQueries({ queryKey: ['reports', 'schedules'] });
    },
  });
  const remove = useMutation({
    mutationFn: (id: string) => api.del(`${ROOT}/schedules/${encodeURIComponent(id)}`),
    onSuccess: () => {
      void client.invalidateQueries({ queryKey: ['reports', 'schedules'] });
    },
  });
  return (
    <section className="card stack">
      <h2>Schedules</h2>
      <p className="secondary">
        Authenticated in-app delivery to the schedule owner only. No email delivery is connected. Only saved reports you own can
        be scheduled.
      </p>
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          write.mutate({ id: editing || undefined, value: form });
        }}
      >
        <fieldset disabled={write.isPending} className="report-form-grid">
          <Field label="Saved report">
            <select className="input" value={form.report_id} onChange={(e) => setForm({ ...form, report_id: e.target.value })}>
              <option value="">Choose a report</option>
              {reports.map((r) => (
                <option key={r.id} value={r.id}>
                  {r.definition.name}
                </option>
              ))}
              {form.report_id && !reports.some((r) => r.id === form.report_id) && (
                <option value={form.report_id}>Unavailable report</option>
              )}
            </select>
          </Field>
          <Field label="Frequency">
            <select
              className="input"
              value={form.frequency}
              onChange={(e) => setForm({ ...form, frequency: e.target.value as ReportSchedule['frequency'] })}
            >
              {['daily', 'weekly', 'monthly'].map((f) => (
                <option key={f} value={f}>
                  {f}
                </option>
              ))}
            </select>
          </Field>
          <Timezone value={form.timezone} onChange={(v) => setForm({ ...form, timezone: v })} />
          <Field label="At time">
            <input className="input" type="time" value={form.at} onChange={(e) => setForm({ ...form, at: e.target.value })} />
          </Field>
          {form.frequency === 'weekly' && (
            <Field label="Weekday">
              <select
                className="input"
                value={form.weekday}
                onChange={(e) => setForm({ ...form, weekday: Number(e.target.value) })}
              >
                {['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'].map((d, i) => (
                  <option key={d} value={i}>
                    {d}
                  </option>
                ))}
              </select>
            </Field>
          )}
          {form.frequency === 'monthly' && (
            <Field label="Month day">
              <input
                className="input"
                type="number"
                min="1"
                max="28"
                value={form.monthday}
                onChange={(e) => setForm({ ...form, monthday: Number(e.target.value) })}
              />
            </Field>
          )}
          <label className="row">
            <input type="checkbox" checked={form.enabled} onChange={(e) => setForm({ ...form, enabled: e.target.checked })} />
            Enabled
          </label>
        </fieldset>
        {(write.error || remove.error) && <p role="alert">{errorMessage(write.error || remove.error)}</p>}
        {notice && <p role="status">{notice}</p>}
        <div className="row">
          <button className="btn btn-primary" disabled={write.isPending || !reports.length}>
            {editing ? 'Update schedule' : 'Create schedule'}
          </button>
          {editing && (
            <button
              type="button"
              className="btn"
              onClick={() => {
                setEditing('');
                setForm({ ...form, report_id: '' });
              }}
            >
              Cancel schedule edit
            </button>
          )}
        </div>
        {!reports.length && <p>Save a report you own to create a schedule.</p>}
      </form>
      <AsyncSection query={query}>
        {(data) => (
          <>
            {!data.schedules.length && <p>No schedules yet.</p>}
            <Collection
              name="Report schedules"
              rows={data.schedules}
              rowKey={(s) => s.id}
              columns={[
                {
                  id: 'name',
                  label: 'Name / details',
                  value: (s) => reports.find((r) => r.id === s.report_id)?.definition.name ?? 'Unavailable report',
                  render: (s) => (
                    <div>
                      <h3>{reports.find((r) => r.id === s.report_id)?.definition.name ?? 'Unavailable report'}</h3>
                      <p>
                        {s.frequency} at {s.at} ({s.timezone}) · {s.enabled ? 'Enabled' : 'Paused'}
                      </p>
                      <p>Next run: {s.next_run_at || 'Not scheduled'}</p>
                      {(s.error || s.last_error) && <p role="alert">{s.error || s.last_error}</p>}
                    </div>
                  ),
                },
                {
                  id: 'status',
                  label: 'Status',
                  value: (s) => (s.enabled ? 'Enabled' : 'Paused'),
                  render: (s) => (s.enabled ? 'Enabled' : 'Paused'),
                },
                { id: 'next', label: 'Next run', value: (s) => s.next_run_at, render: (s) => s.next_run_at || '—' },
                {
                  id: 'actions',
                  label: 'Actions',
                  render: (s) => (
                    <div className="row wrap">
                      <div className="row">
                        <button
                          className="btn"
                          disabled={write.isPending}
                          onClick={() => write.mutate({ id: s.id, value: { ...scheduleInput(s), enabled: !s.enabled } })}
                        >
                          {s.enabled ? 'Pause schedule' : 'Resume schedule'}
                        </button>
                        <button
                          className="btn"
                          disabled={write.isPending}
                          onClick={() => {
                            setEditing(s.id);
                            setForm(scheduleInput(s));
                            setNotice('');
                          }}
                        >
                          Edit schedule
                        </button>
                        <ConfirmAction
                          consequence="Delete this schedule? Future automatic runs will stop."
                          busy={remove.isPending}
                          onConfirm={() => remove.mutate(s.id)}
                        >
                          Delete schedule
                        </ConfirmAction>
                      </div>
                    </div>
                  ),
                },
              ]}
            />
          </>
        )}
      </AsyncSection>
    </section>
  );
}
function RunElapsed({ run }: { run: ReportRun }) {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);
  const start = Date.parse(run.started_at || run.created_at);
  return (
    <p>
      {Number.isFinite(start) ? `Elapsed ${Math.max(0, Math.floor((now - start) / 1000))}s` : 'Waiting for start time'} · Stage:{' '}
      {run.status}
    </p>
  );
}
export default function Reports() {
  const me = useMe();
  const localOnly = useLocalOnly() || me?.local_only === true;
  const client = useQueryClient();
  const [params, setParams] = useSearchParams();
  const runID = params.get('run') ?? '';
  const openRun = (id: string) => {
    setParams((p) => {
      const next = new URLSearchParams(p);
      if (id) next.set('run', id);
      else next.delete('run');
      return next;
    });
  };
  const requestedTab = params.get('tab') ?? 'Library';
  const tab = ['Library', 'Builder', 'Runs', 'Schedules', ...(!localOnly ? ['Budgets'] : [])].includes(requestedTab)
    ? requestedTab
    : 'Library';
  const setTab = (next: string) =>
    setParams((current) => {
      const p = new URLSearchParams(current);
      p.set('tab', next);
      return p;
    });
  const [scheduleReport, setScheduleReport] = useState('');
  const [draft, setDraft] = useState<ReportDefinition>();
  const [addedFilters, setAddedFilters] = useState<string[]>([]);
  const visibleFilters = Array.from(
    new Set([
      ...(draft?.dimensions ?? []),
      ...Object.keys(draft?.filters ?? {}).filter((id) => draft?.filters[id]?.length),
      ...addedFilters,
    ]),
  );
  const [editing, setEditing] = useState<SavedReport>();
  const [shared, setShared] = useState(false);
  const [message, setMessage] = useState('');
  const catalog = useQuery({ queryKey: ['reports', 'catalog', localOnly], queryFn: () => api.get<Catalog>(`${ROOT}/catalog`) });
  const saved = useQuery({ queryKey: ['reports', 'library'], queryFn: () => api.get<{ reports: SavedReport[] }>(ROOT) });
  const optionScope = draft?.scope ?? 'self';
  const optionTeam = optionScope === 'team' ? (draft?.team_id ?? '') : '';
  const options = useQuery({
    queryKey: ['reports', 'options', optionScope, optionTeam],
    queryFn: () =>
      api.get<ReportOptions>(
        `${ROOT}/options?${new URLSearchParams({ scope: optionScope, ...(optionTeam ? { team_id: optionTeam } : {}) })}`,
      ),
    enabled: tab === 'Builder' && !!draft && (optionScope !== 'team' || !!optionTeam),
  });
  const admin = me?.role === 'admin';
  const { teams, error: teamError, reload: reloadTeams } = useReportTeams();
  const metrics = (catalog.data?.metrics ?? []).filter((m) => !localOnly || !isCostMetric(m.id, m.unit));
  const change = (patch: Partial<ReportDefinition>) => {
    setDraft((d) => (d ? { ...d, ...patch } : d));
    setMessage('');
  };
  const openDefinition = (d: ReportDefinition, report?: SavedReport) => {
    setAddedFilters([]);
    setDraft({
      ...d,
      dimensions: [...d.dimensions],
      metrics: d.metrics.filter((id) => !localOnly || !isCostMetric(id, catalog.data?.metrics.find((m) => m.id === id)?.unit)),
      filters: { ...d.filters },
      sections: [...(d.sections ?? [])],
      scenario_discount_percent: localOnly ? 0 : (d.scenario_discount_percent ?? 0),
    });
    setEditing(report?.owner_user_id === me?.id ? report : undefined);
    setShared(report && report.owner_user_id === me?.id ? report.shared : false);
    setMessage('');
    setParams((current) => {
      const p = new URLSearchParams(current);
      p.set('tab', 'Builder');
      p.delete('run');
      return p;
    });
  };
  const payload = () => {
    if (!draft) throw new Error('Choose a template first.');
    const d = {
      ...draft,
      name: draft.name.trim(),
      filters: Object.fromEntries(Object.entries(draft.filters).filter(([, values]) => values.length > 0)),
      metrics: draft.metrics.filter(
        (id) => !localOnly || !isCostMetric(id, catalog.data?.metrics.find((m) => m.id === id)?.unit),
      ),
      scenario_discount_percent: localOnly ? 0 : draft.scenario_discount_percent,
    };
    validateDefinition(d);
    if (d.scope === 'organization' && !admin)
      throw new Error('Organization scope requires an administrator. Choose an authorized scope.');
    if (d.scope === 'team' && !teams.some((t) => t.id === d.team_id)) throw new Error('Choose a team you lead.');
    return d;
  };
  let validationError = '';
  if (draft) {
    try {
      payload();
    } catch (error) {
      validationError = errorMessage(error);
    }
  }
  const save = useMutation({
    mutationFn: async () => {
      const definition = payload();
      return editing
        ? api.put<{ report: SavedReport }>(`${ROOT}/${encodeURIComponent(editing.id)}`, {
            definition,
            shared,
            revision: editing.revision,
          })
        : api.post<{ report: SavedReport }>(ROOT, { definition, shared });
    },
    onSuccess: ({ report }) => {
      setEditing(report);
      setMessage('Report saved.');
      void client.invalidateQueries({ queryKey: ['reports', 'library'] });
    },
  });
  const removeReport = useMutation({
    mutationFn: (id: string) => api.del(`${ROOT}/${encodeURIComponent(id)}`),
    onSuccess: (_data, id) => {
      if (editing?.id === id) setEditing(undefined);
      void client.invalidateQueries({ queryKey: ['reports', 'library'] });
      void client.invalidateQueries({ queryKey: ['reports', 'schedules'] });
    },
  });
  const runs = useQuery({
    queryKey: ['reports', 'runs'],
    queryFn: () => api.get<{ runs: ReportRun[] }>(`${ROOT}/runs`),
    enabled: tab === 'Runs' || tab === 'Library',
    refetchInterval: (q) => (q.state.data?.runs.some(activeRun) ? 2000 : false),
  });
  const latestRuns = new Map<string, ReportRun>();
  for (const item of runs.data?.runs ?? []) {
    if (!item.report_id) continue;
    const previous = latestRuns.get(item.report_id);
    if (!previous || Date.parse(item.created_at) > Date.parse(previous.created_at)) latestRuns.set(item.report_id, item);
  }
  const currentRun = useQuery({
    queryKey: ['reports', 'run', runID],
    queryFn: () => api.get<{ run: ReportRun }>(`${ROOT}/runs/${encodeURIComponent(runID)}`),
    enabled: !!runID,
    retry: false,
    refetchInterval: (q) =>
      q.state.error instanceof ApiError && [401, 403, 404].includes(q.state.error.status)
        ? false
        : activeRun(q.state.data?.run)
          ? 2000
          : false,
  });
  const run = useMutation({
    mutationFn: async () =>
      api.post<{ run: ReportRun }>(`${ROOT}/runs`, { definition: payload(), ...(editing ? { report_id: editing.id } : {}) }),
    onSuccess: ({ run }) => {
      client.setQueryData(['reports', 'run', run.id], { run });
      openRun(run.id);
      void client.invalidateQueries({ queryKey: ['reports', 'runs'] });
    },
  });
  const runAction = useMutation({
    mutationFn: ({ id, action }: { id: string; action: 'cancel' | 'retry' }) =>
      api.post<{ run: ReportRun }>(`${ROOT}/runs/${encodeURIComponent(id)}/${action}`, {}),
    onSuccess: ({ run }) => {
      if (run) {
        client.setQueryData(['reports', 'run', run.id], { run });
        openRun(run.id);
      }
      void client.invalidateQueries({ queryKey: ['reports', 'run'] });
      void client.invalidateQueries({ queryKey: ['reports', 'runs'] });
    },
  });
  const reload = async () => {
    const result = await saved.refetch();
    const fresh = result.data?.reports.find((r) => r.id === editing?.id);
    if (fresh && window.confirm('Reload the saved definition? This replaces your draft edits.')) {
      openDefinition(fresh.definition, fresh);
      save.reset();
    }
  };
  const feedback = (error: unknown): ReactNode =>
    error ? (
      <div role="alert" className="banner banner-danger">
        {errorMessage(error)}
      </div>
    ) : null;
  return (
    <section className="reports-workspace stack">
      {!runID && (
        <header>
          <p className="overline">Analytics workspace</p>
          <div className="row-between">
            <h1>Reports</h1>
            {admin && (
              <Link className="btn" to="/reports/classifications">
                Model labels
              </Link>
            )}
          </div>
          <p className="secondary">Start from a template, refine your scope, and keep a reproducible report.</p>
        </header>
      )}
      {!runID && (
        <nav className="report-tabs" aria-label="Report workspace">
          {['Library', 'Builder', 'Runs', 'Schedules', ...(!localOnly ? ['Budgets'] : [])].map((t) => (
            <button
              key={t}
              className={`btn ${tab === t ? 'btn-primary' : 'btn-ghost'}`}
              aria-pressed={tab === t}
              onClick={() => setTab(t)}
            >
              {t}
            </button>
          ))}
        </nav>
      )}
      {!!runID && (
        <section className="stack report-run" aria-label="Current run">
          <div className="row-between report-reader-toolbar">
            <button className="btn btn-ghost" onClick={() => openRun('')}>
              Back to reports
            </button>
            <div className="row">
              {currentRun.data && (
                <button
                  className="btn btn-ghost"
                  onClick={() => {
                    openDefinition(currentRun.data.run.definition);
                  }}
                >
                  Open run definition
                </button>
              )}
              <button className="btn btn-ghost" disabled={currentRun.isFetching} onClick={() => void currentRun.refetch()}>
                Refresh run details
              </button>
            </div>
          </div>
          {currentRun.data && feedback(currentRun.error)}
          <AsyncSection query={currentRun}>
            {({ run: r }) => (
              <>
                <div className="report-reader-meta">
                  <p role="status" className="report-run-status">
                    {runStatus[r.status]}
                  </p>
                  <span className="secondary">Created {readableDate(r.created_at)}</span>
                  <details className="report-run-information">
                    <summary>Run information</summary>
                    <p>Attempt {r.attempts ?? 1}</p>
                    {r.expires_at && <p>Result expires {readableDate(r.expires_at)}</p>}
                  </details>
                </div>
                {!(r.status === 'complete' && r.result) && <h1>{r.definition.name}</h1>}
                {activeRun(r) && (
                  <>
                    <RunElapsed run={r} />
                    <p>Processing continues on the server. You can leave this page and return using this URL.</p>
                    <ConfirmAction
                      consequence="Cancel this report run?"
                      busy={runAction.isPending}
                      onConfirm={() => runAction.mutate({ id: r.id, action: 'cancel' })}
                    >
                      Cancel run
                    </ConfirmAction>
                  </>
                )}
                {r.error && <p role="alert">{r.error}</p>}
                {(r.status === 'failed' || r.status === 'cancelled') && (
                  <button
                    className="btn"
                    disabled={runAction.isPending}
                    onClick={() => runAction.mutate({ id: r.id, action: 'retry' })}
                  >
                    Retry as new run
                  </button>
                )}
                {r.status === 'expired' && <p>This result has expired. Open its definition to generate a fresh report.</p>}
                {r.status === 'complete' &&
                  (r.result ? (
                    <ReportResultView
                      result={r.result}
                      runID={r.id}
                      drillableDimensions={catalog.data?.dimensions.map((item) => item.id)}
                      onDrillDown={(dimension, value) => {
                        if (!catalog.data?.dimensions.some((item) => item.id === dimension)) return;
                        openDefinition({ ...r.definition, filters: { ...r.definition.filters, [dimension]: [value] } });
                        setMessage('Report narrowed to the selected value. The original scope and period are preserved.');
                      }}
                    />
                  ) : (
                    <p>No result was returned. Refresh run details or generate a new run.</p>
                  ))}
              </>
            )}
          </AsyncSection>
          {feedback(runAction.error)}
        </section>
      )}
      {!runID && tab === 'Budgets' && !localOnly && <Budgets />}
      {!runID && tab === 'Schedules' && (
        <AsyncSection query={saved}>
          {(data) => (
            <Schedules
              initialReport={scheduleReport}
              reports={data.reports.filter((r) => r.owner_user_id === me?.id)}
              timezone={me?.timezone || zone()}
            />
          )}
        </AsyncSection>
      )}
      {!runID && tab === 'Runs' && (
        <section className="card stack">
          <h2>Run history</h2>
          <p className="secondary">Runs are private, durable server jobs. A retry always creates a new run.</p>
          <AsyncSection query={runs}>
            {(data) => (
              <>
                {!data.runs.length && <p>No report runs yet. Run a template to get started.</p>}
                <Collection
                  name="Report runs"
                  rows={data.runs}
                  rowKey={(r) => r.id}
                  columns={[
                    {
                      id: 'name',
                      label: 'Name / details',
                      value: (r) => r.definition.name,
                      render: (r) => (
                        <div>
                          <h3>{r.definition.name}</h3>
                          <p>
                            {runStatus[r.status]} · {readableDate(r.created_at)}
                          </p>
                          {r.error && <p>{r.error}</p>}
                        </div>
                      ),
                    },
                    { id: 'status', label: 'Status', value: (r) => r.status, render: (r) => runStatus[r.status] },
                    { id: 'created', label: 'Created', value: (r) => r.created_at, render: (r) => readableDate(r.created_at) },
                    {
                      id: 'actions',
                      label: 'Actions',
                      render: (r) => (
                        <div className="row wrap">
                          <button
                            className="btn"
                            aria-label={`Open report: ${r.definition.name} · ${readableDate(r.created_at)}`}
                            onClick={() => openRun(r.id)}
                          >
                            Open report
                          </button>
                        </div>
                      ),
                    },
                  ]}
                />
              </>
            )}
          </AsyncSection>
        </section>
      )}
      {!runID && tab === 'Library' && (
        <>
          <section className="card stack">
            <h2>Saved reports</h2>
            {feedback(removeReport.error)}
            <p className="secondary">Shared definitions do not share data. Each run checks your access.</p>
            <AsyncSection query={saved}>
              {(s) => (
                <>
                  {!s.reports.length && <p>No saved reports yet. Start with a template.</p>}
                  <Collection
                    name="Saved reports"
                    rows={s.reports}
                    rowKey={(r) => r.id}
                    columns={[
                      {
                        id: 'name',
                        label: 'Name / details',
                        value: (r) => r.definition.name,
                        render: (r) => (
                          <div>
                            <h3>{r.definition.name}</h3>
                            <p>
                              {r.definition.scope === 'self'
                                ? 'Only me'
                                : r.definition.scope === 'organization'
                                  ? 'Organization'
                                  : (teams.find((t) => t.id === r.definition.team_id)?.name ?? 'Unavailable team')}{' '}
                              · {r.definition.period.replaceAll('_', ' ')}
                            </p>
                            <p className="secondary">
                              {r.shared ? 'Shared definition' : 'Private definition'} · Revision {r.revision}
                            </p>
                            {latestRuns.has(r.id) && (
                              <p className="secondary">
                                Last report: {runStatus[latestRuns.get(r.id)!.status]} ·{' '}
                                {readableDate(latestRuns.get(r.id)!.created_at)}
                              </p>
                            )}
                          </div>
                        ),
                      },
                      {
                        id: 'actions',
                        label: 'Actions',
                        render: (r) => (
                          <div className="row wrap">
                            <button className="btn" onClick={() => openDefinition(r.definition, r)}>
                              {r.owner_user_id === me?.id ? 'Edit' : 'Use'} {r.definition.name}
                            </button>
                            {r.owner_user_id === me?.id && (
                              <>
                                <button
                                  className="btn"
                                  onClick={() => {
                                    setScheduleReport(r.id);
                                    setTab('Schedules');
                                  }}
                                >
                                  Schedule {r.definition.name}
                                </button>
                                <ConfirmAction
                                  consequence={`Delete ${r.definition.name}? This removes the saved definition and may stop its schedules.`}
                                  busy={removeReport.isPending}
                                  onConfirm={() => removeReport.mutate(r.id)}
                                >{`Delete ${r.definition.name}`}</ConfirmAction>
                              </>
                            )}
                          </div>
                        ),
                      },
                    ]}
                  />
                </>
              )}
            </AsyncSection>
          </section>
          <section className="card stack">
            <h2>Built-in templates</h2>
            <AsyncSection query={catalog}>
              {(c) => (
                <div className="report-grid">
                  {c.templates.map((t) => (
                    <article className="report-template stack" key={t.template}>
                      <h3>{t.name}</h3>
                      <p className="secondary">
                        {templateQuestions[t.template] ??
                          `What does ${t.name.toLowerCase()} reveal for this audience and period?`}
                      </p>
                      <button className="btn" onClick={() => openDefinition(t)}>
                        Use {t.name}
                      </button>
                    </article>
                  ))}
                  {!c.templates.length && <p>No templates are available.</p>}
                </div>
              )}
            </AsyncSection>
          </section>
        </>
      )}
      {!runID &&
        tab === 'Builder' &&
        (!draft ? (
          <section className="card">
            <h2>Choose a starting point</h2>
            <p>Select a template or a saved definition from your library.</p>
            <button className="btn" onClick={() => setTab('Library')}>
              Browse templates
            </button>
          </section>
        ) : (
          <section className="card stack report-builder">
            <div>
              <p className="overline">Set up your report</p>
              <h2>{draft.name || 'Untitled report'}</h2>
              <p className="secondary">Choose whose activity to include and a date range. Template settings are ready to use.</p>
            </div>
            {teamError && (
              <div role="alert">
                {errorMessage(teamError)}
                <button className="btn" onClick={() => void reloadTeams()}>
                  Reload teams
                </button>
              </div>
            )}
            <div className="report-form-grid">
              <Field label="Report name">
                <input className="input" value={draft.name} onChange={(e) => change({ name: e.target.value })} />
              </Field>
              <Field label="Scope">
                <select
                  className="input"
                  value={draft.scope}
                  onChange={(e) => change({ scope: e.target.value as ReportDefinition['scope'], team_id: '' })}
                >
                  <option value="self">Only me</option>
                  {teams.length > 0 && <option value="team">Team</option>}
                  {admin && <option value="organization">Organization</option>}
                  {draft.scope === 'organization' && !admin && (
                    <option value="organization" disabled>
                      Unavailable scope — choose another
                    </option>
                  )}
                  {draft.scope === 'team' && !teams.length && (
                    <option value="team" disabled>
                      Unavailable scope — choose another
                    </option>
                  )}
                </select>
              </Field>
              {draft.scope === 'team' && (
                <Field label="Team">
                  <select className="input" value={draft.team_id ?? ''} onChange={(e) => change({ team_id: e.target.value })}>
                    <option value="">Choose a team</option>
                    {teams.map((t) => (
                      <option key={t.id} value={t.id}>
                        {t.name}
                      </option>
                    ))}
                    {draft.team_id && !teams.some((t) => t.id === draft.team_id) && (
                      <option value={draft.team_id} disabled>
                        Unavailable team — choose another
                      </option>
                    )}
                  </select>
                </Field>
              )}
              <Field label="Period">
                <select
                  className="input"
                  value={draft.period}
                  onChange={(e) => change({ period: e.target.value as ReportDefinition['period'], start: '', end: '' })}
                >
                  {['last_7_days', 'last_30_days', 'previous_month', 'custom'].map((p) => (
                    <option key={p} value={p}>
                      {p.replaceAll('_', ' ')}
                    </option>
                  ))}
                </select>
              </Field>
              {draft.period === 'custom' && (
                <>
                  <Field label="Start">
                    <input
                      className="input"
                      type="datetime-local"
                      value={localDate(draft.start)}
                      onChange={(e) => change({ start: e.target.value ? new Date(e.target.value).toISOString() : '' })}
                    />
                  </Field>
                  <Field label="End">
                    <input
                      className="input"
                      type="datetime-local"
                      value={localDate(draft.end)}
                      onChange={(e) => change({ end: e.target.value ? new Date(e.target.value).toISOString() : '' })}
                    />
                  </Field>
                  <p className="field-hint">
                    Custom date inputs use your device timezone ({zone()}); the reporting timezone controls calendar buckets.
                  </p>
                </>
              )}
            </div>
            {validationError && (
              <p role="alert" id="report-validation">
                {validationError}
              </p>
            )}
            {feedback(save.error)}
            {feedback(run.error)}
            {save.error instanceof ApiError && save.error.status === 409 && (
              <>
                <p>
                  Your draft is preserved. Reload the latest revision before saving again; it will not be overwritten
                  automatically.
                </p>
                <button className="btn" onClick={() => void reload()}>
                  Reload latest revision
                </button>
              </>
            )}
            {message && <p role="status">{message}</p>}
            <div className="row">
              <button
                className="btn btn-primary"
                disabled={!!validationError || run.isPending || save.isPending}
                aria-describedby={validationError ? 'report-validation' : undefined}
                onClick={() => run.mutate()}
              >
                Generate report
              </button>
              <button
                className="btn"
                disabled={
                  !!validationError ||
                  run.isPending ||
                  save.isPending ||
                  (save.error instanceof ApiError && save.error.status === 409)
                }
                onClick={() => save.mutate()}
              >
                {editing ? 'Update report' : 'Save report'}
              </button>
            </div>
            <details className="report-customize">
              <summary>Customize report</summary>
              <div className="stack report-customize-body">
                <h3>Timing and comparison</h3>
                <div className="report-form-grid">
                  <Timezone value={draft.timezone || zone()} onChange={(timezone) => change({ timezone })} />
                  <Field label="Group membership">
                    <select
                      className="input"
                      value={draft.group_mode}
                      onChange={(e) => change({ group_mode: e.target.value as ReportDefinition['group_mode'] })}
                    >
                      <option value="historical">Historical membership</option>
                      <option value="current">Current membership</option>
                    </select>
                  </Field>
                </div>
                <p className="secondary">
                  Historical membership uses attribution recorded at request time and may be unavailable for older data. Current
                  membership uses today's groups. Membership can overlap: group totals may not add up to the overall total.
                </p>
                <label className="row">
                  <input type="checkbox" checked={draft.compare} onChange={(e) => change({ compare: e.target.checked })} />
                  Compare with previous period
                </label>
                <h3>Content and groupings</h3>
                <div className="report-form-grid">
                  {[0, 1, 2].map((i) => (
                    <Field key={i} label={`Grouping ${i + 1}`}>
                      <select
                        className="input"
                        value={draft.dimensions[i] ?? ''}
                        onChange={(e) => {
                          const dimensions = [...draft.dimensions];
                          dimensions[i] = e.target.value;
                          change({ dimensions: dimensions.filter(Boolean) });
                        }}
                      >
                        <option value="">None</option>
                        {(catalog.data?.dimensions ?? []).map((d) => (
                          <option
                            key={d.id}
                            value={d.id}
                            disabled={draft.dimensions.includes(d.id) && draft.dimensions[i] !== d.id}
                          >
                            {d.label}
                          </option>
                        ))}
                        {draft.dimensions[i] && !catalog.data?.dimensions.some((d) => d.id === draft.dimensions[i]) && (
                          <option value={draft.dimensions[i]}>Unavailable — preserved</option>
                        )}
                      </select>
                    </Field>
                  ))}
                </div>
                <Multi
                  label="Metrics"
                  values={draft.metrics.filter(
                    (id) => !localOnly || !isCostMetric(id, catalog.data?.metrics.find((m) => m.id === id)?.unit),
                  )}
                  options={metrics}
                  onChange={(v) => change({ metrics: v })}
                />
                <Multi
                  label="Report sections"
                  values={draft.sections}
                  options={(catalog.data?.templates ?? []).map((t) => ({ id: t.template, label: t.name }))}
                  onChange={(v) => change({ sections: v })}
                />
                <h3>Named filters</h3>
                <p className="secondary">
                  Leave a filter unselected for all permitted values. Deleted values remain selected until you explicitly remove
                  them.
                </p>
                <AsyncSection query={options}>
                  {(o) => (
                    <>
                      <Field label="Add filter">
                        <select
                          className="input"
                          value=""
                          onChange={(e) => {
                            if (e.target.value) setAddedFilters((ids) => [...ids, e.target.value]);
                          }}
                        >
                          <option value="">Choose a dimension</option>
                          {(catalog.data?.dimensions ?? [])
                            .filter((d) => !visibleFilters.includes(d.id))
                            .map((d) => (
                              <option key={d.id} value={d.id}>
                                {d.label}
                              </option>
                            ))}
                        </select>
                      </Field>
                      <div className="report-form-grid report-filter-grid">
                        {visibleFilters.map((id) => {
                          const label = catalog.data?.dimensions.find((d) => d.id === id)?.label ?? id;
                          return (
                            <div className="stack" key={id}>
                              <Multi
                                label={`Filter ${label}`}
                                size={4}
                                values={draft.filters[id] ?? []}
                                options={o.dimensions[id] ?? []}
                                onChange={(v) => {
                                  const filters = { ...draft.filters };
                                  if (v.length) filters[id] = v;
                                  else delete filters[id];
                                  change({ filters });
                                }}
                              />
                              <button
                                className="btn btn-ghost"
                                onClick={() => {
                                  const filters = { ...draft.filters };
                                  delete filters[id];
                                  change({ filters });
                                  setAddedFilters((ids) => ids.filter((value) => value !== id));
                                }}
                              >
                                Remove filter {label}
                              </button>
                            </div>
                          );
                        })}
                      </div>
                    </>
                  )}
                </AsyncSection>
                {!localOnly && (
                  <Field
                    label="Hypothetical discount (%)"
                    hint="Savings sensitivity only — not a billing price or an actual discount."
                  >
                    <input
                      className="input"
                      type="number"
                      min="0"
                      max="100"
                      value={draft.scenario_discount_percent}
                      onChange={(e) => change({ scenario_discount_percent: Number(e.target.value) })}
                    />
                  </Field>
                )}
                <label className="row">
                  <input type="checkbox" checked={shared} onChange={(e) => setShared(e.target.checked)} />
                  Share definition (not report data)
                </label>
              </div>
            </details>
          </section>
        ))}
    </section>
  );
}
