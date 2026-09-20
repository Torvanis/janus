import { useEffect, useRef, useState, type ReactNode } from 'react';
import { NavLink, useLocation, useNavigate } from 'react-router-dom';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Breadcrumb } from './Breadcrumb';
import { api } from '../lib/api';
import { resolveTheme, useAppearance, useMediaQuery } from '../lib/hooks';
import { useSession } from './session';
import { AmbientCanvas, type AmbientSignal } from '../components/AmbientCanvas';
import { Badge } from '../components/ui';
import { PageErrorBoundary } from '../components/PageErrorBoundary';
import { LicenseBanner } from './LicenseBanner';
import type { GlobalDashboard, Notification } from '../lib/types';
import { t } from '../lib/i18n';
import './shell.css';
import { ActiveTeamSelector } from './ActiveTeamSelector';

interface NavItem {
  to: string;
  label: () => string;
  glyph: string;
  admin?: boolean;
}

// Labels are resolved lazily through t() so nav copy stays in the i18n layer.
const PRIMARY_NAV: NavItem[] = [
  { to: '/dashboard', label: () => t('shell.navDashboard'), glyph: '◈' },
  { to: '/models', label: () => t('shell.navModels'), glyph: '◇' },
  { to: '/tokens', label: () => t('shell.navTokens'), glyph: '⚿' },
  { to: '/requests', label: () => t('shell.navRequests'), glyph: '≡' },
  { to: '/reports', label: () => 'Reports', glyph: '▥' },
  { to: '/quota', label: () => t('shell.navQuotas'), glyph: '◐' },
  { to: '/teams', label: () => t('shell.navTeam'), glyph: '⧉' },
  { to: '/explore', label: () => t('shell.navOrgPulse'), glyph: '✦' },
  { to: '/help', label: () => t('shell.navHelp'), glyph: '?' },
];

const ADMIN_NAV: NavItem[] = [
  { to: '/admin', label: () => t('shell.navOverview'), glyph: '▤', admin: true },
  { to: '/admin/upstreams', label: () => t('shell.navUpstreams'), glyph: '⇅', admin: true },
  { to: '/admin/models', label: () => t('shell.navModels'), glyph: '◈', admin: true },
  { to: '/admin/managed-models', label: () => t('shell.navManagedModels'), glyph: '⧫', admin: true },
  { to: '/admin/grants', label: () => t('shell.navGrants'), glyph: '⚑', admin: true },
  { to: '/admin/service-tokens', label: () => t('shell.navServiceTokens'), glyph: '⚙', admin: true },
  { to: '/admin/users', label: () => t('shell.navPeople'), glyph: '⚭', admin: true },
  { to: '/admin/quotas', label: () => t('shell.navQuotas'), glyph: '◑', admin: true },
  { to: '/admin/rules', label: () => t('shell.navPolicyRules'), glyph: '⛨', admin: true },
  { to: '/admin/security', label: () => t('shell.navSecurity'), glyph: '⛉', admin: true },
  { to: '/admin/alerts', label: () => t('shell.navAlerts'), glyph: '◉', admin: true },
  { to: '/admin/requests', label: () => t('shell.navRequests'), glyph: '≡', admin: true },
  { to: '/admin/audit', label: () => t('shell.navAuditLog'), glyph: '❑', admin: true },
  { to: '/admin/settings', label: () => t('shell.navSettings'), glyph: '⚙', admin: true },
];

const MOBILE_NAV = PRIMARY_NAV.slice(0, 4);

