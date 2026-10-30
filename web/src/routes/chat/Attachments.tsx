/**
 * Janus Chat attachments: the composer's file chips (upload state, token
 * estimate, remove) and the files shown on a sent message.
 *
 * Files upload as soon as they are picked, dropped or pasted, so the token
 * cost is known before sending. Documents are given to the model in full
 * with every message, which is why the estimate matters.
 */
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { api, ApiError } from '../../lib/api';
import { uploadChatAttachment, type ChatAttachment } from './upload';

export type { ChatAttachment } from './upload';

/** File types the picker offers (the server checks contents regardless). */
export const ATTACH_ACCEPT = [
  'image/png',
  'image/jpeg',
  'image/gif',
  'image/webp',
  'audio/*',
  'video/mp4',
  'video/quicktime',
  'video/webm',
  '.mp3',
  '.wav',
  '.m4a',
  '.ogg',
  '.flac',
  '.aac',
  '.opus',
  '.mp4',
  '.m4v',
  '.mov',
  '.webm',
  '.pdf',
  '.docx',
  '.txt',
  '.md',
  '.csv',
  '.tsv',
  '.json',
  '.jsonl',
  '.yaml',
  '.yml',
  '.xml',
  '.html',
  '.log',
  '.sql',
  '.go',
  '.py',
  '.js',
  '.ts',
  '.tsx',
  '.jsx',
  '.java',
  '.c',
  '.h',
  '.cpp',
  '.cs',
  '.rs',
  '.rb',
  '.php',
  '.sh',
  '.toml',
  '.ini',
].join(',');

export const MAX_ATTACHMENTS = 10;

