-- Incident Memory: one structured snapshot per resolved incident, plus clearly
-- flagged simulated demo records. Snapshots outlive span retention.
CREATE TABLE incident_memory (
    id          BIGSERIAL   PRIMARY KEY,
    incident_id BIGINT      UNIQUE REFERENCES incidents (id) ON DELETE CASCADE,
    simulated   BOOLEAN     NOT NULL DEFAULT FALSE,
    key         TEXT        NOT NULL UNIQUE,
    record      JSONB       NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (simulated = (incident_id IS NULL))
);
