import type { ReactNode } from 'react';
import { useLocalOnly } from '../../app/session';
import { RouteTopNav } from '../../components/RouteTopNav';

export const modelsTabs = [
  { id: 'catalog', label: 'Models', path: '/admin/models' },
  { id: 'pricing-accuracy', label: 'Pricing accuracy', path: '/admin/models/pricing-accuracy' },
] as const;

/** The Models section's tabs. Hidden in local-only mode, which has no prices. */
export function ModelsTabs({ active }: { active: string }): ReactNode {
  const localOnly = useLocalOnly();
  if (localOnly) return null;
  return (
    <RouteTopNav
      label="Models"
      active={active}
      items={modelsTabs.map((tab) => ({ id: tab.id, label: tab.label, to: tab.path }))}
    />
  );
}