export function Shell({ children }: { children: ReactNode }): ReactNode {
  const { me, config, refresh } = useSession();
  const appearance = useAppearance();
  const location = useLocation();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const isMobile = useMediaQuery('(max-width: 767px)');
  const isRail = useMediaQuery('(min-width: 768px) and (max-width: 1023px)');
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const menuRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setDrawerOpen(false);
    setMenuOpen(false);
  }, [location.pathname]);

  // The avatar menu must dismiss like any popover: outside click or Escape.
  useEffect(() => {
    if (!menuOpen) return;
    const onPointerDown = (event: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(event.target as Node)) {
        setMenuOpen(false);
      }
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setMenuOpen(false);
    };
    document.addEventListener('mousedown', onPointerDown);
    document.addEventListener('keydown', onKeyDown);
    return () => {
      document.removeEventListener('mousedown', onPointerDown);
      document.removeEventListener('keydown', onKeyDown);
    };
  }, [menuOpen]);

  // Live signal for the ambient layer and the throughput readout.
  const pulse = useQuery({
    queryKey: ['global-pulse'],
    queryFn: () => api.get<GlobalDashboard>('/api/v1/dashboard/global'),
    refetchInterval: 10_000,
    enabled: Boolean(me),
  });

  const notifications = useQuery({
    queryKey: ['notifications', 'unread'],
    queryFn: () => api.get<{ notifications: Notification[]; unread_count: number }>('/api/v1/notifications?limit=8'),
    refetchInterval: 30_000,
    enabled: Boolean(me),
  });

  const signal = deriveSignal(pulse.data);
  const unread = notifications.data?.unread_count ?? 0;

  const signOut = async () => {
    try {
      await api.post('/auth/logout');
    } finally {
      queryClient.clear();
      navigate('/auth/signed-out?reason=manual', { replace: true });
    }
  };

  const showAdmin = me?.role === 'admin';

  return (
    <div className="shell" data-rail={isRail ? 'true' : 'false'}>
      <AmbientCanvas signal={signal} reducedMotion={appearance.reducedMotion} />
      <a className="skip-link" href="#main">
        {t('shell.skipToContent')}
      </a>

      {!isMobile ? (
        <nav className="sidebar" aria-label={t('shell.primaryNav')}>
          <Brand collapsed={isRail} />
          <div className="nav-group">
            {PRIMARY_NAV.map((item) => (
              <NavItemLink key={item.to} item={item} collapsed={isRail} />
            ))}
          </div>
          {showAdmin ? (
            <>
              <div className="nav-heading">{isRail ? '···' : t('shell.administration')}</div>
              <div className="nav-group">
                {ADMIN_NAV.map((item) => (
                  <NavItemLink key={item.to} item={item} collapsed={isRail} end={item.to === '/admin'} />
                ))}
              </div>
            </>
          ) : null}
          <div className="sidebar-footer">
            <ThroughputReadout signal={signal} collapsed={isRail} />
          </div>
        </nav>
      ) : null}

      <div className="shell-main">
        <header className="topbar">
          {isMobile ? (
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              onClick={() => setDrawerOpen(true)}
              aria-label={t('shell.openMenu')}
              aria-expanded={drawerOpen}
            >
              ☰
            </button>
          ) : null}
          {isMobile ? <Brand collapsed={false} compact /> : <Breadcrumb path={location.pathname} />}
          {me && (me.teams.length > 0 || me.active_team_id) ? (
            <ActiveTeamSelector teams={me.teams} activeTeam={me.active_team_id ?? ''} refresh={refresh} />
          ) : null}

          <div className="row" style={{ marginLeft: 'auto', gap: 'var(--janus-space-1)' }}>
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              // Toggle from the RESOLVED theme: a 'system' user whose OS is
              // already light must land on dark, never on a visual no-op.
              onClick={() => appearance.setTheme(resolveTheme(appearance.theme) === 'light' ? 'dark' : 'light')}
              aria-label={resolveTheme(appearance.theme) === 'light' ? t('shell.switchToDark') : t('shell.switchToLight')}
              title={t('shell.toggleTheme')}
            >
              {resolveTheme(appearance.theme) === 'light' ? '☀' : '☾'}
            </button>
            <button
              type="button"
              className="btn btn-ghost btn-sm"
              onClick={() => appearance.setMotion(appearance.reducedMotion ? 'full' : 'reduced')}
              aria-pressed={appearance.reducedMotion}
              title={appearance.reducedMotion ? t('shell.motionReducedTitle') : t('shell.motionOnTitle')}
            >
              {appearance.reducedMotion ? '◼' : '◈'}
              <span className="sr-only">
                {appearance.reducedMotion ? t('shell.reducedMotionEnabled') : t('shell.fullMotionEnabled')}
              </span>
            </button>
            <NavLink
              to="/notifications"
              className="btn btn-ghost btn-sm"
              aria-label={t('shell.notificationsUnread', { count: unread })}
            >
              ◔
              {unread > 0 ? (
                <span className="badge badge-danger" style={{ padding: '0 6px' }}>
                  {unread > 99 ? '99+' : unread}
                </span>
              ) : null}
            </NavLink>
            <div className="avatar-menu" ref={menuRef}>
              <button
                type="button"
                className="btn btn-ghost btn-sm"
                onClick={() => setMenuOpen((open) => !open)}
                aria-expanded={menuOpen}
                aria-haspopup="menu"
              >
                <span className="avatar" aria-hidden="true">
                  {(me?.name || me?.email || '?').slice(0, 1).toUpperCase()}
                </span>
                <span className="hide-sm">{me?.name || me?.email}</span>
              </button>
              {menuOpen ? (
                <div className="menu" role="menu">
                  <div className="menu-header">
                    <div className="truncate" style={{ fontWeight: 600 }}>
                      {me?.name}
                    </div>
                    <div className="small muted truncate">{me?.email}</div>
                    <div style={{ marginTop: 6 }}>
                      <Badge tone={me?.role === 'admin' ? 'primary' : 'neutral'}>{me?.role ?? 'user'}</Badge>
                    </div>
                  </div>
                  <NavLink to="/settings" className="menu-item" role="menuitem">
                    {t('shell.navPersonalSettings')}
                  </NavLink>
                  <NavLink to="/docs" className="menu-item" role="menuitem">
                    {t('shell.navDocumentation')}
                  </NavLink>
                  <button type="button" className="menu-item" role="menuitem" onClick={signOut}>
                    {t('shell.signOut')}
                  </button>
                </div>
              ) : null}
            </div>
          </div>
        </header>

        <main id="main" className="shell-content">
          <LicenseBanner />
          <PageErrorBoundary>{children}</PageErrorBoundary>
        </main>

        {isMobile ? (
          <nav className="bottom-nav" aria-label={t('shell.primaryNav')}>
            {MOBILE_NAV.map((item) => (
              <NavLink key={item.to} to={item.to} className="bottom-nav-item">
                <span aria-hidden="true">{item.glyph}</span>
                <span>{item.label()}</span>
              </NavLink>
            ))}
            <button type="button" className="bottom-nav-item" onClick={() => setDrawerOpen(true)}>
              <span aria-hidden="true">···</span>
              <span>{t('shell.more')}</span>
            </button>
          </nav>
        ) : null}
      </div>

      {drawerOpen ? (
        <>
          <div className="scrim" onClick={() => setDrawerOpen(false)} aria-hidden="true" />
          <nav className="mobile-drawer" aria-label={t('shell.allSections')}>
            <div className="row-between" style={{ marginBottom: 'var(--janus-space-4)' }}>
              <Brand collapsed={false} />
              <button
                type="button"
                className="btn btn-ghost btn-sm"
                onClick={() => setDrawerOpen(false)}
                aria-label={t('shell.closeMenu')}
              >
                ✕
              </button>
            </div>
            <div className="nav-group">
              {PRIMARY_NAV.map((item) => (
                <NavItemLink key={item.to} item={item} collapsed={false} />
              ))}
              <NavItemLink
                item={{ to: '/settings', label: () => t('shell.navPersonalSettings'), glyph: '⚙' }}
                collapsed={false}
              />
              <NavItemLink item={{ to: '/docs', label: () => t('shell.navDocumentation'), glyph: '❐' }} collapsed={false} />
            </div>
            {showAdmin ? (
              <>
                <div className="nav-heading">{t('shell.administration')}</div>
                <div className="nav-group">
                  {ADMIN_NAV.map((item) => (
                    <NavItemLink key={item.to} item={item} collapsed={false} end={item.to === '/admin'} />
                  ))}
                </div>
              </>
            ) : null}
            <div className="sidebar-footer">
              <span className="small muted">
                Janus {config?.version ?? ''} · {config?.build?.slice(0, 7) ?? ''}
              </span>
            </div>
          </nav>
        </>
      ) : null}
    </div>
  );
}

