import { useRef, type ReactNode } from 'react';
import { Link, type To } from 'react-router-dom';
import './route-top-nav.css';

export interface RouteTopNavItem {
  id: string;
  label: ReactNode;
  to: To;
}

/** Page navigation, not ARIA tabs: native links support keyboard, history and new tabs. */
export function RouteTopNav({ label, items, active }: { label: string; items: readonly RouteTopNavItem[]; active: string }) {
  return (
    <nav className="route-top-nav" aria-label={label}>
      {items.map((item) => (
        <Link key={item.id} to={item.to} aria-current={active === item.id ? 'page' : undefined}>
          {item.label}
        </Link>
      ))}
    </nav>
  );
}

/** Mount on first visit, then retain drafts in memory while sibling routes are visible.
 * No credentials or drafts are serialized to storage. Leaving the workspace unmounts them.
 */
export function RetainedPanel({ active, children }: { active: boolean; children: ReactNode }) {
  const visited = useRef(active);
  if (active) visited.current = true;
  return visited.current ? <div hidden={!active}>{children}</div> : null;
}
