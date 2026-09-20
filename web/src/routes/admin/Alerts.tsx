import { sortCollection } from '../../lib/collections';
import { Collection } from '../../components/Collection';
import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { AlertRule } from '../../lib/types';
import { titleCase } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, Field, Modal, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';
import { useLicensed } from '../../app/session';
import { useUrlState } from '../../lib/hooks';
import { SortHeader } from '../shared';

interface AlertsResponse {
  alerts: AlertRule[];
  email_enabled: boolean;
  email_detail: string;
  triggers: Array<{ value: string; label: string }>;
}

/**
 * Alerts arrive as one whole list, so sorting is client-side. Severity sorts
 * by blast radius (critical first) rather than alphabetically, which is the
 * only ordering an operator actually wants from that column.
 */
const SEVERITY_RANK: Record<string, number> = { critical: 3, warning: 2, info: 1 };

function sortAlerts(alerts: AlertRule[], sort: string): AlertRule[] {
  const ascending = sort.endsWith('_asc');
  const key = sort.replace(/_asc$/, '');
  return sortCollection(alerts, (rule) => (key === 'severity' ? SEVERITY_RANK[rule.severity] : rule.trigger), ascending);
}

export function AlertsPage(): ReactNode {
  // Sort lives in the URL so a sorted view survives a refresh.
  const [sort, setSort] = useUrlState('sort', 'severity');
  const queryClient = useQueryClient();
  const toast = useToast();
  const [createOpen, setCreateOpen] = useState(false);
  const [editing, setEditing] = useState<AlertRule | null>(null);
  const [deleting, setDeleting] = useState<AlertRule | null>(null);

  const alerts = useQuery({
    queryKey: ['admin', 'alerts'],
    queryFn: () => api.get<AlertsResponse>('/api/v1/admin/alerts'),
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/api/v1/admin/alerts/${id}`),
    onSuccess: () => {
      toast(t('adminAlerts.deletedToast'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'alerts'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const sendTest = useMutation({
    mutationFn: (id: string) => api.post(`/api/v1/admin/alerts/${id}/test`),
    onSuccess: () => toast(t('adminAlerts.testToast')),
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  return (
    <div className="page">
      <header className="page-header">
        <div>
          <h1 className="page-title">{t('adminAlerts.title')}</h1>
          <p className="page-subtitle">{t('adminAlerts.subtitle')}</p>
        </div>
        <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
          {t('adminAlerts.newRule')}
        </button>
      </header>

      {alerts.data && !alerts.data.email_enabled ? (
        <div className="banner banner-warning">
          <div>
            <strong>{t('adminAlerts.emailOffTitle')}</strong>
            <div className="small" style={{ marginTop: 4 }}>
              {alerts.data.email_detail} {t('adminAlerts.emailOffBody')}
            </div>
          </div>
        </div>
      ) : null}

      <section className="card card-flush">
        <AsyncSection
          query={alerts}
          empty={{
            when: (data) => data.alerts.length === 0,
            title: t('adminAlerts.emptyTitle'),
            body: t('adminAlerts.emptyBody'),
            action: (
              <button type="button" className="btn btn-primary" onClick={() => setCreateOpen(true)}>
                {t('adminAlerts.createFirst')}
              </button>
            ),
          }}
        >
          {(data) => (
            <Collection
              name="Alert rules"
              rows={sortAlerts(data.alerts, sort)}
              rowKey={(rule) => rule.id}
              resetKey={sort}
              columns={[
                { id: '0', label: '', value: (rule) => rule.trigger, render: () => null },
                { id: '1', label: '', value: (rule) => rule.severity, render: () => null },
                { id: '2', label: '', value: (rule) => rule.channels.join(' '), render: () => null },
              ]}
            >
              {(visible) => (
                <div className="table-wrap">
                  <table className="data">
                    <thead>
                      <tr>
                        <SortHeader label={t('adminAlerts.colTrigger')} sortKey="trigger" active={sort} onSort={setSort} />
                        <SortHeader label={t('adminAlerts.colSeverity')} sortKey="severity" active={sort} onSort={setSort} />
                        <th scope="col">{t('adminAlerts.colChannels')}</th>
                        <th scope="col">{t('adminAlerts.colWebhook')}</th>
                        <th scope="col">
                          <span className="sr-only">{t('tables.actions')}</span>
                        </th>
                      </tr>
                    </thead>
                    <tbody>
                      {visible.map((rule) => (
                        <tr key={rule.id}>
                          <td>{data.triggers.find((trigger) => trigger.value === rule.trigger)?.label ?? rule.trigger}</td>
                          <td>
                            <Badge tone={rule.severity === 'critical' ? 'danger' : 'warning'}>{titleCase(rule.severity)}</Badge>
                          </td>
                          <td className="small">
                            {rule.channels.map((channel) => (
                              <Badge key={channel} tone={channel === 'email' && !data.email_enabled ? 'neutral' : 'primary'}>
                                {channel === 'in_app' ? t('adminAlerts.inApp') : titleCase(channel)}
                              </Badge>
                            ))}
                          </td>
                          <td className="small muted truncate" style={{ maxWidth: 240 }}>
                            {rule.webhook_url || '—'}
                          </td>
                          <td style={{ textAlign: 'right', whiteSpace: 'nowrap' }}>
                            <button
                              type="button"
                              className="btn btn-ghost btn-sm"
                              onClick={() => sendTest.mutate(rule.id)}
                              disabled={sendTest.isPending}
                            >
                              {t('adminAlerts.sendTest')}
                            </button>
                            <button type="button" className="btn btn-ghost btn-sm" onClick={() => setEditing(rule)}>
                              {t('adminAlerts.edit')}
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

      <CreateAlertModal
        key={editing ? editing.id : 'new'}
        open={createOpen || Boolean(editing)}
        rule={editing}
        triggers={alerts.data?.triggers ?? []}
        emailEnabled={alerts.data?.email_enabled ?? false}
        onClose={() => {
          setCreateOpen(false);
          setEditing(null);
        }}
        onCreated={() => {
          setCreateOpen(false);
          setEditing(null);
          void queryClient.invalidateQueries({ queryKey: ['admin', 'alerts'] });
        }}
      />

      <ConfirmDialog
        open={Boolean(deleting)}
        onClose={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
        title={t('adminAlerts.deleteTitle')}
        consequence={t('adminAlerts.deleteConsequence')}
        confirmLabel={t('adminAlerts.deleteConfirm')}
        busy={remove.isPending}
      />
    </div>
  );
}

function CreateAlertModal({
  open,
  rule,
  triggers,
  emailEnabled,
  onClose,
  onCreated,
}: {
  open: boolean;
  rule?: AlertRule | null;
  triggers: Array<{ value: string; label: string }>;
  emailEnabled: boolean;
  onClose: () => void;
  onCreated: () => void;
}): ReactNode {
  const emailLicensed = useLicensed('email_alerts');
  const toast = useToast();
  const [trigger, setTrigger] = useState(rule?.trigger ?? 'quota_breach');
  const [severity, setSeverity] = useState(rule?.severity ?? 'warning');
  const [channels, setChannels] = useState<string[]>(rule?.channels ?? ['in_app']);
  const [webhookURL, setWebhookURL] = useState(rule?.webhook_url ?? '');

  const create = useMutation({
    mutationFn: () => {
      const payload = {
        trigger,
        severity,
        channels,
        webhook_url: webhookURL.trim(),
        enabled: rule ? rule.enabled : true,
      };
      return rule ? api.put(`/api/v1/admin/alerts/${rule.id}`, payload) : api.post('/api/v1/admin/alerts', payload);
    },
    onSuccess: () => {
      toast(rule ? t('adminAlerts.updatedToast') : t('adminAlerts.createdToast'));
      setWebhookURL('');
      onCreated();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  const webhookSelected = channels.includes('webhook');
  const webhookError = webhookSelected && !/^https?:\/\/.+/.test(webhookURL.trim()) ? t('adminAlerts.webhookError') : undefined;
  const valid = channels.length > 0 && !webhookError;

  const toggleChannel = (channel: string) => {
    setChannels((current) => (current.includes(channel) ? current.filter((item) => item !== channel) : [...current, channel]));
  };

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={rule ? t('adminAlerts.editTitle') : t('adminAlerts.newRule')}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={create.isPending}>
            {t('common.cancel')}
          </button>
          <button type="button" className="btn btn-primary" onClick={() => create.mutate()} disabled={!valid || create.isPending}>
            {create.isPending ? t('tables.saving') : rule ? t('adminUpstreams.saveChanges') : t('adminAlerts.createRule')}
          </button>
        </>
      }
    >
      <Field label={t('adminAlerts.colTrigger')} required>
        <select className="select" value={trigger} onChange={(event) => setTrigger(event.target.value)}>
          {triggers.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </Field>

      <Field label={t('adminAlerts.colSeverity')}>
        <select className="select" value={severity} onChange={(event) => setSeverity(event.target.value)}>
          <option value="info">{t('adminAlerts.severityInfo')}</option>
          <option value="warning">{t('adminAlerts.severityWarning')}</option>
          <option value="critical">{t('adminAlerts.severityCritical')}</option>
        </select>
      </Field>

      <Field label={t('adminAlerts.colChannels')} required>
        <div className="stack" style={{ gap: 4 }}>
          <label className="switch">
            <input type="checkbox" checked={channels.includes('in_app')} onChange={() => toggleChannel('in_app')} />
            <span>{t('adminAlerts.inAppInbox')}</span>
          </label>
          <label className="switch">
            <input
              type="checkbox"
              checked={channels.includes('email')}
              onChange={() => toggleChannel('email')}
              disabled={!emailLicensed}
            />
            <span>
              {t('adminAlerts.email')}{' '}
              {!emailLicensed ? (
                <span className="small muted">{t('adminAlerts.emailUpsell')}</span>
              ) : !emailEnabled ? (
                <span className="small muted">{t('adminAlerts.smtpNotConfigured')}</span>
              ) : null}
            </span>
          </label>
          <label className="switch">
            <input type="checkbox" checked={webhookSelected} onChange={() => toggleChannel('webhook')} />
            <span>{t('adminAlerts.webhook')}</span>
          </label>
        </div>
      </Field>

      {webhookSelected ? (
        <Field label={t('adminAlerts.webhookUrl')} required error={webhookError} hint={t('adminAlerts.webhookHint')}>
          <input
            className="input"
            value={webhookURL}
            onChange={(event) => setWebhookURL(event.target.value)}
            placeholder="https://chat.example.com/hooks/…"
            aria-invalid={Boolean(webhookError)}
          />
        </Field>
      ) : null}
    </Modal>
  );
}
