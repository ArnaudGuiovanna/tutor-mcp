-- PostgreSQL schema for tutor-mcp (Phase 2).
-- Ported from the final migrated SQLite schema. Type mapping:
--   DATETIME / TIMESTAMP                -> TIMESTAMPTZ (UTC)
--   INTEGER PRIMARY KEY AUTOINCREMENT   -> BIGINT GENERATED ALWAYS AS IDENTITY
--   REAL                                -> DOUBLE PRECISION
--   INTEGER (booleans 0/1 + counts)     -> INTEGER (round-trips via boolToInt)
--   TEXT / JSON-as-text                 -> TEXT  (JSONB is a future optimization)
-- All statements are idempotent (IF NOT EXISTS) so apply-on-start is safe.

CREATE TABLE IF NOT EXISTS learners (
    id            TEXT PRIMARY KEY,
    email         TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    objective     TEXT NOT NULL,
    webhook_url   TEXT DEFAULT '',
    profile_json  TEXT DEFAULT '{}',
    created_at    TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    last_active   TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS oauth_clients (
    client_id          TEXT PRIMARY KEY,
    client_name        TEXT DEFAULT '',
    redirect_uris      TEXT DEFAULT '[]',
    client_secret_hash TEXT DEFAULT '',
    created_at         TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS refresh_tokens (
    token         TEXT PRIMARY KEY,
    learner_id    TEXT NOT NULL REFERENCES learners(id),
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    client_id     TEXT
);

CREATE TABLE IF NOT EXISTS domains (
    id                       TEXT PRIMARY KEY,
    learner_id               TEXT NOT NULL REFERENCES learners(id),
    name                     TEXT NOT NULL,
    personal_goal            TEXT DEFAULT '',
    graph_json               TEXT NOT NULL,
    value_framings_json      TEXT DEFAULT '',
    last_value_axis          TEXT DEFAULT '',
    archived                 INTEGER DEFAULT 0,
    graph_version            INTEGER NOT NULL DEFAULT 1,
    goal_relevance_json      TEXT NOT NULL DEFAULT '',
    goal_relevance_version   INTEGER NOT NULL DEFAULT 0,
    created_at               TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    phase                    TEXT,
    phase_changed_at         TIMESTAMPTZ,
    phase_entry_entropy      DOUBLE PRECISION,
    priority_rank            INTEGER
);

CREATE TABLE IF NOT EXISTS concept_states (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    concept        TEXT NOT NULL,
    stability      DOUBLE PRECISION DEFAULT 1.0,
    difficulty     DOUBLE PRECISION DEFAULT 0.3,
    elapsed_days   INTEGER DEFAULT 0,
    scheduled_days INTEGER DEFAULT 1,
    reps           INTEGER DEFAULT 0,
    lapses         INTEGER DEFAULT 0,
    card_state     TEXT DEFAULT 'new',
    last_review    TIMESTAMPTZ,
    next_review    TIMESTAMPTZ,
    p_mastery      DOUBLE PRECISION DEFAULT 0.1,
    p_learn        DOUBLE PRECISION DEFAULT 0.3,
    p_forget       DOUBLE PRECISION DEFAULT 0.05,
    p_slip         DOUBLE PRECISION DEFAULT 0.1,
    p_guess        DOUBLE PRECISION DEFAULT 0.2,
    theta          DOUBLE PRECISION DEFAULT 0.0,
    updated_at     TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(learner_id, concept)
);

CREATE TABLE IF NOT EXISTS interactions (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id    TEXT NOT NULL REFERENCES learners(id),
    concept       TEXT NOT NULL,
    activity_type TEXT NOT NULL,
    success       INTEGER NOT NULL,
    response_time INTEGER,
    confidence    DOUBLE PRECISION,
    error_type    TEXT DEFAULT '',
    notes         TEXT,
    hints_requested     INTEGER DEFAULT 0,
    self_initiated      INTEGER DEFAULT 0,
    calibration_id      TEXT DEFAULT '',
    is_proactive_review INTEGER DEFAULT 0,
    misconception_type   TEXT,
    misconception_detail TEXT,
    bkt_slip      DOUBLE PRECISION,
    bkt_guess     DOUBLE PRECISION,
    created_at    TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    domain_id     TEXT,
    rubric_json       TEXT,
    rubric_score_json TEXT
);

CREATE TABLE IF NOT EXISTS availability (
    learner_id     TEXT PRIMARY KEY REFERENCES learners(id),
    windows_json   TEXT DEFAULT '[]',
    avg_duration   INTEGER DEFAULT 30,
    sessions_week  INTEGER DEFAULT 3,
    do_not_disturb INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS scheduled_alerts (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id    TEXT NOT NULL REFERENCES learners(id),
    alert_type    TEXT NOT NULL,
    concept       TEXT DEFAULT '',
    scheduled_at  TIMESTAMPTZ NOT NULL,
    sent          INTEGER DEFAULT 0,
    created_at    TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS oauth_codes (
    code           TEXT PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    code_challenge TEXT NOT NULL,
    client_id      TEXT NOT NULL DEFAULT '',
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS learner_approved_clients (
    learner_id   TEXT NOT NULL REFERENCES learners(id),
    client_id    TEXT NOT NULL REFERENCES oauth_clients(client_id),
    redirect_uri TEXT NOT NULL,
    approved_at  TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (learner_id, client_id, redirect_uri)
);

CREATE TABLE IF NOT EXISTS affect_states (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id           TEXT NOT NULL REFERENCES learners(id),
    session_id           TEXT NOT NULL,
    energy               INTEGER DEFAULT 0,
    subject_confidence   INTEGER DEFAULT 0,
    satisfaction         INTEGER DEFAULT 0,
    perceived_difficulty INTEGER DEFAULT 0,
    next_session_intent  INTEGER DEFAULT 0,
    autonomy_score       DOUBLE PRECISION DEFAULT 0,
    created_at           TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(learner_id, session_id)
);

CREATE TABLE IF NOT EXISTS calibration_records (
    prediction_id TEXT PRIMARY KEY,
    learner_id    TEXT NOT NULL REFERENCES learners(id),
    concept_id    TEXT NOT NULL,
    predicted     DOUBLE PRECISION NOT NULL,
    actual        DOUBLE PRECISION,
    delta         DOUBLE PRECISION,
    created_at    TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS transfer_records (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id   TEXT NOT NULL REFERENCES learners(id),
    concept_id   TEXT NOT NULL,
    context_type TEXT NOT NULL,
    score        DOUBLE PRECISION NOT NULL,
    session_id   TEXT DEFAULT '',
    created_at   TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS implementation_intentions (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    domain_id      TEXT NOT NULL,
    trigger_text   TEXT NOT NULL,
    action_text    TEXT NOT NULL,
    honored        INTEGER,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    scheduled_for  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS webhook_message_queue (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id     TEXT NOT NULL REFERENCES learners(id),
    kind           TEXT NOT NULL,
    scheduled_for  TIMESTAMPTZ NOT NULL,
    expires_at     TIMESTAMPTZ,
    content        TEXT NOT NULL,
    priority       INTEGER DEFAULT 0,
    status         TEXT DEFAULT 'pending',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    sent_at        TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS pedagogical_snapshots (
    id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    interaction_id    BIGINT NOT NULL REFERENCES interactions(id),
    learner_id        TEXT NOT NULL REFERENCES learners(id),
    domain_id         TEXT NOT NULL,
    concept           TEXT NOT NULL,
    activity_type     TEXT NOT NULL,
    before_json       TEXT NOT NULL,
    observation_json  TEXT NOT NULL,
    after_json        TEXT NOT NULL,
    decision_json     TEXT NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
    interpretation_brief TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS pending_consolidations (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id     TEXT NOT NULL,
    period_type    TEXT NOT NULL CHECK (period_type IN ('monthly','quarterly','annual')),
    period_key     TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('pending','delivered','completed','failed')) DEFAULT 'pending',
    detected_at    TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
    delivered_at   TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    UNIQUE(learner_id, period_type, period_key)
);

CREATE TABLE IF NOT EXISTS webhook_push_log (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    learner_id           TEXT NOT NULL REFERENCES learners(id),
    queue_id             BIGINT DEFAULT 0,
    kind                 TEXT NOT NULL,
    domain_id            TEXT DEFAULT '',
    domain_name          TEXT DEFAULT '',
    concept              TEXT DEFAULT '',
    trigger_text         TEXT DEFAULT '',
    pedagogical_intent   TEXT DEFAULT '',
    learning_gain        TEXT DEFAULT '',
    open_loop            TEXT DEFAULT '',
    next_action          TEXT DEFAULT '',
    pushed_at            TIMESTAMPTZ NOT NULL,
    opened_session_at    TIMESTAMPTZ,
    concept_addressed    INTEGER DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    checksum   TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_learners_email_lower ON learners(lower(email));
CREATE INDEX IF NOT EXISTS idx_affect_states_learner ON affect_states(learner_id, created_at);
CREATE INDEX IF NOT EXISTS idx_calibration_records_learner ON calibration_records(learner_id, created_at);
CREATE INDEX IF NOT EXISTS idx_concept_states_learner ON concept_states(learner_id);
CREATE INDEX IF NOT EXISTS idx_concept_states_review ON concept_states(learner_id, next_review);
CREATE INDEX IF NOT EXISTS idx_impl_intent_learner ON implementation_intentions(learner_id, created_at);
CREATE INDEX IF NOT EXISTS idx_interactions_learner_concept ON interactions(learner_id, concept, created_at);
CREATE INDEX IF NOT EXISTS idx_interactions_learner_created ON interactions(learner_id, created_at);
CREATE INDEX IF NOT EXISTS idx_interactions_misconception ON interactions(learner_id, concept, misconception_type);
CREATE INDEX IF NOT EXISTS idx_interactions_self_initiated ON interactions(learner_id, self_initiated, created_at);
CREATE INDEX IF NOT EXISTS idx_oauth_codes_expires ON oauth_codes(expires_at);
CREATE INDEX IF NOT EXISTS idx_pedagogical_snapshots_domain_concept ON pedagogical_snapshots(learner_id, domain_id, concept, created_at);
CREATE INDEX IF NOT EXISTS idx_pedagogical_snapshots_learner_created ON pedagogical_snapshots(learner_id, created_at);
CREATE INDEX IF NOT EXISTS idx_pending_consolidations_learner_status ON pending_consolidations(learner_id, status);
CREATE INDEX IF NOT EXISTS idx_scheduled_alerts_learner_type ON scheduled_alerts(learner_id, alert_type, created_at);
CREATE INDEX IF NOT EXISTS idx_transfer_records_learner_concept ON transfer_records(learner_id, concept_id, created_at);
CREATE INDEX IF NOT EXISTS idx_webhook_push_log_open ON webhook_push_log(learner_id, domain_id, concept_addressed, pushed_at);
CREATE INDEX IF NOT EXISTS idx_wmq_dispatch ON webhook_message_queue(learner_id, kind, status, scheduled_for);
