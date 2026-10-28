-- Stage 1: dev rooms, the versioned board, the transcript and the
-- whiteboard's session state. Migrations are append-only: never edit a
-- file once it has run anywhere; add the next number.

CREATE TABLE meetings (
    id          text PRIMARY KEY,                  -- stage 1: the room name
    title       text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now(),
    started_at  timestamptz NOT NULL DEFAULT now(), -- the meeting clock's zero
    settings    jsonb NOT NULL DEFAULT '{}'
);

-- One row per board version. Versions are immutable; the head is the
-- highest seq. A commit inserts seq = head + 1, so two writers racing for
-- the same seq collide on the primary key and the loser re-reads.
CREATE TABLE board_versions (
    meeting_id    text NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    seq           bigint NOT NULL,
    parent        bigint NOT NULL,
    requester     text NOT NULL DEFAULT '',
    command_id    text NOT NULL DEFAULT '',
    t_ms          bigint NOT NULL,                  -- ms since the meeting started
    doc           jsonb NOT NULL,
    layout        jsonb,
    restored_from bigint,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (meeting_id, seq),
    CHECK (seq = parent + 1)
);

-- The ops that made each version, in order, for the reveal and audit.
CREATE TABLE board_ops (
    meeting_id text NOT NULL,
    seq        bigint NOT NULL,
    idx        integer NOT NULL,
    op         jsonb NOT NULL,
    PRIMARY KEY (meeting_id, seq, idx),
    FOREIGN KEY (meeting_id, seq) REFERENCES board_versions (meeting_id, seq) ON DELETE CASCADE
);

CREATE TABLE transcript_segments (
    id         bigserial PRIMARY KEY,
    meeting_id text NOT NULL REFERENCES meetings (id) ON DELETE CASCADE,
    identity   text NOT NULL,
    speaker    text NOT NULL,
    start_ms   bigint NOT NULL,
    end_ms     bigint NOT NULL,
    text       text NOT NULL
);
CREATE INDEX transcript_segments_by_time ON transcript_segments (meeting_id, start_ms);

-- What the whiteboard needs to pick up where it left off after a restart.
CREATE TABLE agent_state (
    meeting_id    text PRIMARY KEY REFERENCES meetings (id) ON DELETE CASCADE,
    resume_handle text NOT NULL DEFAULT '',
    summary       text NOT NULL DEFAULT '',
    updated_at    timestamptz NOT NULL DEFAULT now()
);
