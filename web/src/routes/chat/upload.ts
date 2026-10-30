/**
 * Upload one Janus Chat attachment: multipart POST with the session cookie
 * and the CSRF header (lib/api.ts only speaks JSON). Kept in its own module
 * so page tests can replace the network call.
 */
import { ApiError, csrfHeaders, type ApiErrorPayload } from '../../lib/api';

export interface ChatAttachment {
  id: string;
  /** video / audio: attached by the person, or made for a reply by Janus's
   * tools (as file). */
  kind: 'image' | 'document' | 'file' | 'video' | 'audio';
  filename: string;
  mime: string;
  size_bytes: number;
  est_tokens: number;
  width?: number;
  height?: number;
  truncated?: boolean;
}

export async function uploadChatAttachment(file: File, signal?: AbortSignal): Promise<ChatAttachment> {
  const form = new FormData();
  form.append('file', file, file.name);
  let res: Response;
  try {
    res = await fetch('/api/v1/chat/attachments', {
      method: 'POST',
      body: form,
      credentials: 'same-origin',
      headers: { Accept: 'application/json', ...csrfHeaders() },
      signal,
    });
  } catch {
    throw new ApiError(0, { message: 'The upload did not reach the gateway.', code: 'network_error', type: 'server_error' });
  }
  const text = await res.text();
  let parsed: unknown;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    parsed = null;
  }
  if (!res.ok) {
    const envelope = (parsed as { error?: ApiErrorPayload } | null)?.error;
    throw new ApiError(
      res.status,
      envelope ?? { message: `The upload failed (HTTP ${res.status}).`, code: 'server_error', type: 'server_error' },
    );
  }
  return parsed as ChatAttachment;
}
