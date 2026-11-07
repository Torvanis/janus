import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { QuestionCard, type ChatQuestion } from './ChatQuestion';

const q: ChatQuestion = {
  questions: [
    {
      question: 'How often should backups run?',
      options: [{ label: 'Nightly' }, { label: 'Hourly', description: 'More storage' }],
      allow_other: true,
    },
    { question: 'Where should they go?', options: [{ label: 'S3' }, { label: 'NFS' }], multi_select: true },
  ],
};

afterEach(cleanup);

describe('QuestionCard', () => {
  it('marks the first choice recommended and sends the picked answers', async () => {
    const onAnswer = vi.fn();
    render(<QuestionCard question={q} active onAnswer={onAnswer} />);
    expect(screen.getAllByText('Recommended')).toHaveLength(2);
    const send = screen.getByRole('button', { name: 'Send answer' });
    expect((send as HTMLButtonElement).disabled).toBe(true);
    const user = userEvent.setup();
    await user.click(screen.getByRole('radio', { name: /Hourly/ }));
    await user.click(screen.getByRole('radio', { name: /Nightly/ }));
    await user.click(screen.getByRole('checkbox', { name: /S3/ }));
    await user.click(screen.getByRole('checkbox', { name: /NFS/ }));
    expect(screen.getByRole('radio', { name: /Nightly/ }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('radio', { name: /Hourly/ }).getAttribute('aria-checked')).toBe('false');
    await user.click(send);
    expect(onAnswer).toHaveBeenCalledWith('How often should backups run: Nightly\nWhere should they go: S3, NFS');
  });

  it('takes a typed answer', async () => {
    const onAnswer = vi.fn();
    render(
      <QuestionCard
        question={{
          questions: [{ question: 'Which port?', options: [{ label: '51820' }, { label: '443' }], allow_other: true }],
        }}
        active
        onAnswer={onAnswer}
      />,
    );
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Your own answer: Which port?'), '8443{Enter}');
    expect(onAnswer).toHaveBeenCalledWith('8443');
  });

  it('is read-only once answered or superseded', () => {
    render(<QuestionCard question={q} active={false} onAnswer={vi.fn()} />);
    expect(screen.queryByRole('button', { name: 'Send answer' })).toBeNull();
    expect((screen.getByRole('radio', { name: /Nightly/ }) as HTMLButtonElement).disabled).toBe(true);
  });
});
