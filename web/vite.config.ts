/// <reference types="vitest/config" />
import { writeFileSync } from 'node:fs';
import { defineConfig, type Plugin } from 'vitest/config';
import react from '@vitejs/plugin-react';

// dist/.gitkeep is TRACKED: the Go `//go:embed all:dist` directive needs the
// directory to exist in a fresh clone, before any web build has run. Vite's
// emptyOutDir wipes it on every build, and a subsequent stage-everything
// commit then silently deletes it from git — which is exactly how the
// fresh-clone build broke before. Restore it after every build.
const GITKEEP = `# Keeps web/dist/ present in fresh clones so the \`//go:embed all:dist\`
# directive in web/embed.go resolves before \`make web\` produces the bundle.
# Do not delete. See .gitignore carve-out \`!web/dist/.gitkeep\`.
`;

function keepGitkeep(): Plugin {
  return {
    name: 'janus-restore-dist-gitkeep',
    closeBundle() {
      writeFileSync(new URL('dist/.gitkeep', import.meta.url), GITKEEP);
    },
  };
}

// The dev server proxies the gateway API so `npm run dev` behaves exactly like
// the production single-binary deployment, where Go serves the built bundle.
export default defineConfig({
  plugins: [react(), keepGitkeep()],
  server: {
    host: '127.0.0.1',
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/auth': 'http://127.0.0.1:8080',
      '/v1': 'http://127.0.0.1:8080',
      '/healthz': 'http://127.0.0.1:8080',
      '/metrics': 'http://127.0.0.1:8080',
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
  test: {
    environment: 'jsdom',
    globals: true,
  },
});
