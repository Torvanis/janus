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

interface NavSection {
  id: string;
  /** Heading; omitted for the top block (Chat, Dashboard). */
  label?: () => string;
  items: NavItem[];
}

// Labels are resolved lazily through t() so nav copy stays in the i18n layer.
// Grouped by what the person is doing, so a long list reads as a few short
// ones: the everyday pages, then usage, then access.
const PRIMARY_SECTIONS: NavSection[] = [
  {
    id: 'main',
    items: [
      { to: '/chat', label: () => 'Chat', glyph: '✎' },
      { to: '/dashboard', label: () => t('shell.navDashboard'), glyph: '◈' },
    ],
  },
  {
    id: 'usage',
    label: () => 'Usage',
    items: [
      { to: '/requests', label: () => t('shell.navRequests'), glyph: '≡' },
      { to: '/reports', label: () => 'Reports', glyph: '▥' },
      { to: '/quota', label: () => t('shell.navQuotas'), glyph: '◐' },
      { to: '/explore', label: () => t('shell.navOrgPulse'), glyph: '✦' },
    ],
  },
  {
    id: 'access',
    label: () => 'Access',
    items: [
      { to: '/models', label: () => t('shell.navModels'), glyph: '◇' },
      { to: '/tokens', label: () => t('shell.navTokens'), glyph: '⚿' },
      { to: '/subscriptions', label: () => t('shell.navSubscriptions'), glyph: '⚯' },
      { to: '/teams', label: () => t('shell.navTeam'), glyph: '⧉' },
      { to: '/help', label: () => t('shell.navHelp'), glyph: '?' },
    ],
  },
];

// Administration folds into sections; the one holding the current page
// opens itself, and the rest remember what the admin last chose.
const ADMIN_SECTIONS: NavSection[] = [
  {
    id: 'admin-overview',
    items: [{ to: '/admin', label: () => t('shell.navOverview'), glyph: '▤', admin: true }],
  },
  {
    // Chat stands on its own: who gets it, its models and its tools.
    id: 'admin-chat',
    items: [{ to: '/admin/chat', label: () => 'Chat', glyph: '✎', admin: true }],
  },
  {
    id: 'admin-gateway',
    label: () => 'Gateway',
    items: [
      { to: '/admin/upstreams', label: () => t('shell.navUpstreams'), glyph: '⇅', admin: true },
      { to: '/admin/models', label: () => t('shell.navModels'), glyph: '◈', admin: true },
      { to: '/admin/managed-models', label: () => t('shell.navManagedModels'), glyph: '⧫', admin: true },
      { to: '/admin/routing', label: () => t('shell.navRouting'), glyph: '⇶', admin: true },
      { to: '/admin/grants', label: () => t('shell.navGrants'), glyph: '⚑', admin: true },
      { to: '/admin/service-tokens', label: () => t('shell.navServiceTokens'), glyph: '⚙', admin: true },
    ],
  },
  {
    id: 'admin-people',
    label: () => 'People & Usage',
    items: [
      { to: '/admin/users', label: () => t('shell.navPeople'), glyph: '⚭', admin: true },
      { to: '/admin/quotas', label: () => t('shell.navQuotas'), glyph: '◑', admin: true },
      { to: '/admin/insights', label: () => t('shell.navInsights'), glyph: '◭', admin: true },
    ],
  },
  {
    id: 'admin-security',
    label: () => 'Safety & Security',
    items: [
      { to: '/admin/security', label: () => t('shell.navSecurity'), glyph: '⛉', admin: true },
      { to: '/admin/child-safety', label: () => 'Child Safety', glyph: '☂', admin: true },
      { to: '/admin/rules', label: () => t('shell.navPolicyRules'), glyph: '⛨', admin: true },
      { to: '/admin/alerts', label: () => t('shell.navAlerts'), glyph: '◉', admin: true },
    ],
  },
  {
    id: 'admin-activity',
    label: () => 'Activity',
    items: [
      { to: '/admin/requests', label: () => t('shell.navRequests'), glyph: '≡', admin: true },
      { to: '/admin/audit', label: () => t('shell.navAuditLog'), glyph: '❑', admin: true },
    ],
  },
  {
    id: 'admin-settings',
    items: [{ to: '/admin/settings', label: () => t('shell.navSettings'), glyph: '⚙', admin: true }],
  },
];

const NAV_OPEN_KEY = 'janus.nav.open';

/** Whether pathname is (under) item.to, the way NavLink decides "active". */
function navMatches(item: NavItem, pathname: string): boolean {
  if (item.to === '/admin') return pathname === '/admin';
  if (item.to === '/admin/users' && pathname.startsWith('/admin/teams')) return true;
  return pathname === item.to || pathname.startsWith(item.to + '/');
}

function readOpenSections(): Record<string, boolean> {
  try {
    const raw = localStorage.getItem(NAV_OPEN_KEY);
    return raw ? (JSON.parse(raw) as Record<string, boolean>) : {};
  } catch {
    return {};
  }
}

