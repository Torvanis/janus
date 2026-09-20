/** Attribution comes from the request record, never today's key affinity. */
export function ChargedTeam({ event }: { event: { team_ids?: string; team_names?: string[]; service_token_id?: string } }) {
  const ids = (event.team_ids ?? '').split(',').filter(Boolean);
  const label = ids.length
    ? event.team_names?.length
      ? event.team_names.join(', ')
      : 'Unavailable team'
    : event.service_token_id
      ? 'Service token'
      : 'Personal';
  return <span title={ids.length ? `Charged team: ${label}` : label}>{label}</span>;
}
