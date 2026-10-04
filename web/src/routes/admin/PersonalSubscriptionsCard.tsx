import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { AdminSubscriptionProvider, AdminSubscriptions, SubscriptionConnection } from '../../lib/types';
import { formatRelative } from '../../lib/format';
import { AsyncSection, Badge, ConfirmDialog, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';

/**
 * Settings → General card for personal subscriptions: the organization-wide
 * switch, which providers people may connect (each with its vendor-terms
 * note), and every connection. Hidden entirely on an offline install, where
 * the feature cannot work.
 */
export function PersonalSubscriptionsCard(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [confirmFeature, setConfirmFeature] = useState<boolean | null>(null);
  const [confirmDisconnect, setConfirmDisconnect] = useState<SubscriptionConnection | null>(null);
  const [showConnections, setShowConnections] = useState(false);

  const data = useQuery({
    queryKey: ['admin', 'subscriptions'],
    queryFn: () => api.get<AdminSubscriptions>('/api/v1/admin/subscriptions'),
  });
  const invalidate = (): void => {
    void queryClient.invalidateQueries({ queryKey: ['admin', 'subscriptions'] });
    void queryClient.invalidateQueries({ queryKey: ['me'] });
  };
  const providerName = (id: string): string => data.data?.providers.find((p) => p.id === id)?.name ?? id;

  const setFeature = useMutation({
    mutationFn: (enabled: boolean) => api.put('/api/v1/admin/subscriptions/feature', { enabled }),
    onSuccess: () => {
      setConfirmFeature(null);
      toast(t('adminSystem.flagUpdatedToast'));
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const toggleProvider = useMutation({
    mutationFn: (p: AdminSubscriptionProvider) =>
      api.put('/api/v1/admin/subscriptions/providers', { enabled: { [p.id]: !p.enabled } }),
    onSuccess: invalidate,
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const disconnect = useMutation({
    mutationFn: (c: SubscriptionConnection) => api.del(`/api/v1/admin/subscriptions/${encodeURIComponent(c.id)}`),
    onSuccess: () => {
      setConfirmDisconnect(null);
      invalidate();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  if (data.data?.offline) return null;

  return (
    <section className="card" id="personal-subscriptions" aria-labelledby="personal-subscriptions-heading">
      <AsyncSection query={data}>
        {(d) => (
          <div className="stack">
            <div className="row-between" style={{ gap: 'var(--janus-space-4)', alignItems: 'flex-start', flexWrap: 'wrap' }}>
              <div style={{ minWidth: 0, flex: '1 1 320px' }}>
                <div className="row" style={{ gap: 'var(--janus-space-3)' }}>
                  <h2 id="personal-subscriptions-heading" className="card-title" style={{ margin: 0 }}>
                    {t('adminSubscriptions.featureLabel')}
                  </h2>
                  <Badge tone={d.enabled ? 'success' : 'neutral'} dot>
                    {d.enabled ? t('adminSubscriptions.enabled') : t('adminSubscriptions.disabled')}
                  </Badge>
                </div>
                <p className="small secondary" style={{ margin: 'var(--janus-space-1) 0 0' }}>
                  {t('adminSubscriptions.featureDescription')}
                </p>
              </div>
              <button
                type="button"
                className={d.enabled ? 'btn' : 'btn btn-primary'}
                onClick={() => setConfirmFeature(!d.enabled)}
              >
                {d.enabled ? t('adminSubscriptions.disable') : t('adminSubscriptions.enable')}
              </button>
            </div>

            {d.enabled ? (
              <>
                <div>
                  <strong className="small">{t('adminSubscriptions.vendorsTitle')}</strong>
                  <ul className="stack" style={{ listStyle: 'none', margin: 'var(--janus-space-2) 0 0', padding: 0 }}>
                    {d.providers.map((p) => (
                      <li key={p.id} style={{ minWidth: 0 }}>
                        <label className="row" style={{ gap: 'var(--janus-space-2)', cursor: 'pointer', flexWrap: 'wrap' }}>
                          <input
                            type="checkbox"
                            checked={p.enabled}
                            disabled={toggleProvider.isPending}
                            onChange={() => toggleProvider.mutate(p)}
                            aria-label={p.name}
                          />
                          <strong>{p.name}</strong>
                          <span className="small secondary">
                            {t('adminSubscriptions.connectionsCount', { count: p.connections })}
                          </span>
                        </label>
                        {p.admin_note ? (
                          <p className="small secondary" style={{ margin: 'var(--janus-space-1) 0 0 26px' }}>
                            <strong>{t('adminSubscriptions.termsNote')}:</strong> {p.admin_note}
                          </p>
                        ) : null}
                      </li>
                    ))}
                  </ul>
                  <p className="small secondary" style={{ margin: 'var(--janus-space-2) 0 0' }}>
                    {t('adminSubscriptions.disabledNote')}
                  </p>
                </div>

                <div>
                  <button
                    type="button"
                    className="btn btn-ghost btn-sm"
                    aria-expanded={showConnections}
                    onClick={() => setShowConnections((v) => !v)}
                  >
                    {showConnections ? t('adminSubscriptions.hideConnections') : t('adminSubscriptions.viewConnections')}
                    {' · '}
                    {t('adminSubscriptions.connectionsSummary', { count: d.connections.length })}
                  </button>
                  {showConnections ? (
                    d.connections.length === 0 ? (
                      <p className="small secondary">{t('adminSubscriptions.noConnections')}</p>
                    ) : (
                      <div className="table-scroll" style={{ marginTop: 'var(--janus-space-2)' }}>
                        <table className="data">
                          <thead>
                            <tr>
                              <th>{t('adminSubscriptions.colUser')}</th>
                              <th>{t('adminSubscriptions.colProvider')}</th>
                              <th>{t('adminSubscriptions.colAccount')}</th>
                              <th>{t('adminSubscriptions.colStatus')}</th>
                              <th>{t('adminSubscriptions.colModels')}</th>
                              <th>{t('adminSubscriptions.colLastUsed')}</th>
                              <th />
                            </tr>
                          </thead>
                          <tbody>
                            {d.connections.map((c) => (
                              <tr key={c.id}>
                                <td>{c.user_email || c.user_id}</td>
                                <td>{providerName(c.provider)}</td>
                                <td className="secondary">{c.account_email || c.account_name}</td>
                                <td>
                                  {c.status === 'active' ? (
                                    <Badge tone="success">{t('subscriptions.statusActive')}</Badge>
                                  ) : (
                                    <Badge tone="warning">{t('subscriptions.statusReauth')}</Badge>
                                  )}
                                </td>
                                <td>
                                  {(c.selected_models ?? []).length} / {c.models.length}
                                </td>
                                <td className="secondary">
                                  {c.last_used_at && !c.last_used_at.startsWith('0001') ? formatRelative(c.last_used_at) : '—'}
                                </td>
                                <td>
                                  <div className="row-actions">
                                    <button
                                      type="button"
                                      className="btn btn-sm btn-danger"
                                      onClick={() => setConfirmDisconnect(c)}
                                    >
                                      {t('subscriptions.disconnect')}
                                    </button>
                                  </div>
                                </td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      </div>
                    )
                  ) : null}
                </div>
              </>
            ) : null}
          </div>
        )}
      </AsyncSection>

      <ConfirmDialog
        open={confirmFeature !== null}
        onClose={() => setConfirmFeature(null)}
        onConfirm={() => confirmFeature !== null && setFeature.mutate(confirmFeature)}
        title={confirmFeature ? t('adminSubscriptions.featureOnTitle') : t('adminSubscriptions.featureOffTitle')}
        consequence={
          confirmFeature ? t('adminSubscriptions.featureOnConsequence') : t('adminSubscriptions.featureOffConsequence')
        }
        confirmLabel={confirmFeature ? t('adminSubscriptions.enable') : t('adminSubscriptions.disable')}
        busy={setFeature.isPending}
      />
      <ConfirmDialog
        open={confirmDisconnect !== null}
        onClose={() => setConfirmDisconnect(null)}
        onConfirm={() => confirmDisconnect && disconnect.mutate(confirmDisconnect)}
        title={t('adminSubscriptions.disconnectTitle', {
          user: confirmDisconnect?.user_email ?? '',
          provider: confirmDisconnect ? providerName(confirmDisconnect.provider) : '',
        })}
        consequence={t('adminSubscriptions.disconnectConsequence', { user: confirmDisconnect?.user_email ?? '' })}
        confirmLabel={t('subscriptions.disconnect')}
        busy={disconnect.isPending}
      />
    </section>
  );
}