export function Shell({ children }: { children: ReactNode }): ReactNode {
  const { me, config, refresh } = useSession();
  // The Subscriptions page exists only while the administrator has the
  // feature on (and never on an offline install).
  const navVisible = (item: NavItem) =>
    (item.to !== '/subscriptions' || me?.personal_subscriptions === true) && (item.to !== '/chat' || me?.chat_preview === true);
  const primarySections = PRIMARY_SECTIONS.map((section) => ({ ...section, items: section.items.filter(navVisible) })).filter(
    (section) => section.items.length > 0,
  );
  const primaryNav = primarySections.flatMap((section) => section.items);
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
          {/* Only the middle scrolls: the brand and the live readout stay put
              however long the menu grows. */}
          <div className="sidebar-scroll">
            <NavSections sections={primarySections} collapsed={isRail} />
            {showAdmin ? (
              <>
                <div className="nav-heading nav-heading-admin">{isRail ? '···' : t('shell.administration')}</div>
                <NavSections sections={ADMIN_SECTIONS} collapsed={isRail} foldable />
              </>
            ) : null}
          </div>
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
            {primaryNav.slice(0, 4).map((item) => (
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
            <NavSections sections={primarySections} collapsed={false} />
            <div className="nav-group">
              <NavItemLink
                item={{ to: '/settings', label: () => t('shell.navPersonalSettings'), glyph: '⚙' }}
                collapsed={false}
              />
              <NavItemLink item={{ to: '/docs', label: () => t('shell.navDocumentation'), glyph: '❐' }} collapsed={false} />
            </div>
            {showAdmin ? (
              <>
                <div className="nav-heading nav-heading-admin">{t('shell.administration')}</div>
                <NavSections sections={ADMIN_SECTIONS} collapsed={false} foldable />
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

/**
 * Renders nav sections. Foldable sections get a heading button; the section
 * holding the current page is always open. In the icon rail every section is
 * open and headings become thin dividers.
 */
function NavSections({
  sections,
  collapsed,
  foldable,
}: {
  sections: NavSection[];
  collapsed: boolean;
  foldable?: boolean;
}): ReactNode {
  const { pathname } = useLocation();
  const [open, setOpen] = useState<Record<string, boolean>>(readOpenSections);
  const ref = useRef<HTMLDivElement>(null);
  // Bring the current page's row into view in a menu taller than the screen
  // (only scrolls the menu, never the page).
  useEffect(() => {
    const active = ref.current?.querySelector<HTMLElement>('.nav-item-active');
    const scroller = active?.closest<HTMLElement>('.sidebar-scroll');
    if (!active || !scroller) return;
    const a = active.getBoundingClientRect();
    const sc = scroller.getBoundingClientRect();
    if (a.top < sc.top || a.bottom > sc.bottom) {
      scroller.scrollTop += a.top - sc.top - sc.height / 3;
    }
  }, [pathname]);
  const toggle = (id: string, next: boolean) => {
    setOpen((prev) => {
      const updated = { ...prev, [id]: next };
      try {
        localStorage.setItem(NAV_OPEN_KEY, JSON.stringify(updated));
      } catch {
        // Private mode: the choice just isn't remembered.
      }
      return updated;
    });
  };
  return (
    <div ref={ref} className="nav-sections">
      {sections.map((section) => {
        const current = section.items.some((item) => navMatches(item, pathname));
        const canFold = foldable && !collapsed && !!section.label;
        const isOpen = !canFold || current || open[section.id] === true;
        const label = section.label?.();
        return (
          <div key={section.id} className="nav-section" data-section={section.id}>
            {label && collapsed ? <div className="nav-divider" aria-hidden="true" /> : null}
            {label && !collapsed && !canFold ? <div className="nav-heading">{label}</div> : null}
            {canFold && label ? (
              <button
                type="button"
                className="nav-heading nav-fold"
                aria-expanded={isOpen}
                aria-controls={`nav-${section.id}`}
                disabled={current}
                title={current ? 'Holds the current page' : undefined}
                onClick={() => toggle(section.id, !isOpen)}
              >
                <span className="nav-fold-caret" aria-hidden="true">
                  ▸
                </span>
                {label}
                <span className="nav-fold-count" aria-hidden="true">
                  {section.items.length}
                </span>
              </button>
            ) : null}
            <div className="nav-group" id={`nav-${section.id}`} hidden={!isOpen}>
              {section.items.map((item) => (
                <NavItemLink key={item.to} item={item} collapsed={collapsed} end={item.to === '/admin'} />
              ))}
            </div>
          </div>
        );
      })}
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

/** Per-second rate: one decimal below 10 so a quiet gateway is not shown as 0. */
export function formatPerSecond(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '0';
  if (value < 10) return value.toFixed(1);
  return Math.round(value).toLocaleString();
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
        <div className="small muted num" data-testid="throughput-per-second">
          {t('shell.tokensInPerSec', { count: formatPerSecond(signal.tokensInPerMinute / 60) })}
          {' · '}
          {t('shell.tokensOutPerSec', { count: formatPerSecond(signal.tokensOutPerMinute / 60) })}
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
