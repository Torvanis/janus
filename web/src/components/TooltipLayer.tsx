/**
 * Hover/focus tooltips for icon buttons (`.icon-btn[data-tooltip]`).
 *
 * The tooltip used to be a CSS `::after` on the button. Row actions live in a
 * sticky table cell, and a sticky cell is its own stacking context, so the
 * tooltip of a first-row button (drawn above the button) sat UNDER the sticky
 * table header and was cut off or hidden entirely. No z-index on the pseudo
 * element can escape that context.
 *
 * One layer, mounted once at the app root, listens for hover and keyboard
 * focus on any icon button and draws the tooltip in a portal on <body> with
 * fixed positioning above every sticky header, drawer and modal. It prefers
 * the spot above the button and flips below when there is no room at the top
 * of the viewport, and it never leaves the viewport sideways.
 */
import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';

const SELECTOR = '.icon-btn[data-tooltip]';
/** Space between the button and the tooltip. */
export const TOOLTIP_GAP = 6;
/** Minimum distance kept from the viewport edges. */
export const TOOLTIP_MARGIN = 4;

type Box = { top: number; left: number; right: number; bottom: number };
export type TooltipPlacement = { top: number; left: number; placement: 'top' | 'bottom' };

/**
 * Where to draw a tooltip of `size` for an anchor at `anchor` (viewport
 * coordinates): above, right-aligned with the anchor, unless that would leave
 * the viewport at the top, in which case below. Horizontally clamped inside
 * the viewport.
 */
export function placeTooltip(
  anchor: Box,
  size: { width: number; height: number },
  viewport: { width: number; height: number },
): TooltipPlacement {
  const above = anchor.top - TOOLTIP_GAP - size.height;
  const below = anchor.bottom + TOOLTIP_GAP;
  const fitsAbove = above >= TOOLTIP_MARGIN;
  const fitsBelow = below + size.height <= viewport.height - TOOLTIP_MARGIN;
  const placement = fitsAbove || !fitsBelow ? 'top' : 'bottom';
  const maxLeft = Math.max(TOOLTIP_MARGIN, viewport.width - TOOLTIP_MARGIN - size.width);
  const left = Math.min(Math.max(anchor.right - size.width, TOOLTIP_MARGIN), maxLeft);
  return { top: placement === 'top' ? Math.max(above, TOOLTIP_MARGIN) : below, left, placement };
}

function tooltipAnchor(target: EventTarget | null): HTMLElement | null {
  const anchor = target instanceof Element ? (target.closest(SELECTOR) as HTMLElement | null) : null;
  // Row-action menu items already print their label next to the icon.
  return anchor?.closest('.row-actions-menu') ? null : anchor;
}

function focusVisible(el: Element): boolean {
  try {
    return el.matches(':focus-visible');
  } catch {
    return false;
  }
}

export function TooltipLayer(): ReactNode {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const [text, setText] = useState('');
  const [pos, setPos] = useState<TooltipPlacement | null>(null);
  const tipRef = useRef<HTMLDivElement>(null);
  const hovered = useRef<HTMLElement | null>(null);
  const focused = useRef<HTMLElement | null>(null);

  const sync = useCallback(() => {
    const next = hovered.current ?? focused.current;
    setAnchor(next);
    setText(next?.getAttribute('data-tooltip') ?? '');
    if (!next) setPos(null);
  }, []);

  useEffect(() => {
    const over = (e: PointerEvent) => {
      const el = tooltipAnchor(e.target);
      if (el === hovered.current) return;
      hovered.current = el;
      sync();
    };
    const out = (e: PointerEvent) => {
      const el = tooltipAnchor(e.target);
      if (!el || el !== hovered.current) return;
      if (el.contains(e.relatedTarget as Node | null)) return;
      hovered.current = null;
      sync();
    };
    const focusIn = (e: FocusEvent) => {
      const el = tooltipAnchor(e.target);
      focused.current = el && focusVisible(el) ? el : null;
      sync();
    };
    const focusOut = () => {
      focused.current = null;
      sync();
    };
    const key = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      hovered.current = null;
      focused.current = null;
      sync();
    };
    document.addEventListener('pointerover', over, true);
    document.addEventListener('pointerout', out, true);
    document.addEventListener('focusin', focusIn, true);
    document.addEventListener('focusout', focusOut, true);
    document.addEventListener('keydown', key, true);
    return () => {
      document.removeEventListener('pointerover', over, true);
      document.removeEventListener('pointerout', out, true);
      document.removeEventListener('focusin', focusIn, true);
      document.removeEventListener('focusout', focusOut, true);
      document.removeEventListener('keydown', key, true);
    };
  }, [sync]);

  // The label can change while shown ("Copy" -> "Copied"), and the button can
  // leave the DOM (row removed, drawer opened); follow both.
  useEffect(() => {
    if (!anchor) return;
    const mo = new MutationObserver(() => {
      if (!anchor.isConnected) {
        if (hovered.current === anchor) hovered.current = null;
        if (focused.current === anchor) focused.current = null;
      }
      sync();
    });
    mo.observe(anchor, { attributes: true, attributeFilter: ['data-tooltip'] });
    mo.observe(document.body, { childList: true, subtree: true });
    return () => mo.disconnect();
  }, [anchor, sync]);

  const place = useCallback(() => {
    const tip = tipRef.current;
    if (!anchor || !tip) return;
    const r = anchor.getBoundingClientRect();
    const t = tip.getBoundingClientRect();
    setPos(placeTooltip(r, { width: t.width, height: t.height }, { width: window.innerWidth, height: window.innerHeight }));
  }, [anchor]);

  useLayoutEffect(() => {
    place();
  }, [place, text]);

  useEffect(() => {
    if (!anchor) return;
    window.addEventListener('scroll', place, true);
    window.addEventListener('resize', place);
    return () => {
      window.removeEventListener('scroll', place, true);
      window.removeEventListener('resize', place);
    };
  }, [anchor, place]);

  if (!anchor || !text) return null;
  return createPortal(
    <div
      ref={tipRef}
      className="janus-tooltip"
      aria-hidden="true"
      data-placement={pos?.placement ?? 'top'}
      style={pos ? { top: pos.top, left: pos.left } : { top: 0, left: 0, visibility: 'hidden' }}
    >
      {text}
    </div>,
    document.body,
  );
}
