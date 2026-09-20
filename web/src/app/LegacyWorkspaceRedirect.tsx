import { Navigate, useLocation } from 'react-router-dom';

/** Legacy bookmarks keep their query and fragment, including unknown future state. */
export function legacyWorkspaceTarget(pathname: string, search: string, hash: string) {
  let target = pathname;
  if (pathname === '/admin/settings') target = '/admin/settings/general';
  if (pathname === '/admin/provisioning') target = '/admin/settings/sign-in';
  if (pathname === '/admin/team-import' || pathname === '/teams/import') target = '/admin/teams/import';
  if (pathname === '/admin/system') {
    const anchor = hash.slice(1).toLowerCase();
    const sections: Record<string, string> = {
      license: 'license',
      updates: 'license',
      'license-heading': 'license',
      'troubleshooting-title': 'troubleshooting',
      'upstream-timeouts-heading': 'general',
      'discovery-interval-heading': 'general',
      troubleshooting: 'troubleshooting',
      capture: 'troubleshooting',
      'docs-feedback': 'troubleshooting',
      'feature-flags': 'general',
      features: 'general',
      'upstream-timeouts': 'general',
      discovery: 'general',
      'model-discovery': 'general',
      'discovery-interval': 'general',
    };
    target = `/admin/settings/${sections[anchor] ?? 'status'}`;
  }
  const params = new URLSearchParams(search);
  if (pathname.startsWith('/admin/users') && params.get('tab') === 'teams') {
    target = '/admin/teams';
    params.delete('tab');
    params.set('view', 'browse');
    search = `?${params.toString()}`;
  }
  return { pathname: target, search, hash };
}

export function LegacyWorkspaceRedirect() {
  const { pathname, search, hash, state } = useLocation();
  return <Navigate replace to={legacyWorkspaceTarget(pathname, search, hash)} state={state} />;
}
