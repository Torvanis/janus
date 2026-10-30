/**
 * Safety & Security → Child Safety. One place for everything about
 * children on this gateway (free on every edition):
 *
 * - who: children and groups, each in an age range;
 * - age ranges: per category Off / Answer appropriately / Block, with the
 *   instructions the model is given (editable, reset to built-in), the text
 *   every turn gets, and models the range may not use;
 * - the safety model that reads each message, and recent events.
 *
 * The rules follow the person to every model they can use, personal plans
 * included; nothing here depends on which model a child picks.
 */
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { LookupInput } from '../../components/LookupInput';
import { blockedModelLookup, teamLookup, userLookup } from '../../lib/lookups';
import type {
  ChildCategorySetting,
  ChildMedia,
  ChildOutcome,
  ChildProfile,
  FamilyAssignment,
  FamilyClassifier,
  FamilyDoc,
} from '../../lib/types';
import { AsyncSection, Badge, Field, useToast } from '../../components/ui';
import { IconButton } from '../../components/IconButton';
import { formatDateTime } from '../../lib/format';
import { t } from '../../lib/i18n';
import { familyVerdict } from '../../lib/familyHealth';

export const CHILD_SAFETY_KEY = ['admin', 'child-safety'] as const;

const NO_MEDIA: ChildMedia = { image: false, video: false, music: false, transcription: false };

/** The four media switches per age range, in plain words. */
const MEDIA_SWITCHES: { key: keyof ChildMedia; label: string; hint: string }[] = [
  { key: 'image', label: 'Make pictures', hint: 'Chat can draw pictures from what they ask. Their request is checked first.' },
  { key: 'video', label: 'Make videos', hint: 'Chat can make short videos from what they ask. Their request is checked first.' },
  { key: 'music', label: 'Make music', hint: 'Chat can make music from what they ask. Their request is checked first.' },
  {
    key: 'transcription',
    label: 'Turn speech into text',
    hint: 'They can attach sound and video files. Janus turns them into text and checks that text before the model reads it. With this off, sound and video files are refused.',
  },
];
const URL = '/api/v1/admin/child-safety';

export function useChildSafety() {
  return useQuery({ queryKey: CHILD_SAFETY_KEY, queryFn: () => api.get<FamilyDoc>(URL) });
}

const OUTCOMES: Array<{ value: ChildOutcome; label: string }> = [
  { value: 'off', label: 'Off' },
  { value: 'answer', label: 'Answer appropriately' },
  { value: 'block', label: 'Block' },
];

/** What each outcome means for a category, in one line. */
function outcomeHelp(slug: string, outcome: ChildOutcome): string {
  if (outcome === 'off') return 'Not checked: the model answers as it normally would.';
  if (slug === 'feeling_low') {
    return outcome === 'block'
      ? 'Janus answers with a kind message pointing to a trusted adult and the 988 Lifeline, does not pass the message on, and notifies you.'
      : 'The model answers, told to be a warm listener and to point to a trusted adult.';
  }
  if (outcome === 'block') return 'A short, kind refusal. The message is not sent to the model, and you are notified.';
  if (slug === 'homework_shortcut' || slug === 'homework_learning') {
    return 'The model tutors: hints and steps, never the final answer. A reply that gives the answer away is stopped there.';
  }
  return 'The model answers, with the instructions below.';
}

const EVENT_LABEL: Record<string, string> = {
  supported: 'Answered kindly',
  blocked: 'Message blocked',
  stream_cut: 'Reply stopped',
  withheld: 'Answer held back',
};

function classifierLabel(c: FamilyClassifier): string {
  const tags: string[] = [c.hosted ? 'hosted' : 'local'];
  if (c.classifier_role === 'generative_guard') tags.push('legacy');
  if (c.recommended === 'hosted_fastest') tags.push('recommended');
  return `${c.name} · ${c.upstream_name} (${tags.join(', ')})`;
}

export function ChildSafetyPage(): ReactNode {
  const doc = useChildSafety();
  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">Child Safety</h1>
          <p className="page-subtitle">
            Rules for the children on this gateway. They follow each child to every model they can use — current and future ones,
            personal plans included — in Janus Chat and through the API.
          </p>
        </div>
      </header>
      <AsyncSection query={doc}>{(d) => <ChildSafetyEditor doc={d} />}</AsyncSection>
    </div>
  );
}

