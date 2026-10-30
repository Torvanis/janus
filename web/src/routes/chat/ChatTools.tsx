/**
 * Chat tools in the thread and the composer:
 *
 * - SearchActivity: "Searching the web for …" while the provider searches,
 *   then a quiet "Searched the web · 3 searches" line (expandable).
 * - SourcesList: the pages the reply cited, as small cards with the site.
 * - ToolsMenu: the composer's "Tools" control — which tools this chat's
 *   model has (on by default) with a switch each; turned-off tools are
 *   remembered per chat (tools_off on the conversation).
 */
import { useState, type ReactNode } from 'react';
import type { ChatActivity, ChatSource } from './ChatPage';

const ACTIVITY_LABEL: Record<string, { doing: string; did: string }> = {
  web_search: { doing: 'Searching the web', did: 'Searched the web' },
  x_search: { doing: 'Searching X', did: 'Searched X' },
  create_image: { doing: 'Making an image', did: 'Made an image' },
  create_file: { doing: 'Writing a file', did: 'Made a file' },
  create_video: { doing: 'Making the video', did: 'Made a video' },
  create_music: { doing: 'Making the music', did: 'Made music' },
  remember: { doing: 'Saving to memory', did: 'Saved to memory' },
  forget: { doing: 'Forgetting', did: 'Updated memory' },
  web_fetch: { doing: 'Reading the page', did: 'Read a page' },
  code_execution: { doing: 'Running code', did: 'Ran code' },
  transcribe: { doing: 'Transcribing', did: 'Transcribed' },
  watch: { doing: 'Watching the video', did: 'Watched the video' },
  // A page, search results or file text kept from the model: it carried
  // instructions aimed at the assistant or was not suitable here.
  withheld: { doing: 'Withholding', did: 'Withheld something it read' },
};

const KIND: Record<string, string> = {
  x_search: 'X',
  create_image: 'Image',
  web_fetch: 'Page',
  code_execution: 'Code',
  create_file: 'File',
  create_video: 'Video',
  create_music: 'Music',
  remember: 'Memory',
  forget: 'Memory',
  transcribe: 'Sound',
  watch: 'Video',
  withheld: 'Withheld',
};

/** Long-running tools: say how long they take while they run. */
const WAIT: Record<string, string> = {
  create_image: ' — this can take up to a minute',
  create_video: ' — videos take one to a few minutes',
  create_music: ' — about half a minute',
};

function label(type: string, done: boolean): string {
  const l = ACTIVITY_LABEL[type];
  if (l) return done ? l.did : l.doing;
  return done ? 'Searched' : 'Searching';
}

export function SearchActivity({ activity, streaming }: { activity?: ChatActivity[]; streaming: boolean }): ReactNode {
  if (!activity || activity.length === 0) return null;
  const running = streaming ? activity.find((a) => a.status === 'searching') : undefined;
  const last = [...activity].reverse().find((a) => a.query);
  if (running || (streaming && activity.every((a) => a.status === 'done') && !last)) {
    const a = running ?? activity[activity.length - 1]!;
    return (
      <div className="chat-activity chat-activity-live" role="status" aria-live="polite">
        <span className="chat-activity-spinner" aria-hidden="true" />
        <span>
          {label(a.type, false)}
          {last?.query && !WAIT[a.type] && a.type !== 'create_file' ? (
            <span className="chat-activity-query"> for “{last.query}”</span>
          ) : null}
          {a.type === 'create_file' && a.query ? <span className="chat-activity-query"> {a.query}</span> : null}
          {WAIT[a.type] ? <span className="chat-activity-query">{WAIT[a.type]}</span> : null}…
        </span>
      </div>
    );
  }
  const searches = activity.filter((a) => a.type === 'web_search' || a.type === 'x_search');
  const runs = activity.filter((a) => a.type === 'code_execution' && (a.code || a.output));
  const images = activity.filter((a) => a.type === 'create_image').length;
  const types = [...new Set(activity.map((a) => a.type))];
  const summary = types.map((t) => label(t, true)).join(' and ');
  const queries = activity.filter((a) => a.query && a.type !== 'code_execution');
  return (
    <details
      className="chat-activity"
      data-kind={
        types.every((t) => t === 'code_execution')
          ? 'code'
          : types.every((t) => t === 'create_image' || t === 'create_video' || t === 'create_music')
            ? 'image'
            : types.every((t) => t === 'create_file' || t === 'remember' || t === 'forget')
              ? 'file'
              : 'search'
      }
    >
      <summary>
        {summary}
        <span className="chat-activity-count">
          {searches.length > 1 ? ` · ${searches.length} searches` : ''}
          {images > 1 ? ` · ${images} images` : ''}
        </span>
      </summary>
      {runs.map((a, i) => (
        <div key={`run-${i}`} className="chat-code-run">
          {a.code ? (
            <pre className="chat-code-run-src" aria-label="Code the model ran">
              <code>{a.code}</code>
            </pre>
          ) : null}
          {a.output ? (
            <pre className="chat-code-run-out" aria-label="Output">
              <code>{a.output}</code>
            </pre>
          ) : null}
        </div>
      ))}
      {queries.length > 0 ? (
        <ul className="chat-activity-list">
          {queries.map((a, i) => (
            <li key={i}>
              <span className="chat-activity-kind">{KIND[a.type] ?? 'Web'}</span> {a.query}
            </li>
          ))}
        </ul>
      ) : null}
    </details>
  );
}

export function SourcesList({ sources }: { sources?: ChatSource[] }): ReactNode {
  const [all, setAll] = useState(false);
  if (!sources || sources.length === 0) return null;
  const shown = all ? sources : sources.slice(0, 4);
  return (
    <div className="chat-sources" aria-label="Sources">
      <div className="chat-sources-title">Sources</div>
      <ol className="chat-sources-list">
        {shown.map((s, i) => (
          <li key={s.url}>
            <a href={s.url} target="_blank" rel="noreferrer noopener" className="chat-source" title={s.url}>
              <span className="chat-source-n">{i + 1}</span>
              <span className="chat-source-text">
                <span className="chat-source-title">{s.title || s.host || s.url}</span>
                {s.title && s.host ? <span className="chat-source-host">{s.host}</span> : null}
              </span>
            </a>
          </li>
        ))}
      </ol>
      {sources.length > 4 ? (
        <button type="button" className="chat-act chat-sources-more" onClick={() => setAll((v) => !v)}>
          {all ? 'Show fewer' : `Show all ${sources.length}`}
        </button>
      ) : null}
    </div>
  );
}

export interface ToolInfo {
  id: string;
  label: string;
  description: string;
}
