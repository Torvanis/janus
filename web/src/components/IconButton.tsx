/**
 * Row-action icons and the icon button that carries them.
 *
 * Tables with several per-row actions used to render each as a text button,
 * which wrapped into a two-line grid on every row (People had four). An icon
 * button keeps a row one line tall; its label is still the accessible name
 * (aria-label) and appears as a tooltip on hover and keyboard focus, so
 * nothing is lost for screen readers or tests that query by role + name.
 */
import { Children, useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { t } from '../lib/i18n';

type IconName =
  | 'edit'
  | 'delete'
  | 'disable'
  | 'enable'
  | 'key'
  | 'shield'
  | 'revoke'
  | 'rates'
  | 'rotate'
  | 'send'
  | 'more'
  | 'copy'
  | 'check'
  | 'up'
  | 'down';

const PATHS: Record<IconName, ReactNode> = {
  edit: <path d="M4 20h4L19 9l-4-4L4 16v4zM14 6l4 4" />,
  delete: (
    <>
      <path d="M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13" />
      <path d="M10 11v6M14 11v6" />
    </>
  ),
  disable: (
    <>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M6 6l12 12" />
    </>
  ),
  enable: (
    <>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M8 12.5l2.6 2.6L16 9.5" />
    </>
  ),
  key: (
    <>
      <circle cx="8" cy="15" r="4" />
      <path d="M11 12l8-8M16 7l3 3M14 9l2 2" />
    </>
  ),
  shield: (
    <>
      <path d="M12 3l7 3v5c0 5-3 8.5-7 10-4-1.5-7-5-7-10V6l7-3z" />
      <path d="M9 12h6" />
    </>
  ),
  revoke: (
    <>
      <circle cx="12" cy="12" r="8.5" />
      <path d="M8 12h8" />
    </>
  ),
  rates: <path d="M12 3v18M16 7.5c0-1.9-1.8-3-4-3s-4 1.1-4 3 1.8 2.6 4 3 4 1.1 4 3-1.8 3-4 3-4-1.1-4-3" />,
  rotate: (
    <>
      <path d="M4 12a8 8 0 0 1 13.7-5.7L20 8.5" />
      <path d="M20 4v4.5h-4.5" />
      <path d="M20 12a8 8 0 0 1-13.7 5.7L4 15.5" />
      <path d="M4 20v-4.5h4.5" />
    </>
  ),
  send: <path d="M4 12l16-8-6 16-2.5-6.5L4 12zM11.5 13.5L20 4" />,
  copy: (
    <>
      <rect x="8.5" y="8.5" width="11" height="11" rx="2" />
      <path d="M15.5 8.5V6.5a2 2 0 0 0-2-2h-7a2 2 0 0 0-2 2v7a2 2 0 0 0 2 2h2" />
    </>
  ),
  check: <path d="M5 12.5l4.5 4.5L19 7.5" />,
  more: (
    <>
      <circle cx="6" cy="12" r="1.2" />
      <circle cx="12" cy="12" r="1.2" />
      <circle cx="18" cy="12" r="1.2" />
    </>
  ),
  up: <path d="M12 19V5M6 11l6-6 6 6" />,
  down: <path d="M12 5v14M6 13l6 6 6-6" />,
};

export function Icon({ name, size = 16 }: { name: IconName; size?: number }): ReactNode {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.8}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      {PATHS[name]}
    </svg>
  );
}

/**
 * A compact icon-only button. `label` is required: it is the accessible name
 * and the hover/focus tooltip. `danger` tints the hover state for destructive
 * actions so delete never looks like edit.
 */
export function IconButton({
  icon,
  label,
  onClick,
  disabled,
  danger,
  title,
}: {
  icon: IconName;
  label: string;
  onClick: () => void;
  disabled?: boolean;
  danger?: boolean;
  /** Tooltip override when the label alone is not enough (e.g. why it is disabled). */
  title?: string;
}): ReactNode {
  return (
    <button
      type="button"
      className={`icon-btn${danger ? ' icon-btn-danger' : ''}`}
      aria-label={label}
      data-tooltip={title ?? label}
      onClick={onClick}
      disabled={disabled}
    >
      <Icon name={icon} />
    </button>
  );
}

