import { sortCollection } from '../../lib/collections';
import { Collection } from '../../components/Collection';
import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { BlockingRule, RuleClause } from '../../lib/types';
import { formatNumber, formatRelative, titleCase } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Drawer, Field, useToast } from '../../components/ui';
import { PolicyCaveat, SortHeader } from '../shared';
import { useUrlState } from '../../lib/hooks';
import { t, type MessageKey } from '../../lib/i18n';

interface RulesResponse {
  rules: BlockingRule[];
  clause_types: string[];
  caveat: string;
}

const CLAUSE_HELP_KEYS: Record<string, MessageKey> = {
  user_agent_pattern: 'adminRules.clauseHelp.userAgentPattern',
  source_ip_cidr: 'adminRules.clauseHelp.sourceIpCidr',
  x_forwarded_for_cidr: 'adminRules.clauseHelp.xForwardedForCidr',
  endpoint_regex: 'adminRules.clauseHelp.endpointRegex',
  http_method: 'adminRules.clauseHelp.httpMethod',
  header_match: 'adminRules.clauseHelp.headerMatch',
  downstream_token_pattern: 'adminRules.clauseHelp.downstreamTokenPattern',
  model_name: 'adminRules.clauseHelp.modelName',
};

function clauseHelp(type: string): string | undefined {
  const key = CLAUSE_HELP_KEYS[type];
  return key ? t(key) : undefined;
}

/**
 * Rules arrive as one whole list, so sorting is client-side. Ordering by hits
 * answers "which rule is actually firing"; by last hit, "which rule is dead
 * weight" — neither was reachable before.
 */
const RULE_SORTS = {
  name: (r: BlockingRule) => r.name.toLowerCase(),
  hits: (r: BlockingRule) => r.hit_count ?? 0,
  last_hit: (r: BlockingRule) => r.last_hit_at ?? '',
};

function sortRules(rules: BlockingRule[], sort: string): BlockingRule[] {
  const ascending = sort.endsWith('_asc');
  const key = sort.replace(/_asc$/, '') as keyof typeof RULE_SORTS;
  const accessor = RULE_SORTS[key] ?? RULE_SORTS.name;
  return sortCollection(rules, accessor, ascending);
}

