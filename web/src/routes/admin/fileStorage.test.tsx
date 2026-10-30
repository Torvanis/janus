/**
 * File storage card (Settings → General): database vs S3-compatible, the
 * write-only secret, the connection test, and the deployment lock.
 */
import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import type { ReactNode } from 'react';
import { api } from '../../lib/api';
import type { FileStorageDocument } from '../../lib/types';
import { ToastProvider } from '../../components/ui';
import { FileStorageCard } from './FileStorageCard';

vi.mock('../../lib/api', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../../lib/api')>();
  return { ...actual, api: { get: vi.fn(), post: vi.fn(), patch: vi.fn(), put: vi.fn(), del: vi.fn() } };
});
const mocked = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.resetAllMocks();
});

function wrap(node: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <ToastProvider>
        <MemoryRouter>{node}</MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>,
  );
}

function doc(over: Partial<FileStorageDocument> = {}): FileStorageDocument {
  return {
    mode: 'database',
    s3: {
      endpoint: '',
      bucket: '',
      region: 'us-east-1',
      path_style: false,
      prefix: '',
      access_key_id: '',
      secret_set: false,
      ca_bundle: '',
    },
    env_managed: [],
    max_object_bytes: 25 * 1024 * 1024,
    usage: { database: { objects: 3, bytes: 2048 } },
    backends: ['database', 's3'],
    ...over,
  };
}

const input = (label: string) => screen.getByLabelText(label, { selector: 'input' }) as HTMLInputElement;

describe('FileStorageCard', () => {
  it('shows database by default and reveals S3 fields when chosen', async () => {
    mocked.get.mockResolvedValue(doc());
    wrap(<FileStorageCard />);
    expect(await screen.findByRole('heading', { name: 'File storage' })).toBeTruthy();
    expect((screen.getByRole('radio', { name: 'Database' }) as HTMLInputElement).checked).toBe(true);
    expect(screen.queryByLabelText('Endpoint', { selector: 'input' })).toBeNull();
    expect(screen.getByText(/Existing files stay where they are/)).toBeTruthy();
    await userEvent.setup().click(screen.getByRole('radio', { name: 'S3-compatible' }));
    expect(input('Endpoint').className).toContain('input');
    expect(input('Secret access key').type).toBe('password');
  });

  it('tests the unsaved config, then saves it without echoing the secret back', async () => {
    mocked.get.mockResolvedValue(doc());
    mocked.post.mockResolvedValue({ ok: false, mode: 's3', error: 'put object: SignatureDoesNotMatch' });
    const saved = doc({ mode: 's3', s3: { ...doc().s3, endpoint: 'http://seaweed:8333', bucket: 'janus', secret_set: true } });
    mocked.put.mockResolvedValue(saved);
    const user = userEvent.setup();
    wrap(<FileStorageCard />);
    await user.click(await screen.findByRole('radio', { name: 'S3-compatible' }));
    await user.type(input('Endpoint'), 'http://seaweed:8333');
    await user.type(input('Bucket'), 'janus');
    await user.click(input('Path-style addressing'));
    await user.type(input('Access key ID'), 'AK');
    await user.type(input('Secret access key'), 'sk-1');
    await user.click(screen.getByRole('button', { name: 'Test connection' }));
    expect(await screen.findByText(/Connection failed: put object: SignatureDoesNotMatch/)).toBeTruthy();
    expect(mocked.post).toHaveBeenCalledWith('/api/v1/admin/storage/test', {
      mode: 's3',
      s3: {
        endpoint: 'http://seaweed:8333',
        bucket: 'janus',
        region: 'us-east-1',
        path_style: true,
        prefix: '',
        access_key_id: 'AK',
        secret_access_key: 'sk-1',
        ca_bundle: '',
      },
    });
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(mocked.put).toHaveBeenCalled());
    expect(await screen.findByText('File storage saved.')).toBeTruthy();
    await waitFor(() => expect(input('Secret access key').value).toBe(''));
    expect(input('Secret access key').placeholder).toBe('unchanged');
  });

  it('locks every field when the deployment sets the mode', async () => {
    mocked.get.mockResolvedValue(
      doc({
        mode: 's3',
        s3: { ...doc().s3, endpoint: 'http://rel-seaweedfs:8333', bucket: 'janus', secret_set: true },
        env_managed: ['mode', 'endpoint', 'bucket'],
      }),
    );
    mocked.post.mockResolvedValue({ ok: true, mode: 's3', latency_ms: 12 });
    wrap(<FileStorageCard />);
    expect(await screen.findByText('Set by the deployment')).toBeTruthy();
    expect(input('Endpoint').closest('fieldset')?.disabled).toBe(true);
    expect(screen.queryByRole('button', { name: 'Save' })).toBeNull();
    await userEvent.setup().click(screen.getByRole('button', { name: 'Test connection' }));
    expect(await screen.findByText(/Connection works/)).toBeTruthy();
  });
});
