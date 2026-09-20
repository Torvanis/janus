/**
 * LookupInput: a multiselect that searches as you type.
 *
 * Chips show the chosen items by their human label; the value handed back
 * is the list of ids the API actually filters on. Typing "samp" queries the
 * source and lists "Casey Sample" — the admin never has to know a user id
 * or spell a model name exactly. Arrow keys move the highlight, Enter picks,
 * Escape closes, Backspace on an empty box removes the last chip.
 *
 * Sources are async so each field can hit its own endpoint with the typed
 * text; results are debounced and stale responses are dropped.
 */
import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { t } from '../lib/i18n';

export interface LookupOption {
  id: string;
  label: string;
  /** Secondary line — email, provider, alias target. */
  detail?: string;
}

export interface LookupInputProps {
  label: string;
  /** Selected ids, in order. */
  value: string[];
  onChange: (next: string[]) => void;
  /** Called with the typed text (may be empty for "show some"); returns matches. */
  search: (query: string) => Promise<LookupOption[]>;
  /** Resolve ids that arrived from the server to labels for the chips. */
  resolve?: (ids: string[]) => Promise<LookupOption[]>;
  placeholder?: string;
  /** Allow committing text that matched nothing (free-form ids). Default false. */
  allowFreeText?: boolean;
  /** Milliseconds to wait after the last keystroke before searching. */
  debounceMs?: number;
  /** Fired whenever the id→label map grows, so a parent summary can show names. */
  onLabels?: (labels: Record<string, string>) => void;
}

