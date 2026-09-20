import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { ApiError } from '../lib/api';
import { t } from '../lib/i18n';

/* --- Loading, empty, and error states ------------------------------------- */

export function Spinner({ label }: { label?: string }): ReactNode {
  label ??= t('common.loading');
  return (
    <span className="row" role="status" aria-live="polite">
      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r="6" fill="none" stroke="var(--janus-color-border-strong)" strokeWidth="2" />
        <path d="M8 2a6 6 0 0 1 6 6" fill="none" stroke="var(--janus-color-primary-bg)" strokeWidth="2" strokeLinecap="round">
          <animateTransform
            attributeName="transform"
            type="rotate"
            from="0 8 8"
            to="360 8 8"
            dur="900ms"
            repeatCount="indefinite"
          />
        </path>
      </svg>
      <span className="small muted">{label}</span>
    </span>
  );
}

export function SkeletonRows({ rows = 5, height = 18 }: { rows?: number; height?: number }): ReactNode {
  return (
    <div className="stack" aria-hidden="true">
      {Array.from({ length: rows }, (_, index) => (
        <div key={index} className="skeleton" style={{ height, width: `${100 - index * 4}%` }} />
      ))}
    </div>
  );
}

export function EmptyState({
  title,
  body,
  action,
  icon = '◇',
}: {
  title: string;
  body: string;
  action?: ReactNode;
  icon?: string;
}): ReactNode {
  return (
    <div className="state">
      <div aria-hidden="true" style={{ fontSize: 28, color: 'var(--janus-color-text-muted)' }}>
        {icon}
      </div>
      <div className="state-title">{title}</div>
      <p className="state-body">{body}</p>
      {action}
    </div>
  );
}

/**
 * Renders a failure as a human sentence plus the gateway's error code and
 * request id, never a stack trace or a bare status number.
 */
function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }): ReactNode {
  const apiError = error instanceof ApiError ? error : null;
  const message = apiError?.payload.message ?? (error instanceof Error ? error.message : t('errorState.fallbackBody'));

  return (
    <div className="state" role="alert">
      <div aria-hidden="true" style={{ fontSize: 28, color: 'var(--janus-color-danger-fg)' }}>
        !
      </div>
      <div className="state-title">{t('errorState.title')}</div>
      <p className="state-body">{message}</p>
      {apiError?.payload.reason ? <p className="state-body small muted">{apiError.payload.reason}</p> : null}
      <div className="row">
        {onRetry ? (
          <button type="button" className="btn" onClick={onRetry}>
            {t('common.tryAgain')}
          </button>
        ) : null}
        {apiError?.payload.docs_url ? (
          <a className="btn btn-ghost" href={apiError.payload.docs_url}>
            {t('errorState.whatDoesThisMean')}
          </a>
        ) : null}
      </div>
      {apiError?.payload.request_id ? (
        <p className="small muted mono">{t('errorState.requestId', { id: apiError.payload.request_id })}</p>
      ) : null}
    </div>
  );
}

/**
 * One place that decides between loading, error, empty, and content, so no view
 * can accidentally ship only the happy path.
 */
export function AsyncSection<T>({
  query,
  empty,
  children,
  skeleton,
}: {
  query: { data: T | undefined; isLoading: boolean; error: unknown; refetch: () => void };
  empty?: { when: (data: T) => boolean; title: string; body: string; action?: ReactNode };
  children: (data: T) => ReactNode;
  skeleton?: ReactNode;
}): ReactNode {
  if (query.isLoading && query.data === undefined) {
    return <>{skeleton ?? <SkeletonRows />}</>;
  }
  if (query.error && query.data === undefined) {
    return <ErrorState error={query.error} onRetry={query.refetch} />;
  }
  if (query.data === undefined) {
    return <SkeletonRows />;
  }
  return (
    <>
      {query.error ? (
        <div className="banner banner-warning" role="alert">
          Refresh failed. Showing previously loaded data.{' '}
          <button type="button" className="btn btn-sm" onClick={query.refetch}>
            Retry
          </button>
        </div>
      ) : null}
      {empty?.when(query.data) ? (
        <EmptyState title={empty.title} body={empty.body} action={empty.action} />
      ) : (
        children(query.data)
      )}
    </>
  );
}

/* --- Badge ---------------------------------------------------------------- */

export type Tone = 'neutral' | 'success' | 'warning' | 'danger' | 'info' | 'primary';

/**
 * Disclosure chevron: points down when open, right when collapsed. An inline
 * SVG rather than a glyph so it renders identically on every platform font.
 */
