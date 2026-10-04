/**
 * Row-action icons and the icon button that carries them.
 *
 * Tables with several per-row actions used to render each as a text button,
 * which wrapped into a two-line grid on every row (People had four). An icon
 * button keeps a row one line tall; its label is still the accessible name
 * (aria-label) and appears as a tooltip on hover and keyboard focus, so
 * nothing is lost for screen readers or tests that query by role + name.
 */
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';

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
  | 'check';

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

/** Right-aligned, single-line group of row actions. */
export function RowActions({ children }: { children: ReactNode }): ReactNode {
  return <div className="row-actions">{children}</div>;
}