interface Draft {
  enabled: boolean;
  classifier_model_id: string;
  ask: 'together' | 'separately';
  profiles: ChildProfile[];
  assignments: FamilyAssignment[];
}

function draftFrom(doc: FamilyDoc): Draft {
  return {
    enabled: doc.family.enabled,
    classifier_model_id: doc.family.classifier_model_id,
    ask: doc.family.ask,
    profiles: doc.family.profiles.map((p) => ({
      ...p,
      base: p.base || undefined,
      categories: Object.fromEntries(
        Object.entries(p.categories).map(([k, v]) => [k, { outcome: v.outcome, instructions: v.instructions || undefined }]),
      ),
      blocked_models: [...p.blocked_models],
      media: { ...NO_MEDIA, ...p.media },
    })),
    assignments: doc.family.assignments.map((a) => ({ ...a })),
  };
}

function ChildSafetyEditor({ doc }: { doc: FamilyDoc }): ReactNode {
  const qc = useQueryClient();
  const toast = useToast();
  const [draft, setDraft] = useState<Draft>(() => draftFrom(doc));
  const [selected, setSelected] = useState<string>(doc.family.profiles[1]?.id ?? doc.family.profiles[0]?.id ?? '');
  useEffect(() => setDraft(draftFrom(doc)), [doc]);
  const dirty = useMemo(() => JSON.stringify(draft) !== JSON.stringify(draftFrom(doc)), [draft, doc]);

  const save = useMutation({
    mutationFn: () => api.put<FamilyDoc>(URL, draft),
    onSuccess: (next) => {
      qc.setQueryData(CHILD_SAFETY_KEY, next);
      toast('Child Safety saved.');
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const verdict = familyVerdict(doc.status.state, doc.status.reasons);
  const profile = draft.profiles.find((p) => p.id === selected) ?? draft.profiles[0];
  const setProfile = (next: ChildProfile) =>
    setDraft((d) => ({ ...d, profiles: d.profiles.map((p) => (p.id === next.id ? next : p)) }));
  const addRange = () => {
    const from = profile ?? draft.profiles[0];
    const id = `cs_${Math.random().toString(36).slice(2, 10)}`;
    const copy: ChildProfile = {
      id,
      name: `${from?.name ?? 'Kids'} (copy)`,
      builtin: false,
      base: from?.base || doc.builtin[from?.id ?? '']?.base || '',
      categories: Object.fromEntries(
        doc.categories.map((c) => [
          c.slug,
          {
            outcome: from?.categories[c.slug]?.outcome ?? doc.builtin[from?.id ?? '']?.outcomes[c.slug] ?? 'answer',
            instructions: from?.categories[c.slug]?.instructions || doc.builtin[from?.id ?? '']?.categories[c.slug] || '',
          },
        ]),
      ),
      blocked_models: [...(from?.blocked_models ?? [])],
      media: { ...NO_MEDIA, ...from?.media },
    };
    setDraft((d) => ({ ...d, profiles: [...d.profiles, copy] }));
    setSelected(id);
  };

  return (
    <div className="stack" style={{ gap: 'var(--janus-space-5)' }}>
      <section className="card" aria-labelledby="cs-status">
        <div className="card-header" style={{ alignItems: 'flex-start' }}>
          <div>
            <h2 id="cs-status" style={{ margin: 0 }}>
              Status <Badge tone={verdict.tone}>{t(verdict.title)}</Badge>
            </h2>
            <p
              className="small secondary"
              style={{ margin: '4px 0 0' }}
              data-testid="family-verdict"
              data-state={doc.status.state}
            >
              {verdict.reasons.length > 0
                ? verdict.reasons.map((r) => t(r)).join(' ')
                : verdict.tone === 'success'
                  ? `Checking messages and replies for ${doc.status.covered_users} ${doc.status.covered_users === 1 ? 'child' : 'children'}.`
                  : 'Turn it on, choose a safety model and add the children.'}
            </p>
            <p className="small muted" style={{ margin: '4px 0 0' }}>
              Included in every edition.
            </p>
          </div>
          <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
            <label className="switch">
              <input
                type="checkbox"
                checked={draft.enabled}
                aria-label="Child Safety on"
                onChange={(e) => setDraft((d) => ({ ...d, enabled: e.target.checked }))}
              />
              <span>{draft.enabled ? 'On' : 'Off'}</span>
            </label>
            <button type="button" className="btn btn-primary" disabled={!dirty || save.isPending} onClick={() => save.mutate()}>
              {save.isPending ? 'Saving…' : 'Save'}
            </button>
          </div>
        </div>
      </section>

      <ChildrenCard doc={doc} draft={draft} setDraft={setDraft} />

      <section className="card" aria-labelledby="cs-ranges">
        <div className="card-header">
          <div>
            <h2 id="cs-ranges" style={{ margin: 0 }}>
              Age ranges
            </h2>
            <p className="small secondary" style={{ margin: '4px 0 0' }}>
              What happens when a child's message is about one of these, for each age range. Each child gets the rules of their
              range; a child in several groups gets the strictest setting of each.
            </p>
          </div>
          <button type="button" className="btn" onClick={addRange}>
            Copy to a new range
          </button>
        </div>
        <div className="segmented" role="tablist" aria-label="Age ranges" style={{ marginBottom: 'var(--janus-space-4)' }}>
          {draft.profiles.map((p) => (
            <button
              key={p.id}
              type="button"
              role="tab"
              aria-selected={p.id === profile?.id}
              aria-pressed={p.id === profile?.id}
              onClick={() => setSelected(p.id)}
            >
              {p.name}
            </button>
          ))}
        </div>
        {profile ? (
          <RangeEditor
            key={profile.id}
            doc={doc}
            profile={profile}
            onChange={setProfile}
            onDelete={
              profile.builtin
                ? undefined
                : () => {
                    setDraft((d) => ({
                      ...d,
                      profiles: d.profiles.filter((p) => p.id !== profile.id),
                      assignments: d.assignments.filter((a) => a.profile_id !== profile.id),
                    }));
                    setSelected(draft.profiles[1]?.id ?? '');
                  }
            }
          />
        ) : null}
      </section>

      <section className="card" aria-labelledby="cs-always">
        <h2 id="cs-always" style={{ marginTop: 0 }}>
          Always on
        </h2>
        <p className="small secondary" style={{ margin: '0 0 6px' }}>
          Blocked for every child, in every age range, in what they send and what the model answers. Self-harm always gets the
          kind answer and a notification.
        </p>
        <ul className="small" style={{ margin: 0 }} aria-label="Always blocked">
          {doc.safety_categories.map((c) => (
            <li key={c.slug}>{c.name}</li>
          ))}
        </ul>
        <div className="field-label" style={{ marginTop: 'var(--janus-space-3)' }}>
          Tutor rule
        </div>
        <p className="small secondary" style={{ margin: '0 0 4px' }}>
          Added to every message while either homework category is set to Answer appropriately. Replies are checked too: one that
          gives a final answer away stops there and ends with:
        </p>
        <blockquote className="small" style={{ margin: 0 }}>
          “{doc.withheld_note}”
        </blockquote>
        <details className="small" style={{ marginTop: 6 }}>
          <summary>Show the tutor rule</summary>
          <p style={{ margin: '4px 0 0' }}>{doc.tutor_rule}</p>
        </details>
      </section>

      <section className="card" aria-labelledby="cs-model">
        <h2 id="cs-model" style={{ marginTop: 0 }}>
          Safety model
        </h2>
        <Field
          label="Safety model"
          hint="Reads each message (and reply) and says which categories it is about. Any decision model (Jev, Clef, Clef-Flash, Nimble); set a model's classifier role under Security → Classifiers first."
        >
          <select
            className="select"
            aria-label="Safety model"
            value={draft.classifier_model_id}
            onChange={(e) => setDraft((d) => ({ ...d, classifier_model_id: e.target.value }))}
          >
            <option value="">Choose a safety model…</option>
            {doc.classifiers.map((c) => (
              <option key={c.id} value={c.id}>
                {classifierLabel(c)}
              </option>
            ))}
          </select>
        </Field>
        <ul className="small secondary" style={{ margin: '6px 0 0' }}>
          <li>Recommended: hosted Jev — the fastest and most accurate in our tests.</li>
          <li>Hosted means the children's messages and the replies are sent to that provider to be checked.</li>
          <li>Fully local: Clef. Lower delay on local hardware: Nimble or Clef-Flash (ask one question per category).</li>
        </ul>
        <label className="switch" style={{ marginTop: 'var(--janus-space-2)' }}>
          <input
            type="checkbox"
            checked={draft.ask === 'separately'}
            aria-label="Ask one question per category"
            onChange={(e) => setDraft((d) => ({ ...d, ask: e.target.checked ? 'separately' : 'together' }))}
          />
          <span>Ask one question per category</span>
        </label>
      </section>

      <section className="card" aria-labelledby="cs-recent">
        <div className="card-header">
          <h2 id="cs-recent" style={{ margin: 0 }}>
            Recent events
          </h2>
          <Link className="small" to="/admin/security/violations">
            All violations
          </Link>
        </div>
        {doc.recent.length === 0 ? (
          <p className="muted small" style={{ margin: 0 }}>
            Nothing yet.
          </p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th scope="col">When</th>
                <th scope="col">Who</th>
                <th scope="col">What happened</th>
                <th scope="col">Categories</th>
              </tr>
            </thead>
            <tbody>
              {doc.recent.map((v) => (
                <tr key={v.id}>
                  <td>{formatDateTime(v.created_at)}</td>
                  <td>{v.user_label || v.user_id}</td>
                  <td>
                    <Badge tone={v.action === 'supported' ? 'info' : 'warning'}>
                      {(v.rule_id ?? '') === 'final_answer' ? 'Answer held back' : (EVENT_LABEL[v.action] ?? v.action)}
                    </Badge>
                  </td>
                  <td className="small">{(v.rule_names ?? []).join(', ')}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
      {dirty ? (
        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn" onClick={() => setDraft(draftFrom(doc))}>
            Discard changes
          </button>
          <button type="button" className="btn btn-primary" disabled={save.isPending} onClick={() => save.mutate()}>
            {save.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      ) : null}
    </div>
  );
}

function ChildrenCard({
  doc,
  draft,
  setDraft,
}: {
  doc: FamilyDoc;
  draft: Draft;
  setDraft: (fn: (d: Draft) => Draft) => void;
}): ReactNode {
  const [kind, setKind] = useState<'user' | 'team'>('user');
  const [picked, setPicked] = useState<string[]>([]);
  const [range, setRange] = useState(draft.profiles[1]?.id ?? draft.profiles[0]?.id ?? '');
  const [labels, setLabels] = useState<Record<string, string>>(() =>
    Object.fromEntries(doc.family.assignments.map((a) => [a.scope_id, a.name ?? a.scope_id])),
  );
  // Stable: LookupInput re-fires onLabels whenever its identity changes.
  const mergeLabels = useCallback((l: Record<string, string>) => {
    setLabels((prev) => (Object.entries(l).every(([k, v]) => prev[k] === v) ? prev : { ...prev, ...l }));
  }, []);
  const add = () => {
    setDraft((d) => {
      const keep = d.assignments.filter((a) => !(a.scope_type === kind && picked.includes(a.scope_id)));
      return { ...d, assignments: [...keep, ...picked.map((id) => ({ scope_type: kind, scope_id: id, profile_id: range }))] };
    });
    setPicked([]);
  };
  const lookup = kind === 'user' ? userLookup : teamLookup;
  const rows = [...draft.assignments].sort(
    (a, b) => a.scope_type.localeCompare(b.scope_type) || (labels[a.scope_id] ?? '').localeCompare(labels[b.scope_id] ?? ''),
  );
  return (
    <section className="card" aria-labelledby="cs-children">
      <h2 id="cs-children" style={{ marginTop: 0 }}>
        Children
      </h2>
      <p className="small secondary" style={{ margin: '0 0 var(--janus-space-3)' }}>
        Add a child directly, or a group (team) of children. A child's own age range wins over their groups'. It goes by who they
        are, not which workspace they have picked.
      </p>
      {rows.length === 0 ? (
        <p className="muted small">No one yet.</p>
      ) : (
        <table className="table" aria-label="Children and groups">
          <thead>
            <tr>
              <th scope="col">Child or group</th>
              <th scope="col">Age range</th>
              <th scope="col" style={{ width: 48 }}>
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((a) => {
              const name = labels[a.scope_id] ?? a.name ?? a.scope_id;
              return (
                <tr key={`${a.scope_type}:${a.scope_id}`}>
                  <th scope="row" style={{ fontWeight: 500 }}>
                    {name} {a.scope_type === 'team' ? <Badge tone="neutral">group</Badge> : null}
                  </th>
                  <td>
                    <select
                      className="select page-size"
                      aria-label={`Age range for ${name}`}
                      value={a.profile_id}
                      onChange={(e) =>
                        setDraft((d) => ({
                          ...d,
                          assignments: d.assignments.map((x) =>
                            x.scope_type === a.scope_type && x.scope_id === a.scope_id ? { ...x, profile_id: e.target.value } : x,
                          ),
                        }))
                      }
                    >
                      {draft.profiles.map((p) => (
                        <option key={p.id} value={p.id}>
                          {p.name}
                        </option>
                      ))}
                    </select>
                  </td>
                  <td>
                    <IconButton
                      icon="delete"
                      danger
                      label={`Remove ${name}`}
                      onClick={() =>
                        setDraft((d) => ({
                          ...d,
                          assignments: d.assignments.filter((x) => !(x.scope_type === a.scope_type && x.scope_id === a.scope_id)),
                        }))
                      }
                    />
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      <div className="cs-add" role="group" aria-label="Add children">
        <div className="segmented" role="radiogroup" aria-label="Add">
          {(['user', 'team'] as const).map((k) => (
            <button
              key={k}
              type="button"
              role="radio"
              aria-checked={kind === k}
              aria-pressed={kind === k}
              onClick={() => {
                setKind(k);
                setPicked([]);
              }}
            >
              {k === 'user' ? 'Child' : 'Group'}
            </button>
          ))}
        </div>
        <div style={{ flex: 1, minWidth: 220 }}>
          <LookupInput
            key={kind}
            label={kind === 'user' ? 'Children to add' : 'Groups to add'}
            value={picked}
            onChange={setPicked}
            search={lookup.search}
            resolve={lookup.resolve}
            onLabels={mergeLabels}
            placeholder={kind === 'user' ? 'Type a name or email' : 'Type a team name'}
          />
        </div>
        <select
          className="select page-size"
          aria-label="Age range to add them to"
          value={range}
          onChange={(e) => setRange(e.target.value)}
        >
          {draft.profiles.map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
        <button type="button" className="btn" disabled={picked.length === 0} onClick={add}>
          Add
        </button>
      </div>
    </section>
  );
}

function RangeEditor({
  doc,
  profile,
  onChange,
  onDelete,
}: {
  doc: FamilyDoc;
  profile: ChildProfile;
  onChange: (p: ChildProfile) => void;
  onDelete?: () => void;
}): ReactNode {
  const builtin = doc.builtin[profile.id] ?? doc.builtin.cs_kids;
  const setCat = (slug: string, patch: Partial<ChildCategorySetting>) => {
    const cur = profile.categories[slug] ?? { outcome: builtin?.outcomes[slug] ?? 'answer' };
    onChange({ ...profile, categories: { ...profile.categories, [slug]: { ...cur, ...patch } } });
  };
  const base = profile.base ?? builtin?.base ?? '';
  return (
    <div className="stack" style={{ gap: 'var(--janus-space-4)' }}>
      {!profile.builtin ? (
        <div className="row" style={{ gap: 'var(--janus-space-2)', alignItems: 'flex-end' }}>
          <div style={{ flex: 1 }}>
            <Field label="Name">
              <input
                className="input"
                aria-label="Name"
                value={profile.name}
                onChange={(e) => onChange({ ...profile, name: e.target.value })}
              />
            </Field>
          </div>
          {onDelete ? (
            <button type="button" className="btn btn-danger" onClick={onDelete}>
              Delete range
            </button>
          ) : null}
        </div>
      ) : null}
      <table className="table cs-cats" aria-label={`Categories for ${profile.name}`}>
        <thead>
          <tr>
            <th scope="col">Category</th>
            <th scope="col">What happens</th>
          </tr>
        </thead>
        <tbody>
          {doc.categories.map((c) => {
            const set = profile.categories[c.slug];
            const outcome = set?.outcome ?? builtin?.outcomes[c.slug] ?? 'answer';
            // undefined = the built-in text; anything typed (even empty
            // while editing) is the parent's own.
            const text = set?.instructions ?? builtin?.categories[c.slug] ?? '';
            const custom = set?.instructions !== undefined && set.instructions !== builtin?.categories[c.slug];
            return (
              <tr key={c.slug}>
                <th scope="row" style={{ fontWeight: 500, verticalAlign: 'top' }}>
                  {c.name}
                  <div className="small muted" style={{ fontWeight: 400 }}>
                    {c.includes}
                  </div>
                </th>
                <td style={{ verticalAlign: 'top' }}>
                  <div className="segmented" role="radiogroup" aria-label={`${c.name}: what happens`}>
                    {OUTCOMES.map((o) => (
                      <button
                        key={o.value}
                        type="button"
                        role="radio"
                        aria-checked={outcome === o.value}
                        aria-pressed={outcome === o.value}
                        onClick={() => setCat(c.slug, { outcome: o.value })}
                      >
                        {o.label}
                      </button>
                    ))}
                  </div>
                  <p className="small secondary" style={{ margin: '4px 0 0' }}>
                    {outcomeHelp(c.slug, outcome)}
                  </p>
                  {c.slug === 'feeling_low' && outcome === 'off' ? (
                    <p className="small" role="note" style={{ margin: '4px 0 0', color: 'var(--janus-color-warning-fg)' }}>
                      With this off, a child who says they are struggling gets an ordinary answer, and you are not told.
                    </p>
                  ) : null}
                  {outcome === 'answer' ? (
                    <details style={{ marginTop: 4 }}>
                      <summary className="small">
                        Instructions to the model {custom ? <Badge tone="info">edited</Badge> : null}
                      </summary>
                      <textarea
                        className="textarea"
                        rows={4}
                        aria-label={`Instructions for ${c.name}`}
                        value={text}
                        onChange={(e) => setCat(c.slug, { instructions: e.target.value })}
                      />
                      {custom ? (
                        <button type="button" className="btn btn-sm" onClick={() => setCat(c.slug, { instructions: undefined })}>
                          Reset to built-in
                        </button>
                      ) : null}
                    </details>
                  ) : null}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      <Field label="Every message" hint="Added to every message from a child in this range, whatever it is about.">
        <textarea
          className="textarea"
          rows={3}
          aria-label="Every message"
          value={base}
          onChange={(e) => onChange({ ...profile, base: e.target.value })}
        />
      </Field>
      {profile.base !== undefined && profile.base !== '' && profile.base !== builtin?.base ? (
        <div>
          <button type="button" className="btn btn-sm" onClick={() => onChange({ ...profile, base: undefined })}>
            Reset to built-in
          </button>
        </div>
      ) : null}
      <fieldset className="stack" style={{ gap: 'var(--janus-space-2)', border: 0, padding: 0, margin: 0 }}>
        <legend style={{ fontWeight: 500 }}>What they can make</legend>
        <p className="small muted" style={{ margin: 0 }}>
          Off by default. When a switch is off, Chat does not offer it to children in this range. Pictures they attach themselves
          are passed to the model and are not checked.
        </p>
        {MEDIA_SWITCHES.map((m) => {
          const media = { ...NO_MEDIA, ...profile.media };
          return (
            <label className="switch" key={m.key} style={{ alignItems: 'flex-start' }}>
              <input
                type="checkbox"
                aria-label={m.label}
                checked={media[m.key]}
                onChange={(e) => onChange({ ...profile, media: { ...media, [m.key]: e.target.checked } })}
              />
              <span>
                {m.label}
                <span className="small muted" style={{ display: 'block' }}>
                  {m.hint}
                </span>
              </span>
            </label>
          );
        })}
      </fieldset>
      <div>
        <LookupInput
          label="Block these models (optional)"
          value={profile.blocked_models}
          onChange={(v) => onChange({ ...profile, blocked_models: v })}
          search={blockedModelLookup.search}
          resolve={blockedModelLookup.resolve}
          allowFreeText
          placeholder="Type a model name, e.g. my/xai/grok-4.7"
        />
        <p className="small muted" style={{ margin: '4px 0 0' }}>
          Not needed for safety: these rules already apply to every model. Use it to keep a range off particular models.
        </p>
      </div>
    </div>
  );
}
