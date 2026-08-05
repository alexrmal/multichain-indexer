CREATE TABLE IF NOT EXISTS blocks (
    chain_id    BIGINT NOT NULL,
    number      BIGINT NOT NULL,
    hash        TEXT NOT NULL,
    parent_hash TEXT NOT NULL,
    "timestamp" TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (chain_id, number)
);

CREATE TABLE IF NOT EXISTS transactions (
    chain_id     BIGINT NOT NULL,
    hash         TEXT NOT NULL,
    block_number BIGINT NOT NULL,
    from_address TEXT NOT NULL,
    to_address   TEXT,
    value_wei    TEXT NOT NULL,
    PRIMARY KEY (chain_id, hash),
    FOREIGN KEY (chain_id, block_number)
        REFERENCES blocks (chain_id, number) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_tx_block ON transactions (chain_id, block_number);

CREATE TABLE IF NOT EXISTS balances (
    chain_id         BIGINT NOT NULL,
    address          TEXT NOT NULL,
    balance_wei      TEXT NOT NULL,
    updated_at_block BIGINT NOT NULL,
    PRIMARY KEY (chain_id, address)
);
