ALTER TABLE attempts ADD COLUMN terminal_provider_version TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN terminal_protocol_version TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN terminal_capabilities_json TEXT NOT NULL DEFAULT '[]';
