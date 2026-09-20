// GENERATED FILE — DO NOT EDIT BY HAND.
// Source of truth: CHANGELOG.md at the repository root.
// Regenerate with: cd web && node scripts/gen-docs.mjs
// Drift-checked by src/docs/gen.test.ts.

export interface ChangelogCategory {
  name: string;
  items: string[];
}

export interface ChangelogRelease {
  version: string;
  /** Release date as written in CHANGELOG.md; empty when undated. */
  date: string;
  categories: ChangelogCategory[];
}

export const CHANGELOG: ChangelogRelease[] = [
  {
    version: '2026.9.1',
    date: '2026-09-20',
    categories: [
      {
        name: 'Added',
        items: [
          'Initial public release of Janus Edge, a self-hosted AI gateway with provider routing, model discovery, and OpenAI-compatible APIs.',
          'User and service tokens, identity-provider integration, local authentication, team membership, quotas, and usage reporting.',
          'Security policies, guard classifiers, audit logging, and administrative troubleshooting.',
          'Separate personal Team workspaces and organization-wide team management under Administration → People → Teams.',
          'Offline license verification and self-hosted installation support.',
        ],
      },
    ],
  },
];