export function RulesPage(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  // Sort lives in the URL so a sorted view is a shareable link.
  const [sort, setSort] = useUrlState('sort', 'name_asc');
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<BlockingRule | null>(null);
  const [deleting, setDeleting] = useState<BlockingRule | null>(null);
  const [testerOpen, setTesterOpen] = useState(false);

  const rules = useQuery({
    queryKey: ['admin', 'rules'],
    queryFn: () => api.get<RulesResponse>('/api/v1/admin/rules/blocking'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/rules/blocking/${id}`),
    onSuccess: () => {
      toast(t('adminRules.deletedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'rules'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminRules.title')}</h1>
          <p className="page-subtitle">{t('adminRules.subtitle')}</p>
        </div>
        <div className="row">
          <button type="button" className="btn" onClick={() => setTesterOpen(true)}>
            {t('adminRules.testRequest')}
          </button>
          <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
            {t('adminRules.newRule')}
          </button>
        </div>
      </header>

      <PolicyCaveat />

      <section className="card card-flush">
        <AsyncSection
          query={rules}
          empty={{
            when: (data) => data.rules.length === 0,
            title: t('adminRules.emptyTitle'),
            body: t('adminRules.emptyBody'),
            action: (
              <button type="button" className="btn btn-primary" onClick={() => setCreating(true)}>
                {t('adminRules.createFirst')}
              </button>
            ),
          }}
        >
          {(data) => (
            <Collection
              name="Blocking rules"
              rows={sortRules(data.rules, sort)}
              rowKey={(rule) => rule.id}
              resetKey={sort}
              columns={[
                { id: '0', label: '', value: (rule) => rule.name, render: () => null },
                { id: '1', label: '', value: (rule) => rule.reason, render: () => null },
                { id: '2', label: '', value: (rule) => rule.clauses.map((c) => c.pattern).join(' '), render: () => null },
              ]}
            >
              {(visible) => (
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <SortHeader label={t('adminRules.colRule')} sortKey="name" active={sort} onSort={setSort} />
                        <th scope="col">{t('adminRules.colConditions')}</th>
                        <th scope="col">{t('adminRules.colMatch')}</th>
                        <SortHeader label={t('adminRules.colHits')} sortKey="hits" active={sort} onSort={setSort} />
                        <SortHeader label={t('adminRules.colLastHit')} sortKey="last_hit" active={sort} onSort={setSort} />
                        <th scope="col">
                          <span className="sr-only">{t('tables.actions')}</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {visible.map((rule) => (
                        <tr key={rule.id}>
                          <td>
                            <div>{rule.name}</div>
                            {rule.reason ? <div className="small muted">{rule.reason}</div> : null}
                          </td>
                          <td>
                            <ul style={{ margin: 0, paddingLeft: 16 }}>
                              {rule.clauses.map((clause, index) => (
                                <li key={index} className="small">
                                  {clause.negate ? t('adminRules.not') : ''}
                                  {clause.type.replace(/_/g, ' ')}
                                  {clause.header ? ` [${clause.header}]` : ''} · <code>{clause.pattern}</code>
                                </li>
                              ))}
                            </ul>
                          </td>
                          <td>
                            <Badge tone="neutral">{rule.combinator.toUpperCase()}</Badge>
                          </td>
                          <td className="num small">{formatNumber(rule.hit_count)}</td>
                          <td className="small muted">
                            {rule.last_hit_at ? formatRelative(rule.last_hit_at) : t('adminRules.never')}
                          </td>
                          <td style={{ textAlign: 'right' }}>
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setEditing(rule)}>
                              {t('adminRules.edit')}
                            </button>
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setDeleting(rule)}>
                              {t('tables.delete')}
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </Collection>
          )}
        </AsyncSection>
      </section>

      <RuleDrawer
        key={editing ? editing.id : 'new'}
        open={creating || Boolean(editing)}
        rule={editing}
        clauseTypes={rules.data?.clause_types ?? []}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
        onSaved={() => {
          setCreating(false);
          setEditing(null);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'rules'] });
        }}
      />

      <TesterDrawer open={testerOpen} onClose={() => setTesterOpen(false)} />

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
        title={t('adminRules.deleteTitle', { name: deleting?.name ?? '' })}
        consequence={t('adminRules.deleteConsequence', { count: formatNumber(deleting?.hit_count ?? 0) })}
        confirmLabel={t('adminRules.deleteConfirm')}
        busy={remove.isPending}
      />
    </div>
  );
}

function RuleDrawer({
  open,
  rule,
  clauseTypes,
  onClose,
  onSaved,
}: {
  open: boolean;
  rule?: BlockingRule | null;
  clauseTypes: string[];
  onClose: () => void;
  onSaved: () => void;
}): ReactNode {
  const toast = useToast();
  const [name, setName] = useState(rule?.name ?? '');
  const [reason, setReason] = useState(rule?.reason ?? '');
  const [combinator, setCombinator] = useState<'and' | 'or'>(rule?.combinator === 'or' ? 'or' : 'and');
  const [clauses, setClauses] = useState<RuleClause[]>(
    rule && rule.clauses.length > 0
      ? rule.clauses.map((clause) => ({ ...clause }))
      : [{ type: 'user_agent_pattern', pattern: '' }],
  );

  const create = useMutation({
    mutationFn: () => {
      const payload = {
        name: name.trim(),
        reason: reason.trim(),
        combinator,
        enabled: rule ? rule.enabled : true,
        clauses,
      };
      return rule
        ? api.put(`/api/v1/admin/rules/blocking/${rule.id}`, payload)
        : api.post('/api/v1/admin/rules/blocking', payload);
    },
    onSuccess: () => {
      toast(rule ? t('adminRules.updatedToast') : t('adminRules.createdToast'));
      setName('');
      setReason('');
      setClauses([{ type: 'user_agent_pattern', pattern: '' }]);
      onSaved();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const valid = Boolean(name.trim()) && clauses.every((clause) => clause.pattern.trim());

  const update = (index: number, patch: Partial<RuleClause>) => {
    setClauses((current) => current.map((clause, i) => (i === index ? { ...clause, ...patch } : clause)));
  };

  if (!open) return null;

  return (
    <Drawer
      open
      onClose={onClose}
      title={rule ? t('adminRules.editRuleTitle', { name: rule.name }) : t('adminRules.newRuleTitle')}
    >
      <Field label={t('adminRules.nameLabel')} required hint={t('adminRules.nameHint')}>
        <input
          className="input"
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder={t('adminRules.namePlaceholder')}
          autoFocus
        />
      </Field>

      <Field label={t('adminRules.reasonLabel')} hint={t('adminRules.reasonHint')}>
        <input
          className="input"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder={t('adminRules.reasonPlaceholder')}
        />
      </Field>

      <Field label={t('adminRules.combineLabel')}>
        <select className="select" value={combinator} onChange={(event) => setCombinator(event.target.value as 'and' | 'or')}>
          <option value="and">{t('adminRules.combineAnd')}</option>
          <option value="or">{t('adminRules.combineOr')}</option>
        </select>
      </Field>

      <div className="stack">
        {clauses.map((clause, index) => (
          <div key={index} className="card" style={{ padding: 'var(--janus-space-3)' }}>
            <div className="row-between" style={{ marginBottom: 'var(--janus-space-2)' }}>
              <span className="overline">{t('adminRules.conditionN', { count: index + 1 })}</span>
              {clauses.length > 1 ? (
                <button
                  type="button"
                  className="btn btn-ghost btn-sm"
                  onClick={() => setClauses((current) => current.filter((_, i) => i !== index))}
                >
                  {t('adminRules.remove')}
                </button>
              ) : null}
            </div>
            <Field label={t('adminRules.signal')} hint={clauseHelp(clause.type)}>
              <select className="select" value={clause.type} onChange={(event) => update(index, { type: event.target.value })}>
                {clauseTypes.map((type) => (
                  <option key={type} value={type}>
                    {titleCase(type.replace(/_/g, ' '))}
                  </option>
                ))}
              </select>
            </Field>
            {clause.type === 'header_match' ? (
              <Field label={t('adminRules.headerName')} required>
                <input
                  className="input"
                  value={clause.header ?? ''}
                  onChange={(event) => update(index, { header: event.target.value })}
                  placeholder="X-Client-Tool"
                />
              </Field>
            ) : null}
            <Field label={t('adminRules.pattern')} required>
              <input
                className="input"
                value={clause.pattern}
                onChange={(event) => update(index, { pattern: event.target.value })}
                placeholder={clause.type.includes('cidr') ? '192.168.0.0/16' : 'openclaw'}
              />
            </Field>
            <label className="switch">
              <input
                type="checkbox"
                checked={Boolean(clause.negate)}
                onChange={(event) => update(index, { negate: event.target.checked })}
              />
              <span className="small">
                {t('adminRules.invertPrefix')} <em>{t('adminRules.invertEmphasis')}</em> {t('adminRules.invertSuffix')}
              </span>
            </label>
          </div>
        ))}
        <button
          type="button"
          className="btn"
          onClick={() => setClauses((current) => [...current, { type: 'user_agent_pattern', pattern: '' }])}
        >
          {t('adminRules.addCondition')}
        </button>
      </div>

      <div className="row" style={{ marginTop: 'var(--janus-space-4)' }}>
        <button type="button" className="btn btn-primary" onClick={() => create.mutate()} disabled={!valid || create.isPending}>
          {create.isPending ? t('tables.saving') : rule ? t('adminUpstreams.saveChanges') : t('adminRules.createRule')}
        </button>
        <button type="button" className="btn" onClick={onClose}>
          {t('common.cancel')}
        </button>
      </div>
    </Drawer>
  );
}

function TesterDrawer({ open, onClose }: { open: boolean; onClose: () => void }): ReactNode {
  const [userAgent, setUserAgent] = useState('openclaw/0.9');
  const [sourceIP, setSourceIP] = useState('10.1.2.3');
  const [path, setPath] = useState('/v1/chat/completions');
  const [method, setMethod] = useState('POST');
  const [model, setModel] = useState('gpt-4o-mini');
  const [tokenPrefix, setTokenPrefix] = useState('janus_');

  const test = useMutation({
    mutationFn: () =>
      api.post<{ matched: boolean; rule_name?: string; clause?: string; reason?: string; detail?: string }>(
        '/api/v1/admin/rules/blocking/test',
        {
          user_agent: userAgent,
          source_ip: sourceIP,
          path,
          method,
          model,
          token_prefix: tokenPrefix,
        },
      ),
  });

  if (!open) return null;

  return (
    <Drawer open onClose={onClose} title={t('adminRules.testerTitle')}>
      <p className="small secondary">{t('adminRules.testerIntro')}</p>

      <Field label={t('adminRules.userAgent')}>
        <input className="input" value={userAgent} onChange={(event) => setUserAgent(event.target.value)} />
      </Field>
      <Field label={t('adminRules.sourceIp')}>
        <input className="input" value={sourceIP} onChange={(event) => setSourceIP(event.target.value)} />
      </Field>
      <Field label={t('adminRules.path')}>
        <input className="input" value={path} onChange={(event) => setPath(event.target.value)} />
      </Field>
      <Field label={t('adminRules.method')}>
        <select className="select" value={method} onChange={(event) => setMethod(event.target.value)}>
          {['POST', 'GET', 'PUT', 'PATCH', 'DELETE'].map((verb) => (
            <option key={verb} value={verb}>
              {verb}
            </option>
          ))}
        </select>
      </Field>
      <Field label={t('tables.model')}>
        <input className="input" value={model} onChange={(event) => setModel(event.target.value)} />
      </Field>
      <Field label={t('adminRules.tokenPrefix')}>
        <input className="input" value={tokenPrefix} onChange={(event) => setTokenPrefix(event.target.value)} />
      </Field>

      <button type="button" className="btn btn-primary" onClick={() => test.mutate()} disabled={test.isPending}>
        {test.isPending ? t('adminRules.evaluating') : t('adminRules.evaluate')}
      </button>

      {test.data ? (
        <div className={`banner banner-${test.data.matched ? 'danger' : 'info'}`} role="status">
          <div>
            {test.data.matched ? (
              <>
                <strong>{t('adminRules.blockedBy', { name: test.data.rule_name ?? '' })}</strong>
                <div className="small" style={{ marginTop: 4 }}>
                  {t('adminRules.matchingCondition', { name: test.data.clause ?? '' })}
                  {test.data.reason ? t('adminRules.reasonReturned', { name: test.data.reason }) : ''}
                </div>
              </>
            ) : (
              <strong>{t('adminRules.noRuleBlocks')}</strong>
            )}
          </div>
        </div>
      ) : null}
    </Drawer>
  );
}
