-- +goose Up
CREATE TABLE shows (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    price_paise    BIGINT NOT NULL CHECK (price_paise >= 0),
    per_user_limit INT NOT NULL DEFAULT 4 CHECK (per_user_limit > 0),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE reservations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id         UUID NOT NULL REFERENCES shows(id),
    user_id         TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    seats           TEXT[] NOT NULL,
    amount_paise    BIGINT NOT NULL DEFAULT 0 CHECK (amount_paise >= 0),
    status          TEXT NOT NULL DEFAULT 'confirmed' CHECK (status IN ('confirmed', 'cancelled')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    cancelled_at    TIMESTAMPTZ,
    UNIQUE (user_id, idempotency_key)
);

CREATE TABLE seats (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id        UUID NOT NULL REFERENCES shows(id),
    seat_label     TEXT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'available' CHECK (status IN ('available', 'held', 'confirmed')),
    user_id        TEXT,
    reservation_id UUID REFERENCES reservations(id),
    UNIQUE (show_id, seat_label),
    -- A seat has an owner exactly when it isn't available. Enforced by the
    -- database so no code path can leave a sold seat ownerless or vice versa.
    CONSTRAINT seats_owner_matches_status CHECK (
        (status = 'available' AND user_id IS NULL AND reservation_id IS NULL)
        OR (status <> 'available' AND user_id IS NOT NULL AND reservation_id IS NOT NULL)
    )
);

-- Serves the per-user limit count: seats WHERE show_id = ? AND user_id = ?
CREATE INDEX seats_show_user_idx ON seats (show_id, user_id) WHERE user_id IS NOT NULL;

-- Lock-only rows (DESIGN.md §4 step 2): serialize one user's requests per show.
CREATE TABLE user_show_locks (
    show_id UUID NOT NULL REFERENCES shows(id),
    user_id TEXT NOT NULL,
    PRIMARY KEY (show_id, user_id)
);

-- +goose Down
DROP TABLE user_show_locks;
DROP TABLE seats;
DROP TABLE reservations;
DROP TABLE shows;
