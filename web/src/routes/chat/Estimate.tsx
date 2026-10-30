/**
 * Janus Chat: what the next message will roughly cost, shown in the
 * composer before sending, and the quota closest to its limit.
 *
 * The input estimate mirrors the server's context estimate (chars / 4 plus
 * a small per-message overhead, files at their extracted size): the next
 * request re-sends the conversation, so input = the last request's real
 * input + its reply + the new text and files. Output is not known in
 * advance; the estimate covers input only and says so.
 */
import type { ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { api } from '../../lib/api';
import type { QuotaStatus } from '../../lib/types';

const PER_MESSAGE = 4;

export function estimateInputTokens(opts: {
  lastIn: number;
  lastOut: number;
  draft: string;
  fileTokens: number;
  instructionChars: number;
  conversationStarted: boolean;
}): number {
  const draft = Math.ceil(opts.draft.length / 4) + PER_MESSAGE;
  // A new chat has no measured request yet: count its instructions.
  const base = opts.conversationStarted
    ? opts.lastIn + opts.lastOut
    : Math.ceil(opts.instructionChars / 4) + (opts.instructionChars ? PER_MESSAGE : 0);
  return Math.max(0, Math.round(base + draft + opts.fileTokens));
}

/** nano-USD per million tokens × tokens → USD. */
export function costUSD(tokens: number, rateNanoPerM: number): number {
  return (tokens * rateNanoPerM) / 1e6 / 1e9;
}

function money(usd: number): string {
  if (usd <= 0) return '$0';
  if (usd < 0.0001) return '<$0.0001';
  if (usd < 0.01) return `$${usd.toFixed(4)}`;
  return `$${usd.toFixed(3)}`;
}

/** The single most pressing quota: breached first, then the highest percent at risk. */
export function pressingQuota(quotas: QuotaStatus[]): QuotaStatus | null {
  const relevant = quotas.filter((q) => q.breached || q.at_risk);
  relevant.sort((a, b) => Number(b.breached) - Number(a.breached) || b.percent - a.percent);
  return relevant[0] ?? null;
}

export function useChatQuota() {
  return useQuery({
    queryKey: ['chat', 'quota'],
    queryFn: () => api.get<{ quotas: QuotaStatus[] }>('/api/v1/dashboard/quota'),
    staleTime: 30_000,
    retry: false,
  });
}

/**
 * The estimate line in the composer bar. Personal-plan models are paid by
 * the plan (no per-token price), so they show tokens only.
 */
export function SendEstimate({
  tokens,
  inRate,
  personal,
  contextWindow,
}: {
  tokens: number;
  inRate: number;
  personal: boolean;
  contextWindow: number;
}): ReactNode {
  if (tokens <= 0) return null;
  const over = contextWindow > 0 && tokens > contextWindow;
  const k = tokens >= 1000 ? `${(tokens / 1000).toFixed(tokens >= 10000 ? 0 : 1)}k` : String(tokens);
  let text = `~${k} tokens in`;
  let title = `About ${tokens.toLocaleString()} input tokens: the conversation so far plus this message and its files.`;
  if (!personal && inRate > 0) {
    text += ` · ~${money(costUSD(tokens, inRate))}`;
    title += ' Cost is for input only; the reply is billed on top at the model’s output rate.';
  } else if (personal) {
    title += ' Paid by your plan, not per token.';
  }
  if (over) title += ' This is more than the model’s context window, so the oldest messages will be left out.';
  return (
    <span className={over ? 'chat-estimate over' : 'chat-estimate'} title={title} aria-label={title}>
      {text}
    </span>
  );
}

/** A slim line above the composer when a quota is close to or past its limit. */
export function QuotaLine({ quota }: { quota: QuotaStatus | null }): ReactNode {
  if (!quota) return null;
  const pct = Math.round(quota.percent);
  const what = quota.metric_label || quota.metric;
  const scope = quota.subject_name ? ` for ${quota.subject_name}` : '';
  const when = quota.reset_at
    ? ` · resets ${new Date(quota.reset_at).toLocaleString(undefined, { dateStyle: 'medium', timeStyle: 'short' })}`
    : '';
  // Either way the next request is refused once a limit is reached;
  // hard_kill also cuts a reply that crosses it mid-stream.
  const midReply = quota.breach_behavior === 'hard_kill' ? ' A reply that crosses the limit is cut off.' : '';
  const text = quota.breached
    ? `${what}${scope} is used up (${quota.window_label || quota.window}): new messages are refused until it resets${when}`
    : `${what}${scope}: ${pct}% used (${quota.window_label || quota.window})${when}.${midReply}`;
  return (
    <p className={quota.breached ? 'chat-quota breached' : 'chat-quota'} role="status">
      {text}
    </p>
  );
}
