import { useState, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { api } from '../../lib/api';
import { ConfirmDialog, useToast } from '../../components/ui';

// Measuring capabilities for the whole catalog: whether a model is measured
// when it is enabled, and the action that measures every enabled chat model.

interface ProbeSetting {
  on_enable: boolean;
}

export function CapabilityProbeControls({ enabledChatModels }: { enabledChatModels: number }): ReactNode {
  const toast = useToast();
  const client = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const [onlyUnmeasured, setOnlyUnmeasured] = useState(true);
  const setting = useQuery({
    queryKey: ['admin', 'capability-probe'],
    queryFn: () => api.get<ProbeSetting>('/api/v1/admin/system/capability-probe'),
  });
  const save = useMutation({
    mutationFn: (on: boolean) => api.put<ProbeSetting>('/api/v1/admin/system/capability-probe', { on_enable: on }),
    onSuccess: (next) => {
      client.setQueryData(['admin', 'capability-probe'], next);
      toast(next.on_enable ? 'Models are measured when enabled' : 'Models are no longer measured when enabled');
    },
    onError: (failure: Error) => toast(failure.message, 'danger'),
  });
  const measureAll = useMutation({
    mutationFn: () =>
      api.post<{ started: number }>('/api/v1/admin/models/capabilities/probe', { only_unmeasured: onlyUnmeasured }),
    onSuccess: (result) => {
      setConfirming(false);
      void client.invalidateQueries({ queryKey: ['admin', 'models'] });
      toast(
        result.started > 0
          ? `Measuring ${result.started} model${result.started === 1 ? '' : 's'}, one after another`
          : 'Nothing to measure: every enabled chat model already has a measurement',
      );
    },
    onError: (failure: Error) => toast(failure.message, 'danger'),
  });
  // An older server has no such setting.
  if (!setting.data || typeof setting.data.on_enable !== 'boolean') return null;
  return (
    <div
      className="row wrap"
      style={{ gap: 'var(--janus-space-4)', alignItems: 'center' }}
      data-testid="capability-probe-controls"
    >
      <label className="row small" style={{ gap: 'var(--janus-space-2)', alignItems: 'center' }}>
        <input
          type="checkbox"
          checked={setting.data.on_enable}
          disabled={save.isPending}
          onChange={(event) => save.mutate(event.target.checked)}
        />
        Measure what a model can do when it is enabled
      </label>
      <button type="button" className="btn btn-sm" disabled={enabledChatModels === 0} onClick={() => setConfirming(true)}>
        Measure enabled models
      </button>
      <ConfirmDialog
        open={confirming}
        onClose={() => setConfirming(false)}
        onConfirm={() => measureAll.mutate()}
        title="Measure enabled models"
        consequence={`Janus sends each enabled chat model (${enabledChatModels} now) up to 30 small requests, one model after another, to find out what its provider does not say: thinking, images, sound, video, tools, refused settings. That is a few hundred tokens per model, recorded as platform usage.`}
        confirmLabel="Measure"
        danger={false}
        busy={measureAll.isPending}
      >
        <label className="row small" style={{ gap: 'var(--janus-space-2)', alignItems: 'center' }}>
          <input type="checkbox" checked={onlyUnmeasured} onChange={(event) => setOnlyUnmeasured(event.target.checked)} />
          Skip models that already have a measurement
        </label>
      </ConfirmDialog>
    </div>
  );
}