function NavItemLink({ item, collapsed, end }: { item: NavItem; collapsed: boolean; end?: boolean }): ReactNode {
  const { pathname } = useLocation();
  const peopleTeams = item.to === '/admin/users' && (pathname === '/admin/teams' || pathname.startsWith('/admin/teams/'));
  return (
    <NavLink
      to={peopleTeams ? '/admin/teams' : item.to}
      end={end}
      className={({ isActive }) => `nav-item${isActive || peopleTeams ? ' nav-item-active' : ''}`}
      title={collapsed ? item.label() : undefined}
    >
      <span className="nav-glyph" aria-hidden="true">
        {item.glyph}
      </span>
      {!collapsed ? <span className="nav-label">{item.label()}</span> : <span className="sr-only">{item.label()}</span>}
    </NavLink>
  );
}

function Brand({ collapsed, compact }: { collapsed: boolean; compact?: boolean }): ReactNode {
  return (
    <NavLink to="/dashboard" className="brand" aria-label={t('shell.brandHome')}>
      <span className="brand-mark" aria-hidden="true">
        {/* Two faces: one looking inward, one outward. */}
        <svg viewBox="0 0 32 32" width="26" height="26">
          <defs>
            <linearGradient id="janus-brand" x1="0" y1="0" x2="1" y2="1">
              <stop offset="0%" stopColor="#7c86ff" />
              <stop offset="100%" stopColor="#fbbf24" />
            </linearGradient>
          </defs>
          <circle cx="16" cy="16" r="14" fill="none" stroke="url(#janus-brand)" strokeWidth="2" />
          <path d="M16 4v24" stroke="url(#janus-brand)" strokeWidth="1.5" opacity="0.5" />
          <path d="M11 11c-2.5 2-2.5 8 0 10" fill="none" stroke="#7c86ff" strokeWidth="2" strokeLinecap="round" />
          <path d="M21 11c2.5 2 2.5 8 0 10" fill="none" stroke="#fbbf24" strokeWidth="2" strokeLinecap="round" />
        </svg>
      </span>
      {!collapsed ? <span className="brand-word">{compact ? 'Janus' : 'Janus'}</span> : null}
    </NavLink>
  );
}

