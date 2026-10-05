-- The board's single source of truth. Consul holds only what must vanish when
-- a worker dies (claims, semaphores); everything that lasts lives here.

CREATE TABLE IF NOT EXISTS tasks (
  id            INTEGER PRIMARY KEY,
  title         TEXT    NOT NULL,
  body          TEXT    NOT NULL DEFAULT '',
  repo          TEXT    NOT NULL DEFAULT '',
  base_rev      TEXT    NOT NULL DEFAULT '',
  state         TEXT    NOT NULL DEFAULT 'queued',
  priority      INTEGER NOT NULL DEFAULT 0,
  labels_json   TEXT    NOT NULL DEFAULT '{}',
  assignee      TEXT    NOT NULL DEFAULT '',
  result_branch TEXT    NOT NULL DEFAULT '',
  parent_id     INTEGER REFERENCES tasks(id),
  created_by    TEXT    NOT NULL DEFAULT '',
  created_at    TEXT    NOT NULL,
  updated_at    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS tasks_state  ON tasks(state, priority DESC, id);
CREATE INDEX IF NOT EXISTS tasks_parent ON tasks(parent_id);

CREATE TABLE IF NOT EXISTS comments (
  id         INTEGER PRIMARY KEY,
  task_id    INTEGER NOT NULL REFERENCES tasks(id),
  author     TEXT    NOT NULL,
  body       TEXT    NOT NULL,
  created_at TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS comments_task ON comments(task_id, id);

-- Full-text search over comments, kept in step by triggers.
CREATE VIRTUAL TABLE IF NOT EXISTS comments_fts USING fts5(body, content='comments', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS comments_ai AFTER INSERT ON comments BEGIN
  INSERT INTO comments_fts(rowid, body) VALUES (new.id, new.body);
END;

CREATE TABLE IF NOT EXISTS events (
  id        INTEGER PRIMARY KEY,
  task_id   INTEGER NOT NULL REFERENCES tasks(id),
  kind      TEXT    NOT NULL,   -- created, state, assigned, comment, usage, ...
  actor     TEXT    NOT NULL,
  data_json TEXT    NOT NULL DEFAULT '{}',
  at        TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS events_task ON events(task_id, id);

CREATE TABLE IF NOT EXISTS usage (
  id          INTEGER PRIMARY KEY,
  task_id     INTEGER NOT NULL REFERENCES tasks(id),
  runtime     TEXT    NOT NULL,   -- agent loop: pi, claude-code
  backend     TEXT    NOT NULL,   -- glm, anthropic, ...
  model       TEXT    NOT NULL,
  input       INTEGER NOT NULL DEFAULT 0,
  cache_read  INTEGER NOT NULL DEFAULT 0,
  cache_write INTEGER NOT NULL DEFAULT 0,
  output      INTEGER NOT NULL DEFAULT 0,
  wall_s      REAL    NOT NULL DEFAULT 0,
  at          TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_task ON usage(task_id);
