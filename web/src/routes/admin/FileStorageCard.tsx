import { useEffect, useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { FileStorageDocument, FileStorageTestResult } from '../../lib/types';
import { formatBytes } from '../../lib/format';
import { AsyncSection, Badge, Field, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';

interface StorageForm {
  mode: 'database' | 's3';
  endpoint: string;
  bucket: string;
  region: string;
  pathStyle: boolean;
  prefix: string;
  accessKeyId: string;
  secret: string;
  caBundle: string;
}

function formFrom(doc: FileStorageDocument): StorageForm {
  return {
    mode: doc.mode,
    endpoint: doc.s3.endpoint,
    bucket: doc.s3.bucket,
    region: doc.s3.region || 'us-east-1',
    pathStyle: doc.s3.path_style,
    prefix: doc.s3.prefix,
    accessKeyId: doc.s3.access_key_id,
    secret: '',
    caBundle: doc.s3.ca_bundle,
  };
}

function requestBody(form: StorageForm): Record<string, unknown> {
  return {
    mode: form.mode,
    s3: {
      endpoint: form.endpoint,
      bucket: form.bucket,
      region: form.region,
      path_style: form.pathStyle,
      prefix: form.prefix,
      access_key_id: form.accessKeyId,
      secret_access_key: form.secret,
      ca_bundle: form.caBundle,
    },
  };
}

/**
 * Settings → General card for file storage: database (default) or an
 * S3-compatible bucket, shared by chat attachments and troubleshooting
 * captures. Fields the deployment sets (JANUS_STORAGE_MODE, JANUS_S3_*) are
 * read-only. The secret is write-only: the API only reports whether one is
 * stored.
 */
export function FileStorageCard(): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const data = useQuery({
    queryKey: ['admin', 'storage'],
    queryFn: () => api.get<FileStorageDocument>('/api/v1/admin/storage'),
  });
  const [form, setForm] = useState<StorageForm | null>(null);
  const [testResult, setTestResult] = useState<FileStorageTestResult | null>(null);

  useEffect(() => {
    if (data.data && form === null) setForm(formFrom(data.data));
  }, [data.data, form]);

  const update = (patch: Partial<StorageForm>): void => {
    setForm((prev) => (prev ? { ...prev, ...patch } : prev));
    setTestResult(null);
  };

  const save = useMutation({
    mutationFn: (f: StorageForm) => api.put<FileStorageDocument>('/api/v1/admin/storage', requestBody(f)),
    onSuccess: (doc) => {
      queryClient.setQueryData(['admin', 'storage'], doc);
      setForm(formFrom(doc));
      toast(t('adminStorage.saved'));
      void queryClient.invalidateQueries({ queryKey: ['admin', 'troubleshooting'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  const test = useMutation({
    mutationFn: (f: StorageForm) => api.post<FileStorageTestResult>('/api/v1/admin/storage/test', requestBody(f)),
    onSuccess: (result) => setTestResult(result),
    onError: (error: Error) => setTestResult({ ok: false, mode: '', error: error.message }),
  });

  return (
    <section className="card" id="file-storage" aria-labelledby="file-storage-heading">
      <AsyncSection query={data}>
        {(doc) => {
          const f = form ?? formFrom(doc);
          const locked = doc.env_managed.includes('mode');
          const busy = save.isPending || test.isPending;
          return (
            <div className="stack">
              <div>
                <div className="row" style={{ gap: 'var(--janus-space-3)' }}>
                  <h2 id="file-storage-heading" className="card-title" style={{ margin: 0 }}>
                    {t('adminStorage.title')}
                  </h2>
                  <Badge tone="neutral">{doc.mode === 's3' ? t('adminStorage.modeS3') : t('adminStorage.modeDatabase')}</Badge>
                  {locked ? <Badge tone="info">{t('adminStorage.envManaged')}</Badge> : null}
                </div>
                <p className="small secondary" style={{ margin: 'var(--janus-space-1) 0 0' }}>
                  {t('adminStorage.description')}
                </p>
              </div>
              {locked ? <p className="small muted">{t('adminStorage.envManagedNote')}</p> : null}

              <fieldset className="stack" disabled={locked || busy} style={{ border: 0, padding: 0, margin: 0 }}>
                <legend className="field-label">{t('adminStorage.modeLabel')}</legend>
                <label className="row" style={{ gap: 'var(--janus-space-2)', alignItems: 'flex-start' }}>
                  <input
                    type="radio"
                    name="file-storage-mode"
                    value="database"
                    checked={f.mode === 'database'}
                    onChange={() => update({ mode: 'database' })}
                    aria-label={t('adminStorage.modeDatabase')}
                  />
                  <span>
                    <strong>{t('adminStorage.modeDatabase')}</strong>
                    <span className="small secondary" style={{ display: 'block' }}>
                      {t('adminStorage.modeDatabaseHint')}
                    </span>
                  </span>
                </label>
                <label className="row" style={{ gap: 'var(--janus-space-2)', alignItems: 'flex-start' }}>
                  <input
                    type="radio"
                    name="file-storage-mode"
                    value="s3"
                    checked={f.mode === 's3'}
                    onChange={() => update({ mode: 's3' })}
                    aria-label={t('adminStorage.modeS3')}
                  />
                  <span>
                    <strong>{t('adminStorage.modeS3')}</strong>
                    <span className="small secondary" style={{ display: 'block' }}>
                      {t('adminStorage.modeS3Hint')}
                    </span>
                  </span>
                </label>

                {f.mode === 's3' ? (
                  <div className="stack">
                    <Field label={t('adminStorage.endpoint')} hint={t('adminStorage.endpointHint')} required>
                      <input
                        className="input"
                        aria-label={t('adminStorage.endpoint')}
                        value={f.endpoint}
                        onChange={(e) => update({ endpoint: e.target.value })}
                        placeholder="https://"
                      />
                    </Field>
                    <Field label={t('adminStorage.bucket')} hint={t('adminStorage.bucketHint')} required>
                      <input
                        className="input"
                        aria-label={t('adminStorage.bucket')}
                        value={f.bucket}
                        onChange={(e) => update({ bucket: e.target.value })}
                      />
                    </Field>
                    <Field label={t('adminStorage.region')}>
                      <input
                        className="input"
                        aria-label={t('adminStorage.region')}
                        value={f.region}
                        onChange={(e) => update({ region: e.target.value })}
                      />
                    </Field>
                    <label className="row small" style={{ gap: 'var(--janus-space-2)' }}>
                      <input
                        type="checkbox"
                        checked={f.pathStyle}
                        onChange={(e) => update({ pathStyle: e.target.checked })}
                        aria-label={t('adminStorage.pathStyle')}
                      />
                      <span>
                        {t('adminStorage.pathStyle')}
                        <span className="secondary"> · {t('adminStorage.pathStyleHint')}</span>
                      </span>
                    </label>
                    <Field label={t('adminStorage.prefix')} hint={t('adminStorage.prefixHint')}>
                      <input
                        className="input"
                        aria-label={t('adminStorage.prefix')}
                        value={f.prefix}
                        onChange={(e) => update({ prefix: e.target.value })}
                      />
                    </Field>
                    <Field label={t('adminStorage.accessKeyId')} required>
                      <input
                        className="input"
                        aria-label={t('adminStorage.accessKeyId')}
                        autoComplete="off"
                        value={f.accessKeyId}
                        onChange={(e) => update({ accessKeyId: e.target.value })}
                      />
                    </Field>
                    <Field label={t('adminStorage.secretAccessKey')} hint={t('adminStorage.secretHint')} required>
                      <input
                        className="input"
                        type="password"
                        aria-label={t('adminStorage.secretAccessKey')}
                        autoComplete="new-password"
                        value={f.secret}
                        placeholder={doc.s3.secret_set ? t('adminStorage.secretUnchanged') : ''}
                        onChange={(e) => update({ secret: e.target.value })}
                      />
                    </Field>
                    <Field label={t('adminStorage.caBundle')} hint={t('adminStorage.caBundleHint')}>
                      <textarea
                        className="textarea"
                        aria-label={t('adminStorage.caBundle')}
                        rows={3}
                        value={f.caBundle}
                        onChange={(e) => update({ caBundle: e.target.value })}
                        placeholder="-----BEGIN CERTIFICATE-----"
                      />
                    </Field>
                  </div>
                ) : null}
              </fieldset>

              <p className="small secondary" style={{ margin: 0 }}>
                {t('adminStorage.existingStay')}
                {' · '}
                {t('adminStorage.usage', {
                  count: doc.usage.database.objects,
                  size: formatBytes(doc.usage.database.bytes),
                })}
                {' · '}
                {t('adminStorage.maxObject', { size: formatBytes(doc.max_object_bytes) })}
              </p>

              {testResult ? (
                <div
                  className={testResult.ok ? 'banner banner-info small' : 'banner banner-danger small'}
                  role={testResult.ok ? 'status' : 'alert'}
                >
                  {testResult.ok
                    ? t('adminStorage.testOk', { time: String(testResult.latency_ms ?? 0) })
                    : t('adminStorage.testFailed', { error: testResult.error ?? '' })}
                </div>
              ) : null}

              <div className="row" style={{ gap: 'var(--janus-space-2)' }}>
                <button type="button" className="btn" disabled={busy} onClick={() => test.mutate(f)}>
                  {test.isPending ? t('adminStorage.testing') : t('adminStorage.test')}
                </button>
                {!locked ? (
                  <button type="button" className="btn btn-primary" disabled={busy} onClick={() => save.mutate(f)}>
                    {t('adminStorage.save')}
                  </button>
                ) : null}
              </div>
            </div>
          );
        }}
      </AsyncSection>
    </section>
  );
}