export interface PendingAttachment {
  key: string;
  name: string;
  size: number;
  isImage: boolean;
  status: 'uploading' | 'ready' | 'error';
  error?: string;
  attachment?: ChatAttachment;
  /** Object URL of the local file, for image thumbnails. */
  previewUrl?: string;
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${Math.round(n / 1024)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function objectURL(file: File): string | undefined {
  return typeof URL.createObjectURL === 'function' && file.type.startsWith('image/') ? URL.createObjectURL(file) : undefined;
}

function revoke(url: string | undefined): void {
  if (url && typeof URL.revokeObjectURL === 'function') URL.revokeObjectURL(url);
}

let seq = 0;

/** Composer attachment state: add files (upload immediately), remove, clear. */
export function useComposerAttachments() {
  const [items, setItems] = useState<PendingAttachment[]>([]);
  const itemsRef = useRef(items);
  itemsRef.current = items;

  useEffect(
    () => () => {
      for (const it of itemsRef.current) revoke(it.previewUrl);
    },
    [],
  );

  const addFiles = useCallback((files: Iterable<File>) => {
    const list = Array.from(files);
    const room = MAX_ATTACHMENTS - itemsRef.current.length;
    const accepted = list.slice(0, Math.max(0, room));
    const fresh: PendingAttachment[] = accepted.map((f) => ({
      key: `att-${++seq}`,
      name: f.name || 'pasted image',
      size: f.size,
      isImage: f.type.startsWith('image/'),
      status: 'uploading',
      previewUrl: objectURL(f),
    }));
    const over: PendingAttachment[] = list.slice(accepted.length).map((f) => ({
      key: `att-${++seq}`,
      name: f.name || 'file',
      size: f.size,
      isImage: false,
      status: 'error',
      error: `At most ${MAX_ATTACHMENTS} files per message.`,
    }));
    setItems((prev) => [...prev, ...fresh, ...over]);
    accepted.forEach((file, i) => {
      const key = fresh[i]!.key;
      const named = file.name
        ? file
        : new File([file], `pasted-image.${(file.type.split('/')[1] ?? 'png').replace('jpeg', 'jpg')}`, { type: file.type });
      uploadChatAttachment(named)
        .then((att) =>
          setItems((prev) =>
            prev.map((it) => (it.key === key ? { ...it, status: 'ready', attachment: att, name: att.filename } : it)),
          ),
        )
        .catch((err: unknown) =>
          setItems((prev) =>
            prev.map((it) =>
              it.key === key
                ? { ...it, status: 'error', error: err instanceof ApiError ? err.message : 'The upload failed.' }
                : it,
            ),
          ),
        );
    });
  }, []);

  const remove = useCallback((key: string) => {
    const it = itemsRef.current.find((x) => x.key === key);
    if (!it) return;
    revoke(it.previewUrl);
    setItems((prev) => prev.filter((x) => x.key !== key));
    // An upload that was never sent is deleted right away; the server also
    // purges forgotten ones after a day.
    if (it.attachment) void api.del(`/api/v1/chat/attachments/${it.attachment.id}`).catch(() => undefined);
  }, []);

  /** Forget the chips after a send (the files now belong to the message). */
  const clear = useCallback(() => {
    for (const it of itemsRef.current) revoke(it.previewUrl);
    setItems([]);
  }, []);

  const ready = items.filter((it) => it.status === 'ready' && it.attachment);
  return {
    items,
    addFiles,
    remove,
    clear,
    uploading: items.some((it) => it.status === 'uploading'),
    hasErrors: items.some((it) => it.status === 'error'),
    readyIds: ready.map((it) => it.attachment!.id),
    tokens: ready.reduce((sum, it) => sum + (it.attachment?.est_tokens ?? 0), 0),
  };
}

/** The chips above the message box. */
export function AttachmentChips({
  items,
  onRemove,
  tokens,
  contextWindow,
}: {
  items: PendingAttachment[];
  onRemove: (key: string) => void;
  tokens: number;
  contextWindow: number;
}): ReactNode {
  if (items.length === 0) return null;
  const heavy = contextWindow > 0 && tokens > contextWindow * 0.8;
  return (
    <div className="chat-attachments" aria-label="Attached files">
      <ul className="chat-chips">
        {items.map((it) => (
          <li key={it.key} className={`chat-chip chat-chip-${it.status}`} data-status={it.status}>
            {it.isImage && it.previewUrl ? <img className="chat-chip-thumb" src={it.previewUrl} alt="" /> : null}
            <span className="chat-chip-text">
              <span className="chat-chip-name" title={it.name}>
                {it.name}
              </span>
              <span className="chat-chip-meta small muted">
                {formatBytes(it.size)}
                {it.status === 'uploading' ? ' · uploading…' : null}
                {it.status === 'ready' && it.attachment
                  ? ` · ~${it.attachment.est_tokens.toLocaleString()} tokens${it.attachment.truncated ? ' (text cut to fit)' : ''}`
                  : null}
              </span>
              {it.status === 'error' ? (
                <span className="chat-chip-reason small" role="alert">
                  {it.error}
                </span>
              ) : null}
            </span>
            <button type="button" className="chat-chip-remove" aria-label={`Remove ${it.name}`} onClick={() => onRemove(it.key)}>
              ✕
            </button>
          </li>
        ))}
      </ul>
      {tokens > 0 ? (
        <p className={heavy ? 'chat-attach-total small chat-error' : 'chat-attach-total small muted'}>
          About {tokens.toLocaleString()} tokens of files
          {heavy
            ? ` — that is over 80% of this model's ${contextWindow.toLocaleString()}-token context window, so little room is left for the conversation.`
            : ' are sent with every message in this chat.'}
        </p>
      ) : null}
    </div>
  );
}

/** Files on a sent message: image thumbnails (open full size), players for
 * sound and video, and document chips. */
export function MessageAttachments({ attachments }: { attachments?: ChatAttachment[] }): ReactNode {
  if (!attachments || attachments.length === 0) return null;
  return (
    <ul className="chat-msg-files" aria-label="Files">
      {attachments.map((a) => {
        const href = `/api/v1/chat/attachments/${a.id}/content`;
        return a.kind === 'image' ? (
          <li key={a.id}>
            <a href={href} target="_blank" rel="noopener noreferrer" className="chat-msg-image" title={a.filename}>
              <img src={href} alt={a.filename} loading="lazy" />
            </a>
          </li>
        ) : a.kind === 'video' ? (
          <li key={a.id} className="chat-msg-video">
            <video controls preload="metadata" src={href} aria-label={a.filename} />
          </li>
        ) : a.kind === 'audio' ? (
          <li key={a.id} className="chat-msg-audio">
            <span className="chat-chip-name" title={a.filename}>
              {a.filename}
            </span>
            <audio controls preload="metadata" src={href} aria-label={a.filename} />
          </li>
        ) : (
          <li key={a.id} className="chat-chip chat-chip-ready">
            <span className="chat-chip-text">
              <span className="chat-chip-name" title={a.filename}>
                {a.filename}
              </span>
              <span className="chat-chip-meta small muted">{formatBytes(a.size_bytes)}</span>
            </span>
          </li>
        );
      })}
    </ul>
  );
}
