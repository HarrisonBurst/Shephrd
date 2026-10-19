CREATE TABLE surface_inputs (
    id INTEGER PRIMARY KEY,
    surface TEXT NOT NULL,
    thread TEXT NOT NULL,
    body TEXT NOT NULL,
    time TEXT NOT NULL,
    answered INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX surface_inputs_pending ON surface_inputs(answered, id);

CREATE TABLE surface_replies (
    id INTEGER PRIMARY KEY,
    input INTEGER NOT NULL UNIQUE REFERENCES surface_inputs(id),
    body TEXT NOT NULL,
    time TEXT NOT NULL
);
CREATE INDEX surface_replies_input ON surface_replies(input);
