/**
 * Safety & Security → Child Safety: verdict states, age ranges with the
 * three outcomes and editable instructions, children/groups with an age
 * range each, and what Save sends. Every edition: no license is mocked.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { ChildBuiltinText, FamilyDoc } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { familyVerdict } from '../../lib/familyHealth';
import { ChildSafetyPage } from './ChildSafety';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

const SLUGS = ['feeling_low', 'not_for_children', 'bypass_controls', 'personal_info', 'homework_shortcut', 'homework_learning'];
const NAMES: Record<string, string> = {
  feeling_low: 'Feeling low or unsafe',
  not_for_children: 'Not suitable for children',
  bypass_controls: 'Getting around parental controls',
  personal_info: 'Sharing personal information',
  homework_shortcut: 'Homework done for them',
  homework_learning: 'Homework learning',
};

function builtin(outcomes: Record<string, 'off' | 'answer' | 'block'>): ChildBuiltinText {
  return {
    base: 'You are talking with a child.',
    categories: Object.fromEntries(SLUGS.map((s) => [s, `Built-in text for ${s}.`])),
    outcomes: Object.fromEntries(SLUGS.map((s) => [s, outcomes[s] ?? 'answer'])),
  };
}

function doc(over: Partial<FamilyDoc> = {}): FamilyDoc {
  return {
    family: {
      enabled: true,
      classifier_model_id: '',
      ask: 'together',
      timeout_ms: 4000,
      profiles: [
        { id: 'cs_little', name: 'Little kids (8 and under)', builtin: true, categories: {}, blocked_models: [] },
        { id: 'cs_kids', name: 'Kids (9-12)', builtin: true, categories: {}, blocked_models: [] },
        { id: 'cs_teens', name: 'Teens (13-17)', builtin: true, categories: {}, blocked_models: [] },
      ],
      assignments: [{ scope_type: 'user', scope_id: 'kid-1', profile_id: 'cs_kids', name: 'Kid One' }],
      updated_at: '2026-10-27T00:00:00Z',
    },
    categories: SLUGS.map((slug) => ({ slug, name: NAMES[slug]!, includes: `About ${slug}.` })),
    safety_categories: [{ slug: 'self_harm', name: 'Suicide and self-harm', includes: '' }],
    classifiers: [
      {
        id: 'jev',
        name: 'jev-1',
        classifier_role: 'decision',
        upstream_id: 'u',
        upstream_name: 'OpenRouter',
        hosted: true,
        recommended: 'hosted_fastest',
      },
      { id: 'clef', name: 'clef', classifier_role: 'decision', upstream_id: 'l', upstream_name: 'Local', hosted: false },
    ],
    builtin: {
      cs_little: builtin({ feeling_low: 'block', not_for_children: 'block', bypass_controls: 'block' }),
      cs_kids: builtin({ feeling_low: 'block' }),
      cs_teens: builtin({}),
    },
    tutor_rule: 'NEVER give the final answer, even after they try.',
    status: { state: 'not_protecting', reasons: ['no_model'], covered_users: 1 },
    recent: [],
    support_message: 'I am really glad you told me.',
    withheld_note: "I almost gave you the answer there, but you'll learn more by working it out yourself.",
    ...over,
  };
}

function mount(d: FamilyDoc) {
  mocked.get.mockImplementation(async (path: string) => {
    if (path === '/api/v1/admin/child-safety') return d as never;
    if (path.startsWith('/api/v1/admin/users')) return { users: [{ id: 'kid-1', name: 'Kid One', email: 'k@x' }] } as never;
    if (path.startsWith('/api/v1/admin/models'))
      return { models: [{ id: 'mdl-1', name: 'gpt-x', display_name: 'Big Brain', upstream_name: 'openai' }] } as never;
    if (path.startsWith('/api/v1/admin/managed-models'))
      return {
        managed_models: [
          { id: 'mm-1', name: 'creative', kind: 'alias', target_public_name: 'Big Brain', target_model_name: 'gpt-x' },
        ],
      } as never;
    throw new Error(`unexpected GET ${path}`);
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter>
          <ChildSafetyPage />
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe('familyVerdict', () => {
  it('ranks protecting, not protecting with reasons, and off', () => {
    expect(familyVerdict('protecting', []).tone).toBe('success');
    const red = familyVerdict('not_protecting', ['no_model', 'no_one_assigned']);
    expect(red.tone).toBe('danger');
    expect(red.reasons).toEqual(['adminSecurity.family.reasonNoModel', 'adminSecurity.family.reasonNoOneAssigned']);
    expect(familyVerdict('off', []).tone).toBe('neutral');
  });
});

describe('Child Safety page', () => {
  it('shows Not protecting with the reason, then Protecting with the count', async () => {
    mount(doc());
    const verdict = await screen.findByTestId('family-verdict');
    expect(verdict.getAttribute('data-state')).toBe('not_protecting');
    expect(verdict.textContent).toContain('No safety model is chosen');
    cleanup();
    mount(doc({ status: { state: 'protecting', reasons: [], covered_users: 3 } }));
    expect((await screen.findByTestId('family-verdict')).textContent).toContain('3 children');
  });

  it('shows each age range with its defaults and the three outcomes per category', async () => {
    const user = userEvent.setup();
    mount(doc());
    await screen.findByTestId('family-verdict');
    // Kids is open first.
    expect(screen.getByRole('tab', { name: 'Kids (9-12)' }).getAttribute('aria-selected')).toBe('true');
    const feeling = screen.getByRole('radiogroup', { name: 'Feeling low or unsafe: what happens' });
    expect(within(feeling).getByRole('radio', { name: 'Block' }).getAttribute('aria-checked')).toBe('true');
    expect(
      within(feeling)
        .getAllByRole('radio')
        .map((r) => r.textContent),
    ).toEqual(['Off', 'Answer appropriately', 'Block']);
    expect(screen.getByText(/988 Lifeline/)).toBeTruthy();
    // Little kids block grown-up topics by default.
    await user.click(screen.getByRole('tab', { name: 'Little kids (8 and under)' }));
    const grownUp = screen.getByRole('radiogroup', { name: 'Not suitable for children: what happens' });
    expect(within(grownUp).getByRole('radio', { name: 'Block' }).getAttribute('aria-checked')).toBe('true');
    // The tutor rule and the always-on list are shown.
    expect(screen.getByText(/one that gives a final answer away stops there/)).toBeTruthy();
    expect(screen.getByRole('list', { name: 'Always blocked' }).textContent).toContain('Suicide and self-harm');
  });

  it('edits instructions, resets them, warns when Feeling low is off, and saves the age range', async () => {
    const user = userEvent.setup();
    mocked.put.mockResolvedValue(doc() as never);
    mount(doc());
    await screen.findByTestId('family-verdict');
    const privacy = screen.getByRole('radiogroup', { name: 'Sharing personal information: what happens' });
    await user.click(within(privacy).getByRole('radio', { name: 'Answer appropriately' }));
    const box = screen.getByLabelText('Instructions for Sharing personal information') as HTMLTextAreaElement;
    expect(box.value).toBe('Built-in text for personal_info.');
    await user.clear(box);
    await user.type(box, 'Our words.');
    expect(screen.getByRole('button', { name: 'Reset to built-in' })).toBeTruthy();
    await user.click(
      within(screen.getByRole('radiogroup', { name: 'Feeling low or unsafe: what happens' })).getByRole('radio', { name: 'Off' }),
    );
    expect(screen.getByRole('note').textContent).toMatch(/gets an ordinary answer, and you are not told/);
    await user.click(
      within(screen.getByRole('radiogroup', { name: 'Homework done for them: what happens' })).getByRole('radio', {
        name: 'Block',
      }),
    );
    await user.selectOptions(screen.getByLabelText('Safety model', { selector: 'select' }), 'jev');
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const [path, body] = mocked.put.mock.calls[0] as [string, FamilyDoc['family']];
    expect(path).toBe('/api/v1/admin/child-safety');
    expect(body.classifier_model_id).toBe('jev');
    const kids = body.profiles.find((p) => p.id === 'cs_kids')!;
    expect(kids.categories.personal_info).toEqual({ outcome: 'answer', instructions: 'Our words.' });
    expect(kids.categories.feeling_low!.outcome).toBe('off');
    expect(kids.categories.homework_shortcut!.outcome).toBe('block');
    expect(body.assignments).toEqual([{ scope_type: 'user', scope_id: 'kid-1', profile_id: 'cs_kids', name: 'Kid One' }]);
  });

  it('moves a child to another age range, copies a range and blocks a model on it', async () => {
    const user = userEvent.setup();
    mocked.put.mockResolvedValue(doc() as never);
    mount(doc());
    await screen.findByTestId('family-verdict');
    await user.selectOptions(screen.getByLabelText('Age range for Kid One'), 'cs_teens');
    await user.click(screen.getByRole('button', { name: 'Copy to a new range' }));
    const name = screen.getByLabelText('Name', { selector: 'input' }) as HTMLInputElement;
    expect(name.value).toBe('Kids (9-12) (copy)');
    await user.clear(name);
    await user.type(name, 'Kids, strict');
    const blocked = screen.getByLabelText('Block these models (optional)');
    await user.type(blocked, 'my/xai/grok-4.7{enter}');
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const [, body] = mocked.put.mock.calls[0] as [string, FamilyDoc['family']];
    expect(body.assignments[0]!.profile_id).toBe('cs_teens');
    const strict = body.profiles.find((p) => p.name === 'Kids, strict')!;
    expect(strict.builtin).toBe(false);
    expect(strict.blocked_models).toEqual(['my/xai/grok-4.7']);
    expect(Object.keys(strict.categories).sort()).toEqual([...SLUGS].sort());
  });

  it('shows blocked models by name and saves their ids, so a rename keeps the block', async () => {
    const user = userEvent.setup();
    const d = doc();
    d.family.profiles[1]!.blocked_models = ['mm-1'];
    mocked.put.mockResolvedValue(d as never);
    mount(d);
    await screen.findByTestId('family-verdict');
    expect(await screen.findByText('creative')).toBeTruthy();
    expect(screen.queryByText('mm-1')).toBeNull();
    const blocked = screen.getByLabelText('Block these models (optional)');
    await user.type(blocked, 'Big Br');
    await user.click(await screen.findByRole('option', { name: /Big Brain/ }));
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const [, body] = mocked.put.mock.calls[0] as [string, FamilyDoc['family']];
    expect(body.profiles.find((p) => p.id === 'cs_kids')!.blocked_models).toEqual(['mm-1', 'mdl-1']);
  });

  it('switches media on per age range (all off by default) and saves them', async () => {
    const user = userEvent.setup();
    mocked.put.mockResolvedValue(doc() as never);
    mount(doc());
    await screen.findByTestId('family-verdict');
    for (const label of ['Make pictures', 'Make videos', 'Make music', 'Turn speech into text']) {
      expect((screen.getByRole('checkbox', { name: label }) as HTMLInputElement).checked).toBe(false);
    }
    await user.click(screen.getByRole('checkbox', { name: 'Make pictures' }));
    await user.click(screen.getByRole('checkbox', { name: 'Turn speech into text' }));
    await user.click(screen.getAllByRole('button', { name: 'Save' })[0]!);
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    const [, body] = mocked.put.mock.calls[0] as [string, FamilyDoc['family']];
    expect(body.profiles.find((p) => p.id === 'cs_kids')!.media).toEqual({
      image: true,
      video: false,
      music: false,
      transcription: true,
    });
    expect(body.profiles.find((p) => p.id === 'cs_teens')!.media).toEqual({
      image: false,
      video: false,
      music: false,
      transcription: false,
    });
  });
});
