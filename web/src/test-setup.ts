import { configure } from '@testing-library/react';

// findBy*/waitFor give up after 1s by default. On shared CI runners, next to
// two race-enabled Go jobs, a chat composer menu took longer than that to
// appear (PR #249: "Unable to find role=listbox"), while the same test passes
// in well under a second alone. A missing element still fails, after 5s.
configure({ asyncUtilTimeout: 5_000 });
