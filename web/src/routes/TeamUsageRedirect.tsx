import { Navigate, useLocation, useParams } from 'react-router-dom';

/** Keep saved usage links while routing every team workflow through its shared header. */
export function TeamUsageRedirect() {
  const { teamId } = useParams();
  const location = useLocation();
  const params = new URLSearchParams(location.search);
  params.set('view', 'manage');
  params.set('section', 'usage');
  return (
    <Navigate
      replace
      to={{ pathname: `/teams/${encodeURIComponent(teamId!)}`, search: params.toString(), hash: location.hash }}
      state={location.state}
    />
  );
}
