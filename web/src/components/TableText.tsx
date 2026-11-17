/**
 * Text pieces for dense tables and lists.
 *
 * Every shortened piece of text carries its full value in a `title`, so a cut
 * name, description or URL can always be read by hovering it. Use these
 * instead of a bare `.truncate` / line-clamp class.
 */
import type { ElementType, ReactNode } from 'react';
import { formatDateTime } from '../lib/format';
import { t } from '../lib/i18n';

/**
 * One line with an ellipsis (`lines` = 1, default) or a clamp to N lines. The
 * full text is the tooltip; pass `title` when the children are not plain text.
 */
export function Truncate({
  children,
  title,
  lines = 1,
  as: Tag = 'span',
  className,
  maxWidth,
}: {
  children: ReactNode;
  title?: string;
  lines?: 1 | 2 | 3;
  as?: ElementType;
  className?: string;
  maxWidth?: number | string;
}): ReactNode {
  const full = title ?? (typeof children === 'string' || typeof children === 'number' ? String(children) : undefined);
  const cls = [lines === 1 ? 'truncate' : `clamp clamp-${lines}`, className].filter(Boolean).join(' ');
  return (
    <Tag className={cls} title={full || undefined} style={maxWidth !== undefined ? { maxWidth } : undefined}>
      {children}
    </Tag>
  );
}

/**
 * "<top model> +N more": the name gets the ellipsis, the count never does,
 * and the tooltip says both.
 */
export function TopModel({ name, count }: { name: string | undefined; count: number | undefined }): ReactNode {
  if (!name) return <span className="muted">—</span>;
  const more = Math.max(0, (count ?? 0) - 1);
  return (
    <span className="cell-with-more" title={more > 0 ? t('tables.topModelMore', { model: name, count: more }) : name}>
      <span className="truncate">{name}</span>
      {more > 0 ? <span className="muted nowrap">{t('tables.plusMore', { count: more })}</span> : null}
    </span>
  );
}

/**
 * A table header label that wraps only between words. A hyphenated word
 * ("sign-in") stays whole and the sort arrow stays on the last word's line,
 * so a narrow column never shows "LAST SIGN-" over "IN".
 */
export function HeaderLabel({ label, after }: { label: string; after?: ReactNode }): ReactNode {
  const words = label.split(' ').filter(Boolean);
  return (
    <>
      {words.map((word, i) => (
        <span key={i} className="th-word">
          {word}
          {i === words.length - 1 ? after : ' '}
        </span>
      ))}
    </>
  );
}

/** Comma list of names (router or model ids) that wraps between names, never inside one. */
export function UsedBy({ names }: { names: readonly string[] }): ReactNode {
  return (
    <>
      {names.map((name, i) => (
        <span key={`${name}-${i}`}>
          {i > 0 ? ' ' : null}
          <span className="nowrap">
            {name}
            {i < names.length - 1 ? ',' : ''}
          </span>
        </span>
      ))}
    </>
  );
}

const sameDay = (a: Date, b: Date): boolean =>
  a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();

/**
 * Short, one-line request time for log tables: "3:42 PM" today, "Nov 15, 3:42 PM"
 * this year, "Nov 15, 2025" before that. The full date and time is the tooltip.
 */
export function formatTableTime(value: string | undefined, now: Date = new Date()): string {
  if (!value || value.startsWith('0001-01-01')) return '—';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return '—';
  if (sameDay(date, now)) return new Intl.DateTimeFormat(undefined, { timeStyle: 'short' }).format(date);
  if (date.getFullYear() === now.getFullYear()) {
    return new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' }).format(
      date,
    );
  }
  return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium' }).format(date);
}

/** The time cell of a request table: one line, never wrapped. */
export function TimeCell({ value, className = 'small muted' }: { value: string | undefined; className?: string }): ReactNode {
  return (
    <td className={`${className} cell-time`} title={formatDateTime(value)}>
      {value ? <time dateTime={value}>{formatTableTime(value)}</time> : '—'}
    </td>
  );
}
