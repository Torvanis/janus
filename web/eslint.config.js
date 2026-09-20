// Flat ESLint config for the Janus web SPA. Semantics-focused: formatting is
// Prettier's job (eslint-config-prettier disables every stylistic rule), and
// `tsc --noEmit` already gates the type-level errors, so this layer catches
// the logic mistakes the compiler cannot (unsafe equality, unused disables,
// accidental shadowed exports, …).
import js from '@eslint/js';
import tseslint from 'typescript-eslint';
import prettier from 'eslint-config-prettier';

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'public/**'] },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  prettier,
  {
    files: ['src/**/*.{ts,tsx}', 'scripts/**/*.mjs', 'vite.config.ts'],
    rules: {
      // The compiler (`tsc --noEmit`, part of `npm run build`) already flags
      // unused locals with better fix-it context; duplicating it here only
      // produces double reports for the same mistake.
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      // Deliberate escape hatches (adapter payloads, JSON columns) annotate
      // with `any` on purpose and are reviewed case by case.
      '@typescript-eslint/no-explicit-any': 'off',
      eqeqeq: ['error', 'smart'],
    },
  },
  {
    // Node-side tooling (docs generator, Vite config) runs outside the
    // browser bundle: give it the Node globals and allow the vitest/config
    // triple-slash reference Vite's own template uses.
    files: ['scripts/**/*.mjs', 'vite.config.ts'],
    languageOptions: {
      globals: { process: 'readonly', console: 'readonly', URL: 'readonly', URLSearchParams: 'readonly' },
    },
    rules: {
      '@typescript-eslint/triple-slash-reference': 'off',
    },
  },
);
