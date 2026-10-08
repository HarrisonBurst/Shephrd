ALTER TABLE tasks ADD COLUMN remote_delivery_state TEXT NOT NULL DEFAULT 'unverified';

ALTER TABLE attempts ADD COLUMN base_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN landing_kind TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN landed_source_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN landed_target_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN landed_target_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN landed_checkpoint_revision INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempts ADD COLUMN landed_verified_at TEXT;
ALTER TABLE attempts ADD COLUMN release_state TEXT NOT NULL DEFAULT 'held' CHECK(release_state IN ('held', 'releasing', 'released'));
ALTER TABLE attempts ADD COLUMN release_claimed_at TEXT;
ALTER TABLE attempts ADD COLUMN release_owner_pid INTEGER NOT NULL DEFAULT 0;
ALTER TABLE attempts ADD COLUMN release_reason TEXT NOT NULL DEFAULT '';

UPDATE attempts SET release_state='released' WHERE released_at IS NOT NULL;
