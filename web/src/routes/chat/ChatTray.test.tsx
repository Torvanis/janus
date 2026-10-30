import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ChatTray, ModeChips, TrayButton, childMediaNote } from './ChatTray';

function wrap(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

afterEach(() => vi.restoreAllMocks());

const tools = [
  { id: 'web_search', label: 'Web search', description: 'Search the web' },
  { id: 'create_file', label: 'Create files', description: 'Hand over whole files' },
  { id: 'x_search', label: 'X search', description: 'Search X' },
];

function tray(over: Partial<Parameters<typeof ChatTray>[0]> = {}) {
  const props: Parameters<typeof ChatTray>[0] = {
    open: true,
    onClose: vi.fn(),
    busy: false,
    model: 'a',
    chatOptions: [
      { value: 'a', label: 'Model A' },
      { value: 'b', label: 'Model B' },
      { value: 'c', label: 'Model C' },
      { value: 'd', label: 'Model D' },
    ],
    research: false,
    onResearch: vi.fn(),
    compare: [],
    onCompare: vi.fn(),
    tools,
    available: ['web_search', 'create_file'],
    off: [],
    onToolsOff: vi.fn(),
    imageModels: ['gpt-image-2', 'my/xai/grok-imagine-image'],
    videoModels: ['grok-imagine-video'],
    musicModels: [],
    billedTo: 'Household',
    onOpenSettings: vi.fn(),
    ...over,
  };
  return { props, ...wrap(<ChatTray {...props} />) };
}

function mockSettings(initial: Record<string, string>) {
  let doc = { instructions: '', default_model: '', image_model: '', video_model: '', music_model: '', ...initial };
  const puts: unknown[] = [];
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (_input, init) => {
    if (init?.method === 'PUT') {
      const body = JSON.parse(String(init.body)) as Record<string, string>;
      puts.push(body);
      doc = { ...doc, ...body };
    }
    return new Response(JSON.stringify(doc), { status: 200, headers: { 'Content-Type': 'application/json' } });
  });
  return puts;
}

describe('ChatTray', () => {
  it('saves the person’s own models as their defaults', async () => {
    const puts = mockSettings({ video_model: 'grok-imagine-video' });
    const onImageDefault = vi.fn();
    tray({ onImageDefault });
    const video = (await screen.findByLabelText('Your video model')) as HTMLSelectElement;
    await waitFor(() => expect(video.value).toBe('grok-imagine-video'));
    // No music models: no music picker.
    expect(screen.queryByLabelText('Your music model')).toBeNull();
    const image = screen.getByLabelText('Your images model') as HTMLSelectElement;
    expect(within(image).getByRole('option', { name: 'grok-imagine-image · your plan' })).toBeTruthy();
    fireEvent.change(image, { target: { value: 'my/xai/grok-imagine-image' } });
    await waitFor(() => expect(puts).toContainEqual({ image_model: 'my/xai/grok-imagine-image' }));
    expect(onImageDefault).toHaveBeenCalledWith('my/xai/grok-imagine-image');
    expect(await screen.findByText('Saved')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Your chat model'), { target: { value: 'b' } });
    await waitFor(() => expect(puts).toContainEqual({ default_model: 'b' }));
  });

  it('lists only this model’s tools, per chat, and the modes for the next message', async () => {
    mockSettings({});
    const { props } = tray({ off: ['create_file'] });
    const group = screen.getByRole('group', { name: 'Tools' });
    expect(within(group).queryByRole('switch', { name: 'X search' })).toBeNull();
    fireEvent.click(within(group).getByRole('switch', { name: 'Create files' }));
    expect(props.onToolsOff).toHaveBeenCalledWith([]);
    fireEvent.click(screen.getByRole('switch', { name: 'Deep research' }));
    expect(props.onResearch).toHaveBeenCalledWith(true);
    expect(props.onCompare).toHaveBeenCalledWith([]);
    // The bar model is in the list too; the first pick brings it along as
    // the preferred answer (it can be unchecked).
    expect(screen.getByRole('checkbox', { name: 'Compare with Model A' })).toBeTruthy();
    fireEvent.click(screen.getByRole('checkbox', { name: 'Compare with Model B' }));
    expect(props.onCompare).toHaveBeenLastCalledWith(['a', 'b']);
    expect(props.onResearch).toHaveBeenLastCalledWith(false);
    expect(screen.getByLabelText('Billed to').textContent).toContain('Household');
  });

  it('allows three models at most, and marks the first as preferred', () => {
    mockSettings({});
    const { props } = tray({ compare: ['c', 'b', 'd'] });
    expect((screen.getByRole('checkbox', { name: 'Compare with Model A' }) as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByRole('checkbox', { name: 'Compare with Model B' }) as HTMLInputElement).disabled).toBe(false);
    const preferred = screen.getByText('Preferred').closest('label')!;
    expect(preferred.textContent).toContain('Model C');
    expect(screen.getByText('3 of 3 picked.')).toBeTruthy();
    // Unchecking the bar model leaves only the picked ones.
    fireEvent.click(screen.getByRole('checkbox', { name: 'Compare with Model C' }));
    expect(props.onCompare).toHaveBeenLastCalledWith(['b', 'd']);
  });
  it('locks the modes while a reply runs', () => {
    mockSettings({});
    tray({ busy: true });
    expect((screen.getByRole('switch', { name: 'Deep research' }) as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByText('Wait for the reply to finish to change these.')).toBeTruthy();
  });
});

describe('composer bar', () => {
  it('one Tools button names what is on and counts modes', () => {
    const open = vi.fn();
    render(<TrayButton onOpen={open} toolsOn={['Web search', 'Create files']} modes={1} />);
    const b = screen.getByRole('button', { name: 'Tools: Web search, Create files' });
    expect(b.textContent).toContain('3');
    fireEvent.click(b);
    expect(open).toHaveBeenCalled();
  });
  it('chips show the modes that are on and turn them off', () => {
    const r = vi.fn();
    const c = vi.fn();
    render(
      <ModeChips
        research
        compare={['b']}
        effortLabel="High"
        labelFor={(v) => (v === 'b' ? 'Model B' : v)}
        onResearchOff={r}
        onCompareOff={c}
        onEffort={() => undefined}
      />,
    );
    expect(screen.getByText(/pick one more besides Model B/)).toBeTruthy();
    expect(screen.getByRole('button', { name: /Thinking: High/ })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Turn off deep research' }));
    fireEvent.click(screen.getByRole('button', { name: 'Stop comparing' }));
    expect(r).toHaveBeenCalled();
    expect(c).toHaveBeenCalled();
  });
});

describe('Child Safety media note', () => {
  it('says which media Child Safety keeps from a child, and nothing for others', () => {
    mockSettings({});
    tray({ childMediaOff: ['image', 'transcription'] });
    expect(screen.getByRole('note').textContent).toBe(
      'Child Safety is on for your account, so making pictures and sound and video files are not available here.',
    );
    expect(childMediaNote(['music'])).toBe('Child Safety is on for your account, so making music is not available here.');
    expect(childMediaNote([])).toBe('');
  });
});
