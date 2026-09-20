import { useState, type ReactNode } from 'react';
import { ConfirmDialog } from './ui';
/** Keep destructive confirmations accessible and scoped to their row. */
export function ConfirmAction({
  children,
  consequence,
  onConfirm,
  busy,
  danger = true,
}: {
  children: string;
  consequence: string;
  onConfirm: () => void;
  busy?: boolean;
  danger?: boolean;
}): ReactNode {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" className={`btn ${danger ? 'btn-danger' : ''}`} disabled={busy} onClick={() => setOpen(true)}>
        {children}
      </button>
      <ConfirmDialog
        open={open}
        onClose={() => setOpen(false)}
        title={children}
        consequence={consequence}
        danger={danger}
        busy={busy}
        confirmLabel="Confirm"
        onConfirm={() => {
          onConfirm();
          setOpen(false);
        }}
      />
    </>
  );
}
