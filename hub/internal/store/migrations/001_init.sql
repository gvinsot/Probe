-- Hub state. Accounts and repositories are stored as the JSON documents the
-- file store writes, with the columns their lookups and ordering need.
-- Text compared or sorted like Go strings uses the "C" collation.

CREATE TABLE users (
    key  text PRIMARY KEY,
    data jsonb NOT NULL
);

CREATE TABLE repos (
    user_key  text NOT NULL,
    key       text NOT NULL,
    full_name text COLLATE "C" NOT NULL,
    data      jsonb NOT NULL,
    PRIMARY KEY (user_key, key)
);

-- Webhook (kind 'hooks') and badge (kind 'badges') routing keys.
CREATE TABLE routes (
    kind text NOT NULL,
    key  text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY (kind, key)
);

-- One analysis result per commit and variant. name is the file store's
-- record name (<commit>.json or <commit>.plan.json), which breaks ties in
-- the history order. raw is the confidence report exactly as the CLI wrote it.
CREATE TABLE runs (
    user_key    text NOT NULL,
    repo_key    text NOT NULL,
    name        text COLLATE "C" NOT NULL,
    commit_id   text COLLATE "C" NOT NULL,
    plan        boolean NOT NULL,
    status      text NOT NULL,
    queued_at   timestamptz NOT NULL,
    activity_at timestamptz,
    record      jsonb NOT NULL,
    raw         bytea,
    PRIMARY KEY (user_key, repo_key, name)
);

CREATE INDEX runs_history ON runs (user_key, repo_key, queued_at DESC, name);

-- Retention watermark of a repository history: the latest activity of an
-- evicted normal analysis, and whether an evicted one was pending or undated.
CREATE TABLE run_retention (
    user_key        text NOT NULL,
    repo_key        text NOT NULL,
    evicted_through timestamptz,
    incomplete      boolean NOT NULL DEFAULT false,
    PRIMARY KEY (user_key, repo_key)
);

-- Deployment facts, such as the completed import of a file store.
CREATE TABLE meta (
    key   text PRIMARY KEY,
    value text NOT NULL
);
