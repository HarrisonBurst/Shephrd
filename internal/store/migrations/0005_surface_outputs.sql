CREATE TABLE surface_outputs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    surface TEXT NOT NULL,
    thread TEXT NOT NULL,
    input INTEGER UNIQUE REFERENCES surface_inputs(id),
    inbox_item INTEGER UNIQUE REFERENCES inbox_items(id),
    turn INTEGER UNIQUE,
    body TEXT NOT NULL,
    time TEXT NOT NULL,
    CHECK ((input IS NOT NULL AND inbox_item IS NULL AND turn IS NULL) OR (input IS NULL AND inbox_item IS NOT NULL AND turn IS NOT NULL))
);
INSERT INTO surface_outputs (id, surface, thread, input, body, time)
    SELECT r.id, i.surface, i.thread, r.input, r.body, r.time
    FROM surface_replies r JOIN surface_inputs i ON i.id = r.input;
DROP TABLE surface_replies;
ALTER TABLE surface_outputs RENAME TO surface_replies;
CREATE INDEX surface_replies_scope ON surface_replies(surface, thread, id);
CREATE TABLE routed_inbox_outputs (
    turn INTEGER PRIMARY KEY,
    inbox_item INTEGER NOT NULL UNIQUE REFERENCES inbox_items(id),
    surface TEXT NOT NULL,
    thread TEXT NOT NULL,
    digest BLOB NOT NULL
);
CREATE TABLE surface_cursors (
    surface TEXT NOT NULL,
    thread TEXT NOT NULL,
    cursor INTEGER NOT NULL,
    PRIMARY KEY (surface, thread)
);
ALTER TABLE main_coordinator ADD COLUMN route_surface TEXT NOT NULL DEFAULT '';
ALTER TABLE main_coordinator ADD COLUMN route_thread TEXT NOT NULL DEFAULT '';
