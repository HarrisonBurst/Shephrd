ALTER TABLE attempts ADD COLUMN terminal_socket_path TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN terminal_window_id TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN terminal_workspace_id TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN terminal_tab_id TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN terminal_pane_id TEXT NOT NULL DEFAULT '';
ALTER TABLE attempts ADD COLUMN terminal_surface_id TEXT NOT NULL DEFAULT '';

UPDATE attempts
SET terminal_socket_path = herdr_socket_path,
    terminal_workspace_id = herdr_workspace_id,
    terminal_tab_id = herdr_tab_id,
    terminal_pane_id = herdr_pane_id
WHERE runtime_backend = 'herdr';
