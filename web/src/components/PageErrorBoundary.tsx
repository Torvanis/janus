/**
 * The one class component in the app. A render-time throw anywhere below
 * this boundary must not blank the whole shell: the sidebar, header and
 * every other route stay usable, and the broken page reports itself with
 * a retry. Resets when the route changes so a bad page does not follow
 * the user around.
 */
import { Component, type ReactNode } from 'react';
import { useLocation } from 'react-router-dom';
import { t } from '../lib/i18n';

type State = { error: Error | null };

class Boundary extends Component<{ children: ReactNode; resetKey: string }, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidUpdate(prev: { resetKey: string }): void {
    if (prev.resetKey !== this.props.resetKey && this.state.error) {
      this.setState({ error: null });
    }
  }

  componentDidCatch(error: Error): void {
    // The console is the right place for the stack; the page gets a sentence.
    console.error('[janus] page render failed', error);
  }

  render(): ReactNode {
    if (!this.state.error) return this.props.children;
    return (
      <div className="state" role="alert" data-testid="page-error-boundary">
        <div aria-hidden="true" style={{ fontSize: 28, color: 'var(--janus-color-danger-fg)' }}>
          !
        </div>
        <div className="state-title">{t('errorBoundary.title')}</div>
        <p className="state-body">{t('errorBoundary.body')}</p>
        <p className="state-body small muted mono">{this.state.error.message}</p>
        <div className="row">
          <button type="button" className="btn" onClick={() => this.setState({ error: null })}>
            {t('common.tryAgain')}
          </button>
          <button type="button" className="btn btn-ghost" onClick={() => window.location.reload()}>
            {t('errorBoundary.reload')}
          </button>
        </div>
      </div>
    );
  }
}

export function PageErrorBoundary({ children }: { children: ReactNode }): ReactNode {
  const location = useLocation();
  return <Boundary resetKey={location.pathname}>{children}</Boundary>;
}
