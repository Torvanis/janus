import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
// Global styles must be imported BEFORE any component module: component
// stylesheets (e.g. shell.css) carry responsive overrides for classes defined
// in global.css, and CSS cascade order follows bundle emission order. With
// global.css imported after App, shell.css's mobile media queries were
// silently overridden and phones got the desktop grid/gutter values.
import './styles/global.css';
import { App } from './App';
import { ToastProvider } from './components/ui';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Dashboards must feel live without hammering the gateway.
      staleTime: 5_000,
      refetchOnWindowFocus: true,
      retry: (failureCount, error) => {
        // Authentication and permission failures are terminal: retrying them
        // only produces more 401s and a slower error message.
        const status = (error as { status?: number }).status;
        if (status === 401 || status === 403 || status === 404) return false;
        return failureCount < 2;
      },
    },
  },
});

const container = document.getElementById('root');
if (!container) {
  throw new Error('The application root element is missing from index.html');
}

createRoot(container).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <BrowserRouter>
        <ToastProvider>
          <App />
        </ToastProvider>
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
