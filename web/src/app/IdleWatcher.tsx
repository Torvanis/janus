import { useEffect, useState, type ReactNode } from 'react';
import { useNavigate } from 'react-router-dom';
import { api } from '../lib/api';
import { lastForegroundAt, markActivity, personActive, trackActivity } from '../lib/activity';
import { Modal } from '../components/ui';

const WARN_SECONDS = 60;
const PING_AFTER_MS = 20_000;

/**
 * Idle sign-out. The server's idle clock restarts on every request the
 * person causes; the app's own polling is marked as background and does not
 * count. This keeps the server informed while the person reads or types
 * (a light /me request at most every 20 s), warns a minute before the
 * administrator's idle limit, and goes to the signed-out page when it
 * passes.
 */
export function IdleWatcher({ idleSeconds }: { idleSeconds?: number }): ReactNode {
  const navigate = useNavigate();
  const [left, setLeft] = useState<number | null>(null);

  useEffect(() => {
    trackActivity();
  }, []);

  useEffect(() => {
    if (!idleSeconds || idleSeconds <= 0) return;
    const tick = window.setInterval(() => {
      const now = Date.now();
      if (personActive(now) && now - lastForegroundAt() > PING_AFTER_MS) {
        void api.get('/api/v1/me').catch(() => undefined);
      }
      const remaining = Math.ceil(idleSeconds - (Date.now() - lastForegroundAt()) / 1000);
      if (remaining <= 0) {
        window.clearInterval(tick);
        navigate('/auth/signed-out?reason=idle', { replace: true });
        return;
      }
      setLeft(remaining <= WARN_SECONDS ? remaining : null);
    }, 1000);
    return () => window.clearInterval(tick);
  }, [idleSeconds, navigate]);

  const stay = () => {
    markActivity();
    setLeft(null);
    void api.get('/api/v1/me').catch(() => undefined);
  };
  if (left === null) return null;
  return (
    <Modal open title="Still there?" onClose={stay}>
      <p role="status" aria-live="polite" style={{ margin: 0 }}>
        You will be signed out in {left} second{left === 1 ? '' : 's'} because of inactivity.
      </p>
      <div className="row" style={{ justifyContent: 'flex-end' }}>
        <button type="button" className="btn btn-primary" autoFocus onClick={stay}>
          Stay signed in
        </button>
      </div>
    </Modal>
  );
}