/**
 * The standard copy icon. It turns into a check mark for a moment after a
 * copy; the tooltip and accessible name say what gets copied.
 */
export function CopyIconButton({ value, label, copiedLabel }: { value: string; label: string; copiedLabel: string }): ReactNode {
  const [copied, setCopied] = useState(false);
  const timer = useRef<number | undefined>(undefined);
  useEffect(() => () => window.clearTimeout(timer.current), []);
  const copy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(value);
    } catch {
      // Clipboard access can be blocked; fall back to a selection copy.
      const area = document.createElement('textarea');
      area.value = value;
      document.body.appendChild(area);
      area.select();
      document.execCommand('copy');
      document.body.removeChild(area);
    }
    setCopied(true);
    window.clearTimeout(timer.current);
    timer.current = window.setTimeout(() => setCopied(false), 1500);
  }, [value]);
  return (
    <button
      type="button"
      className="icon-btn icon-btn-sm"
      aria-label={copied ? copiedLabel : label}
      data-tooltip={copied ? copiedLabel : label}
      onClick={() => void copy()}
    >
      <Icon name={copied ? 'check' : 'copy'} size={14} />
    </button>
  );
}

/**
 * Right-aligned, single-line group of row actions, pinned to the right edge of
 * the table's scroll area. On phones (below 768px) two or more actions fold
 * into one "More actions" button whose menu lists them with their labels, so
 * the pinned column is one button wide instead of covering a third of the row.
 */
export function RowActions({ children }: { children: ReactNode }): ReactNode {
  const count = Children.toArray(children).filter(Boolean).length;
  const [menu, setMenu] = useState<{ top?: number; bottom?: number; right: number } | null>(null);
  const toggle = useRef<HTMLButtonElement>(null);
  const pop = useRef<HTMLDivElement>(null);
  const close = useCallback((refocus: boolean) => {
    setMenu(null);
    if (refocus) toggle.current?.focus();
  }, []);
  useEffect(() => {
    if (!menu) return;
    pop.current?.querySelector<HTMLElement>('button:not(:disabled), a[href]')?.focus();
    const onDown = (e: PointerEvent) => {
      if (!pop.current?.contains(e.target as Node) && !toggle.current?.contains(e.target as Node)) close(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close(true);
    };
    // Close when the row moves under the menu (page or table scroll), not on
    // a stray scroll event that leaves the button where it was.
    const at = toggle.current?.getBoundingClientRect();
    const onMove = () => {
      const now = toggle.current?.getBoundingClientRect();
      if (!at || !now || Math.abs(now.top - at.top) > 2 || Math.abs(now.left - at.left) > 2) close(false);
    };
    document.addEventListener('pointerdown', onDown);
    document.addEventListener('keydown', onKey);
    window.addEventListener('resize', onMove);
    window.addEventListener('scroll', onMove, true);
    return () => {
      document.removeEventListener('pointerdown', onDown);
      document.removeEventListener('keydown', onKey);
      window.removeEventListener('resize', onMove);
      window.removeEventListener('scroll', onMove, true);
    };
  }, [menu, close]);
  if (count < 2) return <div className="row-actions">{children}</div>;
  return (
    <div className="row-actions">
      <span className="row-actions-inline">{children}</span>
      <button
        ref={toggle}
        type="button"
        className="icon-btn row-actions-more"
        aria-label={t('tables.moreActions')}
        data-tooltip={t('tables.moreActions')}
        aria-haspopup="true"
        aria-expanded={menu !== null}
        onClick={() => {
          if (menu) return close(false);
          const r = toggle.current!.getBoundingClientRect();
          const right = Math.max(4, window.innerWidth - r.right);
          // Open upward when the row sits in the bottom third of the window.
          setMenu(
            r.bottom > window.innerHeight * 0.66
              ? { bottom: window.innerHeight - r.top + 4, right }
              : { top: r.bottom + 4, right },
          );
        }}
      >
        <Icon name="more" />
      </button>
      {menu
        ? createPortal(
            <div
              ref={pop}
              className="row-actions-menu"
              role="group"
              aria-label={t('tables.actions')}
              style={menu}
              // An action opens its own dialog or runs; either way the menu is done.
              onClick={() => close(false)}
            >
              {children}
            </div>,
            document.body,
          )
        : null}
    </div>
  );
}
