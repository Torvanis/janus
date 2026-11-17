import { Suspense, lazy, type ReactNode } from 'react';
import { Link, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { SessionProvider, useSession } from './app/session';
import { Shell } from './app/Shell';
import { Spinner } from './components/ui';
import { t } from './lib/i18n';
import { LoginPage, SignedOutPage } from './routes/Login';
import { DashboardPage } from './routes/Dashboard';
import { TokensPage } from './routes/Tokens';
import { ModelsPage } from './routes/Models';
import { RequestsPage } from './routes/Requests';
import { QuotaPage } from './routes/Quota';
import { TeamUsageRedirect } from './routes/TeamUsageRedirect';
import Teams from './routes/Teams';
import { LegacyWorkspaceRedirect } from './app/LegacyWorkspaceRedirect';
import Reports from './routes/Reports';
import { ReportClassifications } from './routes/ReportClassifications';
import { ExplorePage } from './routes/Explore';
import { HelpPage } from './routes/Help';
import { NotificationsPage } from './routes/Notifications';
import { SubscriptionsPage } from './routes/Subscriptions';
import { SettingsPage } from './routes/Settings';
import { ForbiddenPage, NotFoundPage } from './routes/Errors';

// Documentation and the admin console are separate bundles: most sessions never
// open them, and the first paint of the dashboard should not pay for them.
const DocsRoutes = lazy(() => import('./routes/docs/DocsRoutes'));
const AdminRoutes = lazy(() => import('./routes/admin/AdminRoutes'));
const ChatPage = lazy(() => import('./routes/chat/ChatPage'));
const SharedChatPage = lazy(() => import('./routes/chat/SharedChatPage'));

export function App(): ReactNode {
  return (
    <SessionProvider>
      <AppRoutes />
    </SessionProvider>
  );
}

function AppRoutes(): ReactNode {
  return (
    <Routes>
      <Route path="/auth/login" element={<LoginPage />} />
      <Route path="/auth/signed-out" element={<SignedOutPage />} />
      <Route
        path="/docs/*"
        element={
          <Suspense fallback={<FullPageSpinner />}>
            <DocsRoutes />
          </Suspense>
        }
      />
      <Route
        path="*"
        element={
          <RequireSession>
            <Shell>
              <Routes>
                <Route path="/" element={<Navigate to="/dashboard" replace />} />
                <Route path="/dashboard" element={<DashboardPage />} />
                <Route path="/tokens" element={<TokensPage />} />
                <Route path="/tokens/:id" element={<TokensPage />} />
                <Route path="/models" element={<ModelsPage />} />
                <Route path="/requests" element={<RequestsPage />} />
                <Route path="/reports" element={<Reports />} />
                <Route path="/reports/classifications" element={<ReportClassifications />} />
                <Route path="/quota" element={<QuotaPage />} />
                <Route path="/teams" element={<Teams />} />
                <Route
                  path="/teams/import"
                  element={
                    <RequireAdmin>
                      <LegacyWorkspaceRedirect />
                    </RequireAdmin>
                  }
                />
                <Route path="/teams/:id" element={<Teams />} />
                <Route path="/teams/:teamId/usage" element={<TeamUsageRedirect />} />
                <Route path="/explore" element={<ExplorePage />} />
                <Route path="/help" element={<HelpPage />} />
                <Route path="/notifications" element={<NotificationsPage />} />
                <Route path="/subscriptions" element={<SubscriptionsPage />} />
                <Route path="/subscriptions/:provider" element={<SubscriptionsPage />} />
                <Route path="/settings" element={<SettingsPage />} />
                <Route
                  path="/chat/shared/:share"
                  element={
                    <RequireChat shared>
                      <Suspense fallback={<FullPageSpinner />}>
                        <SharedChatPage />
                      </Suspense>
                    </RequireChat>
                  }
                />
                {['/chat', '/chat/:id'].map((path) => (
                  <Route
                    key={path}
                    path={path}
                    element={
                      <RequireChat>
                        <Suspense fallback={<FullPageSpinner />}>
                          <ChatPage />
                        </Suspense>
                      </RequireChat>
                    }
                  />
                ))}
                <Route
                  path="/admin/*"
                  element={
                    <RequireAdmin>
                      <Suspense fallback={<FullPageSpinner />}>
                        <AdminRoutes />
                      </Suspense>
                    </RequireAdmin>
                  }
                />
                <Route path="/403" element={<ForbiddenPage />} />
                <Route path="*" element={<NotFoundPage />} />
              </Routes>
            </Shell>
          </RequireSession>
        }
      />
    </Routes>
  );
}

function FullPageSpinner(): ReactNode {
  return (
    <div className="state" style={{ minHeight: '50dvh' }}>
      <Spinner label="Loading" />
    </div>
  );
}

/**
 * Gates the authenticated shell. An expired session sends the viewer to the
 * sign-in page carrying the path they wanted, so they land where they intended
 * after authenticating.
 */
function RequireSession({ children }: { children: ReactNode }): ReactNode {
  const { me, loading } = useSession();
  const location = useLocation();

  if (loading) {
    return (
      <div className="state" style={{ minHeight: '100dvh' }}>
        <Spinner label="Checking your session" />
      </div>
    );
  }
  if (!me) {
    const target = `${location.pathname}${location.search}`;
    return <Navigate to={`/auth/login?redirect_uri=${encodeURIComponent(target)}`} replace />;
  }
  return <>{children}</>;
}

/**
 * Chat pages render only while Administration → Chat has Chat on
 * (me.chat_enabled) and, except for a shared chat, this person may use it
 * (me.chat_available). Otherwise the page explains why and makes no chat
 * API call; administrators get a link to the switch.
 */
function RequireChat({ children, shared = false }: { children: ReactNode; shared?: boolean }): ReactNode {
  const { me } = useSession();
  const isAdmin = me?.role === 'admin';
  if (me?.chat_enabled === true && (shared || me.chat_available === true)) return <>{children}</>;
  const off = me?.chat_enabled !== true;
  return (
    <div className="page">
      <div className="state" style={{ minHeight: '60dvh' }}>
        <h1 className="state-title">{off ? 'Chat is turned off' : 'Chat is not available to you'}</h1>
        <p className="state-body">
          {off
            ? isAdmin
              ? 'Janus Chat is turned off for everyone on this Janus. You can turn it on in Administration → Chat.'
              : 'Janus Chat is turned off for this Janus. Ask an administrator if you need it.'
            : me?.chat_unavailable_reason || 'Ask an administrator for access to Janus Chat.'}
        </p>
        <div className="row">
          {isAdmin ? (
            <Link className="btn btn-primary" to="/admin/chat">
              Open Administration → Chat
            </Link>
          ) : null}
          <Link className={isAdmin ? 'btn' : 'btn btn-primary'} to="/dashboard">
            {t('common.backToDashboard')}
          </Link>
        </div>
      </div>
    </div>
  );
}

function RequireAdmin({ children }: { children: ReactNode }): ReactNode {
  const { me } = useSession();
  if (me?.role !== 'admin') {
    return <ForbiddenPage requiredRole="administrator" />;
  }
  return <>{children}</>;
}
