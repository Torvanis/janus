import { useLayoutEffect, type RefObject } from 'react';

/** Space kept between a popover and the window edge. */
const EDGE = 12;

/**
 * Keeps an absolutely positioned popover (a column picker, a lookup list)
 * inside the window: it caps the height to the room below its anchor and
 * scrolls inside, and it shifts sideways when it would hang past the left or
 * right edge. Runs when the popover opens and again on resize.
 */
export function useFitToViewport(ref: RefObject<HTMLElement | null>, open: boolean): void {
  useLayoutEffect(() => {
    const el = ref.current;
    if (!open || !el) return;
    const fit = () => {
      el.style.maxHeight = '';
      el.style.translate = '';
      const r = el.getBoundingClientRect();
      const below = window.innerHeight - r.top - EDGE;
      if (r.height > below) {
        el.style.maxHeight = `${Math.max(160, Math.floor(below))}px`;
        el.style.overflowY = 'auto';
      }
      const vw = document.documentElement.clientWidth || window.innerWidth;
      let dx = 0;
      if (r.right > vw - EDGE) dx = vw - EDGE - r.right;
      if (r.left + dx < EDGE) dx = EDGE - r.left;
      if (dx !== 0) el.style.translate = `${Math.round(dx)}px 0`;
    };
    fit();
    window.addEventListener('resize', fit);
    return () => window.removeEventListener('resize', fit);
  }, [ref, open]);
}
