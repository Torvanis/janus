import { useState, type ReactNode } from 'react';

/** What the model asked the person (ask_user). */
export interface ChatQuestion {
  questions: {
    question: string;
    options: { label: string; description?: string }[];
    multi_select?: boolean;
    allow_other?: boolean;
  }[];
}

/**
 * The question card under a reply that asked something. Picking choices and
 * sending posts the answer as the next message; the model reads it as the
 * answer (it sees its own question in the history). Shown on the latest
 * reply only; typing a normal message answers it too.
 */
export function QuestionCard({
  question,
  active,
  onAnswer,
}: {
  question?: ChatQuestion;
  /** The reply is the newest on the branch and nothing is running. */
  active: boolean;
  onAnswer: (text: string) => void;
}): ReactNode {
  const qs = question?.questions ?? [];
  const [picked, setPicked] = useState<string[][]>(() => qs.map(() => []));
  const [other, setOther] = useState<string[]>(() => qs.map(() => ''));
  if (qs.length === 0) return null;

  const toggle = (qi: number, label: string, multi: boolean) =>
    setPicked((p) =>
      p.map((cur, i) => {
        if (i !== qi) return cur;
        if (multi) return cur.includes(label) ? cur.filter((x) => x !== label) : [...cur, label];
        return cur[0] === label ? [] : [label];
      }),
    );
  const answerOf = (qi: number) => {
    const typed = other[qi]?.trim();
    return [...(picked[qi] ?? []), ...(typed ? [typed] : [])];
  };
  const complete = qs.every((_, i) => answerOf(i).length > 0);
  const submit = () => {
    if (!complete) return;
    const text =
      qs.length === 1
        ? answerOf(0).join(', ')
        : qs.map((q, i) => `${q.question.replace(/\?$/, '')}: ${answerOf(i).join(', ')}`).join('\n');
    onAnswer(text);
  };

  return (
    <section className="chat-question" aria-label="The model asks">
      {qs.map((q, qi) => {
        const name = `chat-q-${qi}`;
        return (
          <fieldset key={qi} className="chat-question-item" disabled={!active}>
            <legend className="chat-question-text">{q.question}</legend>
            {q.multi_select ? <span className="chat-question-hint">Pick any</span> : null}
            <div className="chat-question-options" role={q.multi_select ? 'group' : 'radiogroup'} aria-label={q.question}>
              {q.options.map((o, oi) => {
                const on = picked[qi]?.includes(o.label) ?? false;
                return (
                  <button
                    key={o.label}
                    type="button"
                    role={q.multi_select ? 'checkbox' : 'radio'}
                    aria-checked={on}
                    name={name}
                    disabled={!active}
                    className={`chat-question-option${on ? ' chat-question-option-on' : ''}`}
                    onClick={() => toggle(qi, o.label, !!q.multi_select)}
                  >
                    <span className="chat-question-label">
                      {o.label}
                      {oi === 0 ? <span className="chat-question-rec">Recommended</span> : null}
                    </span>
                    {o.description ? <span className="chat-question-desc">{o.description}</span> : null}
                  </button>
                );
              })}
            </div>
            {q.allow_other ? (
              <input
                disabled={!active}
                className="input chat-question-other"
                aria-label={`Your own answer: ${q.question}`}
                placeholder="Or type your own answer"
                value={other[qi] ?? ''}
                onChange={(e) => setOther((p) => p.map((v, i) => (i === qi ? e.target.value : v)))}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') {
                    e.preventDefault();
                    submit();
                  }
                }}
              />
            ) : null}
          </fieldset>
        );
      })}
      {active ? (
        <div className="chat-question-actions">
          <button type="button" className="btn btn-primary btn-sm" disabled={!complete} onClick={submit}>
            Send answer
          </button>
          <span className="small muted">Or type a reply below.</span>
        </div>
      ) : null}
    </section>
  );
}
