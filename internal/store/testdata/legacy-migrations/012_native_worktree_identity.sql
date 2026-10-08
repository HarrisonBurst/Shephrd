-- Adds the explicit per-attempt workspace backend and canonical workspace
-- state machine required by the opt-in native Git worktree backend:
--
--   allocating -> held | no_workspace | unknown
--   held       -> releasing | unknown
--   releasing  -> released | held | unknown
--
-- release_state and released_at keep their existing meaning for API
-- compatibility; workspace_state is the canonical lifecycle and the two are
-- kept in agreement by every write path. Legacy rows are backfilled from
-- their recorded release evidence: any attempt with a Treehouse lease gets
-- backend 'treehouse', and no native identity is ever invented for them.

ALTER TABLE attempts ADD COLUMN workspace_backend TEXT NOT NULL DEFAULT ''
    CHECK(workspace_backend IN ('', 'treehouse', 'native_git_worktree'));
ALTER TABLE attempts ADD COLUMN workspace_state TEXT NOT NULL DEFAULT ''
    CHECK(workspace_state IN ('', 'allocating', 'held', 'no_workspace', 'unknown', 'releasing', 'released'));
ALTER TABLE attempts ADD COLUMN intended_worktree_path TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN worktree_git_dir TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN worktree_common_dir TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN workspace_state_changed_at TEXT;

UPDATE attempts SET workspace_backend='treehouse' WHERE lease_id <> '';

UPDATE attempts SET workspace_state = CASE
    WHEN released_at IS NOT NULL THEN 'released'
    WHEN release_state = 'no_workspace' THEN 'no_workspace'
    WHEN release_state = 'releasing' THEN 'releasing'
    WHEN status = 'unknown' THEN 'unknown'
    WHEN worktree_path <> '' OR lease_id <> '' THEN 'held'
    ELSE ''
END;