export function LookupInput({
  label,
  value,
  onChange,
  search,
  resolve,
  placeholder,
  allowFreeText = false,
  debounceMs = 150,
  onLabels,
}: LookupInputProps): ReactNode {
  const [draft, setDraft] = useState('');
  const [open, setOpen] = useState(false);
  const [options, setOptions] = useState<LookupOption[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');
  const [highlight, setHighlight] = useState(0);
  const [labels, setLabels] = useState<Record<string, LookupOption>>({});
  const inputRef = useRef<HTMLInputElement>(null);
  const rootRef = useRef<HTMLDivElement>(null);
  const seq = useRef(0);
  const listId = useId();

  // Resolve labels for ids we have not seen (a session loaded from the
  // server carries ids only).
  useEffect(() => {
    const missing = value.filter((id) => !labels[id]);
    if (missing.length === 0 || !resolve) return;
    let cancelled = false;
    resolve(missing)
      .then((found) => {
        if (cancelled) return;
        setLabels((prev) => {
          const next = { ...prev };
          for (const o of found) next[o.id] = o;
          for (const id of missing) if (!next[id]) next[id] = { id, label: id };
          return next;
        });
      })
      .catch(() => {
        if (!cancelled) setError('Unable to resolve selected labels. IDs are retained.');
      });
    return () => {
      cancelled = true;
    };
  }, [value, labels, resolve]);

  // Search as you type.
  useEffect(() => {
    if (!open) return;
    const mine = ++seq.current;
    setLoading(true);
    setError('');
    setOptions([]);
    const handle = window.setTimeout(() => {
      search(draft).then(
        (found) => {
          if (mine !== seq.current) return;
          setOptions(found.filter((o) => !value.includes(o.id)));
          setHighlight(0);
          setLoading(false);
        },
        () => {
          if (mine !== seq.current) return;
          setOptions([]);
          setError('Search failed. Try again.');
          setLoading(false);
        },
      );
    }, debounceMs);
    return () => {
      window.clearTimeout(handle);
      seq.current++;
    };
  }, [draft, open, search, value, debounceMs]);

  // Close when focus leaves the control entirely.
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (!rootRef.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('mousedown', onDoc);
    return () => document.removeEventListener('mousedown', onDoc);
  }, [open]);

  const pick = (o: LookupOption) => {
    setLabels((prev) => ({ ...prev, [o.id]: o }));
    onChange([...value, o.id]);
    setDraft('');
    // One pick closes the menu; typing again reopens it. Leaving it open
    // after a pick reads as "did that take?" and covers the rows below.
    setOpen(false);
    inputRef.current?.focus();
  };
  const remove = (id: string) => onChange(value.filter((v) => v !== id));

  const chips = useMemo(() => value.map((id) => labels[id] ?? { id, label: id }), [value, labels]);
  useEffect(() => {
    if (!onLabels) return;
    const flat: Record<string, string> = {};
    for (const [id, o] of Object.entries(labels)) flat[id] = o.label;
    onLabels(flat);
  }, [labels, onLabels]);
  const showFree = allowFreeText && draft.trim() !== '' && !options.some((o) => o.id === draft.trim());

  return (
    <div ref={rootRef} className="lookup" onClick={() => inputRef.current?.focus()}>
      <div className="rule-list lookup-box">
        {chips.map((c) => (
          <span key={c.id} className="rule-chip lookup-chip" title={c.detail ? `${c.label} · ${c.detail}` : c.label}>
            <span>{c.label}</span>
            <button
              type="button"
              className="rule-chip-remove"
              onClick={(e) => {
                e.stopPropagation();
                remove(c.id);
              }}
              aria-label={t('lookup.remove', { value: c.label })}
            >
              ×
            </button>
          </span>
        ))}
        <input
          ref={inputRef}
          className="rule-list-input"
          role="combobox"
          aria-label={label}
          aria-expanded={open}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={open && options[highlight] ? `${listId}-${highlight}` : undefined}
          autoComplete="off"
          value={draft}
          placeholder={value.length === 0 ? (placeholder ?? t('lookup.placeholder')) : t('lookup.addMore')}
          onClick={() => setOpen(true)}
          onBlur={() => {
            if (allowFreeText && draft.trim() && !value.includes(draft.trim())) pick({ id: draft.trim(), label: draft.trim() });
          }}
          onChange={(e) => {
            const v = e.target.value;
            // Free-text sources accept a pasted "a, b, c": each becomes a
            // chip at once, so a list copied from a log lands in one go.
            if (allowFreeText && v.includes(',')) {
              const parts = v
                .split(',')
                .map((p) => p.trim())
                .filter((p) => p && !value.includes(p));
              if (parts.length > 0) {
                setLabels((prev) => {
                  const next = { ...prev };
                  for (const p of parts) next[p] = { id: p, label: p };
                  return next;
                });
                onChange([...value, ...parts]);
              }
              setDraft('');
              return;
            }
            setDraft(v);
            setOpen(true);
          }}
          onKeyDown={(e) => {
            const total = options.length + (showFree ? 1 : 0);
            if (e.key === 'ArrowDown') {
              e.preventDefault();
              setOpen(true);
              setHighlight((h) => (total === 0 ? 0 : (h + 1) % total));
            } else if (e.key === 'ArrowUp') {
              e.preventDefault();
              setHighlight((h) => (total === 0 ? 0 : (h - 1 + total) % total));
            } else if (e.key === 'Enter') {
              e.preventDefault();
              if (open && highlight < options.length && options[highlight]) pick(options[highlight]);
              else if (allowFreeText && draft.trim() && !value.includes(draft.trim()))
                pick({ id: draft.trim(), label: draft.trim() });
            } else if (e.key === 'Escape') {
              setOpen(false);
            } else if (e.key === 'Backspace' && draft === '' && value.length > 0) {
              remove(value[value.length - 1]!);
            }
          }}
        />
      </div>
      {error ? (
        <p role="alert" className="small">
          {error}
        </p>
      ) : null}
      {open ? (
        <ul id={listId} role="listbox" className="lookup-menu" aria-label={label}>
          {options.map((o, i) => (
            <li
              key={o.id}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === highlight}
              className={'lookup-option' + (i === highlight ? ' is-active' : '')}
              onMouseEnter={() => setHighlight(i)}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(o);
              }}
            >
              <span className="lookup-option-label">{o.label}</span>
              {o.detail ? <span className="lookup-option-detail">{o.detail}</span> : null}
            </li>
          ))}
          {showFree ? (
            <li
              id={`${listId}-${options.length}`}
              role="option"
              aria-selected={highlight === options.length}
              className={'lookup-option lookup-option-free' + (highlight === options.length ? ' is-active' : '')}
              onMouseEnter={() => setHighlight(options.length)}
              onMouseDown={(e) => {
                e.preventDefault();
                pick({ id: draft.trim(), label: draft.trim() });
              }}
            >
              <span className="lookup-option-label">{t('lookup.useExact', { value: draft.trim() })}</span>
            </li>
          ) : null}
          {!loading && !error && options.length === 0 && !showFree ? (
            <li className="lookup-empty" aria-disabled="true">
              {draft.trim() ? t('lookup.noMatches', { value: draft.trim() }) : t('lookup.typeToSearch')}
            </li>
          ) : null}
          {!loading && options.length > 0 ? (
            <li className="lookup-empty">Showing suggested matches. Refine your search for more.</li>
          ) : null}
          {loading && options.length === 0 ? (
            <li className="lookup-empty" aria-disabled="true">
              {t('lookup.searching')}
            </li>
          ) : null}
        </ul>
      ) : null}
    </div>
  );
}
