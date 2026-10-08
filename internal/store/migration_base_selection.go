package store

const baseSelectionSchema = `ALTER TABLE attempts ADD COLUMN base_strategy TEXT NOT NULL DEFAULT 'default_branch';
ALTER TABLE attempts ADD COLUMN base_ref TEXT NOT NULL DEFAULT '';`