export function Chevron({ open, className }: { open: boolean; className?: string }): ReactNode {
  return (
    <svg
      width="14"
      height="14"
      viewBox="0 0 16 16"
      aria-hidden="true"
      className={`chevron${open ? '' : ' is-collapsed'}${className ? ` ${className}` : ''}`}
    >
      <path
        d="M3.5 6l4.5 4.5L12.5 6"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

export function Badge({ tone = 'neutral', children, dot }: { tone?: Tone; children: ReactNode; dot?: boolean }): ReactNode {
  return <span className={`badge badge-${tone}${dot ? ' badge-dot' : ''}`}>{children}</span>;
}

/* --- Modal & drawer ------------------------------------------------------- */

function useEscape(onClose: () => void): void {
  useEffect(() => {
    const handler = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', handler);
    return () => window.removeEventListener('keydown', handler);
  }, [onClose]);
}

/** Keeps keyboard focus inside an overlay while it is open. */
function useFocusTrap(open: boolean) {
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open || !ref.current) return undefined;
    const container = ref.current;
    const previous = document.activeElement as HTMLElement | null;
    const focusable = container.querySelector<HTMLElement>(
      'button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])',
    );
    focusable?.focus();

    const handler = (event: KeyboardEvent) => {
      if (event.key !== 'Tab') return;
      const items = Array.from(
        container.querySelectorAll<HTMLElement>('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])'),
      ).filter((element) => !element.hasAttribute('disabled'));
      if (items.length === 0) return;
      const first = items[0]!;
      const last = items[items.length - 1]!;
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    container.addEventListener('keydown', handler);
    return () => {
      container.removeEventListener('keydown', handler);
      previous?.focus();
    };
  }, [open]);
  return ref;
}

export function Modal({
  open,
  onClose,
  title,
  description,
  children,
  footer,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  description?: string;
  children: ReactNode;
  footer?: ReactNode;
}): ReactNode {
  useEscape(onClose);
  const ref = useFocusTrap(open);
  const titleId = useId();
  if (!open) return null;

  return (
    <>
      <div className="scrim" onClick={onClose} aria-hidden="true" />
      <div className="modal" role="dialog" aria-modal="true" aria-labelledby={titleId} ref={ref}>
        <div className="row-between" style={{ marginBottom: 'var(--janus-space-4)' }}>
          <h2 id={titleId}>{title}</h2>
          <button type="button" className="btn btn-ghost btn-sm" onClick={onClose} aria-label={t('common.closeDialog')}>
            ✕
          </button>
        </div>
        {description ? <p className="secondary small">{description}</p> : null}
        <div className="stack">{children}</div>
        {footer ? (
          <div className="row" style={{ marginTop: 'var(--janus-space-5)', justifyContent: 'flex-end' }}>
            {footer}
          </div>
        ) : null}
      </div>
    </>
  );
}

export function Drawer({
  open,
  onClose,
  title,
  children,
}: {
  open: boolean;
  onClose: () => void;
  title: string;
  children: ReactNode;
}): ReactNode {
  useEscape(onClose);
  const ref = useFocusTrap(open);
  const titleId = useId();
  if (!open) return null;

  return (
    <>
      <div className="scrim" onClick={onClose} aria-hidden="true" />
      <aside className="drawer" role="dialog" aria-modal="true" aria-labelledby={titleId} ref={ref}>
        <header className="drawer-header">
          <h2 id={titleId}>{title}</h2>
          <button type="button" className="btn btn-ghost btn-sm" onClick={onClose} aria-label={t('common.closePanel')}>
            ✕
          </button>
        </header>
        <div className="drawer-body">{children}</div>
      </aside>
    </>
  );
}

/**
 * Confirmation for destructive or high-consequence actions. The consequence is
 * always spelled out, and irreversible actions additionally require the user to
 * type the resource name.
 */
export function ConfirmDialog({
  open,
  onClose,
  onConfirm,
  title,
  consequence,
  confirmLabel,
  requireTyped,
  danger = true,
  busy = false,
  confirmDisabled = false,
  children,
}: {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  title: string;
  consequence: string;
  confirmLabel?: string;
  requireTyped?: string;
  danger?: boolean;
  busy?: boolean;
  /**
   * Blocks the confirm button beyond the typed-name check — for dialogs that
   * need a further explicit acknowledgement (e.g. a "force" checkbox) before
   * the destructive action may proceed.
   */
  confirmDisabled?: boolean;
  /** Extra detail rendered between the consequence and the typed-name field. */
  children?: ReactNode;
}): ReactNode {
  confirmLabel ??= t('common.confirm');
  const [typed, setTyped] = useState('');
  useEffect(() => {
    if (open) setTyped('');
  }, [open]);

  const ready = (!requireTyped || typed.trim() === requireTyped) && !confirmDisabled;

  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose} disabled={busy}>
            {t('common.cancel')}
          </button>
          <button
            type="button"
            className={danger ? 'btn btn-danger' : 'btn btn-primary'}
            onClick={onConfirm}
            disabled={!ready || busy}
          >
            {busy ? t('common.working') : confirmLabel}
          </button>
        </>
      }
    >
      <p className="secondary">{consequence}</p>
      {children}
      {requireTyped ? (
        <label className="field">
          <span className="field-label">
            {t('confirmDialog.typePrefix')} <strong className="mono">{requireTyped}</strong> {t('confirmDialog.typeSuffix')}
          </span>
          <input
            className="input"
            value={typed}
            onChange={(event) => setTyped(event.target.value)}
            placeholder={requireTyped}
            autoComplete="off"
          />
        </label>
      ) : null}
    </Modal>
  );
}

