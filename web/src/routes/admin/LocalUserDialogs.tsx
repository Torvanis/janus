import { useState, type FormEvent, type ReactNode } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { AdminUserRow } from '../../lib/types';
import { Field, Modal, useToast } from '../../components/ui';
import { t } from '../../lib/i18n';

/**
 * Admin → People: create a Janus account (email + temporary password) and
 * reset a user's password. Both hand the user a temporary password that
 * must be changed at first sign-in; the admin never sets a permanent one.
 */
export function CreateLocalUserModal({ open, onClose }: { open: boolean; onClose: () => void }): ReactNode {
  const queryClient = useQueryClient();
  const toast = useToast();
  const [email, setEmail] = useState('');
  const [name, setName] = useState('');
  const [password, setPassword] = useState('');
  const [admin, setAdmin] = useState(false);

  const create = useMutation({
    mutationFn: () => api.post<AdminUserRow>('/api/v1/admin/users/local', { email, name, password, admin }),
    onSuccess: (u) => {
      toast(t('adminPeople.localCreated', { name: u.email }));
      setEmail('');
      setName('');
      setPassword('');
      setAdmin(false);
      onClose();
      void queryClient.invalidateQueries({ queryKey: ['admin', 'users'] });
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });

  function submit(event: FormEvent) {
    event.preventDefault();
    if (email && password.length >= 12) create.mutate();
  }

  return (
    <Modal open={open} onClose={onClose} title={t('adminPeople.localCreateTitle')} description={t('adminPeople.localCreateBody')}>
      <form className="stack" onSubmit={submit}>
        <Field label={t('login.workEmail')}>
          <input
            className="input"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoFocus
            autoComplete="off"
          />
        </Field>
        <Field label={t('login.name')}>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} autoComplete="off" />
        </Field>
        <Field label={t('adminPeople.tempPassword')} hint={t('adminPeople.tempPasswordHint')}>
          <input
            className="input"
            type="text"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="off"
            style={{ fontFamily: 'var(--janus-font-mono)' }}
          />
        </Field>
        <label className="row" style={{ gap: 'var(--janus-space-2)' }}>
          <input type="checkbox" checked={admin} onChange={(e) => setAdmin(e.target.checked)} />
          <span>{t('adminPeople.makeAdmin')}</span>
        </label>
        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn btn-ghost" onClick={onClose}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={!email || password.length < 12 || create.isPending}>
            {t('adminPeople.localCreate')}
          </button>
        </div>
      </form>
    </Modal>
  );
}

export function ResetPasswordModal({ user, onClose }: { user: AdminUserRow | null; onClose: () => void }): ReactNode {
  const toast = useToast();
  const [password, setPassword] = useState('');
  const reset = useMutation({
    mutationFn: () => api.post<{ reset: boolean }>(`/api/v1/admin/users/${user?.id}/password`, { password }),
    onSuccess: () => {
      toast(t('adminPeople.passwordReset', { name: user?.email ?? '' }));
      setPassword('');
      onClose();
    },
    onError: (error: Error) => toast(error.message, 'danger'),
  });
  return (
    <Modal
      open={Boolean(user)}
      onClose={onClose}
      title={t('adminPeople.resetPasswordTitle', { name: user?.email ?? '' })}
      description={t('adminPeople.resetPasswordBody')}
    >
      <form
        className="stack"
        onSubmit={(e) => {
          e.preventDefault();
          if (password.length >= 12) reset.mutate();
        }}
      >
        <Field label={t('adminPeople.tempPassword')} hint={t('adminPeople.tempPasswordHint')}>
          <input
            className="input"
            type="text"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoFocus
            autoComplete="off"
            style={{ fontFamily: 'var(--janus-font-mono)' }}
          />
        </Field>
        <div className="row" style={{ justifyContent: 'flex-end', gap: 'var(--janus-space-2)' }}>
          <button type="button" className="btn btn-ghost" onClick={onClose}>
            {t('common.cancel')}
          </button>
          <button type="submit" className="btn btn-primary" disabled={password.length < 12 || reset.isPending}>
            {t('adminPeople.resetPassword')}
          </button>
        </div>
      </form>
    </Modal>
  );
}