function ThroughputReadout({ signal, collapsed }: { signal: AmbientSignal; collapsed: boolean }): ReactNode {
  if (collapsed) {
    return (
      <div className="throughput" title={t('shell.requestsPerMinute', { count: Math.round(signal.requestsPerMinute) })}>
        <span className="live-dot" aria-hidden="true" />
      </div>
    );
  }
  return (
    <div className="throughput">
      <span className="live-dot" aria-hidden="true" />
      <div>
        <div className="small" style={{ fontWeight: 600 }}>
          {t('shell.reqPerMin', { count: Math.round(signal.requestsPerMinute).toLocaleString() })}
        </div>
        <div className="small muted num">
          {t('shell.tokensInPerMin', { count: Math.round(signal.tokensInPerMinute).toLocaleString() })}
          {' · '}
          {t('shell.tokensOutPerMin', { count: Math.round(signal.tokensOutPerMinute).toLocaleString() })}
        </div>
      </div>
    </div>
  );
}

/** Converts the last two hours of buckets into a live ambient signal. Exported for tests. */
export function deriveSignal(data: GlobalDashboard | undefined): AmbientSignal {
  if (!data || data.series.length === 0) {
    return {
      tokensPerMinute: 0,
      tokensInPerMinute: 0,
      tokensOutPerMinute: 0,
      requestsPerMinute: 0,
      costPerMinute: 0,
      errorRate: 0,
      recentJobs: [],
    };
  }
  const recent = data.series.slice(-3);
  const bucketMinutes = 5;
  const totals = recent.reduce(
    (acc, point) => ({
      tokensIn: acc.tokensIn + point.totals.tokens_in,
      tokensOut: acc.tokensOut + point.totals.tokens_out,
      requests: acc.requests + point.totals.request_count,
      // In local-only mode every cost_nanousd is 0, so costPerMinute (which
      // only tints the ambient canvas — no dollar figure is ever shown here)
      // degrades to zero warmth without any gating.
      cost: acc.cost + point.totals.cost_nanousd,
      errors: acc.errors + point.totals.error_count,
    }),
    { tokensIn: 0, tokensOut: 0, requests: 0, cost: 0, errors: 0 },
  );
  const minutes = Math.max(1, recent.length * bucketMinutes);
  return {
    // The canvas keeps consuming the combined figure; the readout shows the split.
    tokensPerMinute: (totals.tokensIn + totals.tokensOut) / minutes,
    tokensInPerMinute: totals.tokensIn / minutes,
    tokensOutPerMinute: totals.tokensOut / minutes,
    requestsPerMinute: totals.requests / minutes,
    costPerMinute: totals.cost / 1_000_000_000 / minutes,
    errorRate: totals.requests > 0 ? totals.errors / totals.requests : 0,
    // One entry per real request; the canvas fires each exactly once and
    // sizes its trail from tokens_out.
    recentJobs: (data.recent_jobs ?? []).map((job) => ({
      at: new Date(job.at).getTime(),
      tokensOut: job.tokens_out,
      error: job.error,
    })),
  };
}