/* --- Copy button ---------------------------------------------------------- */

export function CopyButton({ value, label }: { value: string; label?: string }): ReactNode {
  label ??= t('common.copy');
  const [copied, setCopied] = useState(false);

  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Clipboard access can be blocked; fall back to a selection the user can copy.
      const area = document.createElement('textarea');
      area.value = value;
      document.body.appendChild(area);
      area.select();
      document.execCommand('copy');
      document.body.removeChild(area);
    }
    setCopied(true);
    window.setTimeout(() => setCopied(false), 1800);
  }, [value]);

  return (
    <button type="button" className="btn btn-ghost btn-sm" onClick={copy} aria-live="polite">
      {copied ? t('common.copied') : label}
    </button>
  );
}

export function CodeBlock({ code, language }: { code: string; language?: string }): ReactNode {
  return (
    <div className="code-block">
      <div className="code-block-bar">
        <span className="overline">{language ?? t('common.code')}</span>
        <CopyButton value={code} />
      </div>
      <pre>
        <code>{code}</code>
      </pre>
    </div>
  );
}

/* --- Toast ---------------------------------------------------------------- */

interface Toast {
  id: number;
  tone: Tone;
  message: string;
}

const ToastContext = createContext<(message: string, tone?: Tone) => void>(() => {});

export function ToastProvider({ children }: { children: ReactNode }): ReactNode {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const push = useCallback((message: string, tone: Tone = 'success') => {
    const id = Date.now() + Math.random();
    setToasts((current) => [...current, { id, tone, message }]);
    window.setTimeout(() => setToasts((current) => current.filter((toast) => toast.id !== id)), 5000);
  }, []);

  const value = useMemo(() => push, [push]);

  return (
    <ToastContext.Provider value={value}>
      {children}
      <div
        aria-live="polite"
        style={{
          position: 'fixed',
          bottom: 'calc(var(--janus-space-6) + env(safe-area-inset-bottom))',
          right: 'var(--janus-space-6)',
          zIndex: 1000,
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--janus-space-2)',
          maxWidth: 'min(420px, calc(100vw - 32px))',
        }}
      >
        {toasts.map((toast) => (
          <div
            key={toast.id}
            className={`banner banner-${toast.tone === 'danger' ? 'danger' : 'info'}`}
            style={{ boxShadow: 'var(--janus-shadow-3)' }}
          >
            {toast.message}
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

export function useToast(): (message: string, tone?: Tone) => void {
  return useContext(ToastContext);
}

/* --- Field ---------------------------------------------------------------- */

export function Field({
  label,
  hint,
  error,
  children,
  required,
}: {
  label: ReactNode;
  hint?: string;
  error?: string;
  children: ReactNode;
  required?: boolean;
}): ReactNode {
  return (
    <label className="field">
      <span className="field-label">
        {label}
        {required ? (
          <span aria-hidden="true" style={{ color: 'var(--janus-color-danger-fg)' }}>
            {' '}
            *
          </span>
        ) : null}
      </span>
      {children}
      {error ? (
        <span className="field-error" role="alert">
          {error}
        </span>
      ) : hint ? (
        <span className="field-hint">{hint}</span>
      ) : null}
    </label>
  );
}

/* --- Pagination ----------------------------------------------------------- */

export function Pagination({
  offset,
  limit,
  total,
  onChange,
}: {
  offset: number;
  limit: number;
  total: number;
  onChange: (offset: number) => void;
}): ReactNode {
  if (total <= limit) return null;
  const page = Math.floor(offset / limit) + 1;
  const pages = Math.max(1, Math.ceil(total / limit));

  return (
    <nav
      className="row-between"
      style={{ padding: 'var(--janus-space-3) var(--janus-space-4)' }}
      aria-label={t('pagination.label')}
    >
      <span className="small muted">
        {t('pagination.range', { from: offset + 1, to: Math.min(offset + limit, total), total })}
      </span>
      <div className="row">
        <button type="button" className="btn btn-sm" onClick={() => onChange(Math.max(0, offset - limit))} disabled={page <= 1}>
          {t('pagination.previous')}
        </button>
        <span className="small muted num">{t('pagination.page', { page, pages })}</span>
        <button type="button" className="btn btn-sm" onClick={() => onChange(offset + limit)} disabled={offset + limit >= total}>
          {t('pagination.next')}
        </button>
      </div>
    </nav>
  );
}
