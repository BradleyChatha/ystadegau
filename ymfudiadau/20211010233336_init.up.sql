CREATE TABLE package(
    id              INTEGER PRIMARY KEY,
    name            VARCHAR(256) NOT NULL,
    query_vector    TSVECTOR,
    next_update     TIMESTAMP WITH TIME ZONE
);
CREATE INDEX ON package(name);
CREATE INDEX ON package USING gin(query_vector);

CREATE TABLE package_version(
    id          INTEGER PRIMARY KEY,
    package_id  INTEGER NOT NULL,
    semver      VARCHAR(128) NOT NULL,

    CONSTRAINT fk_package_version_package_id FOREIGN KEY(package_id) REFERENCES package(id)
);

CREATE TABLE package_snapshot(
    id                  INTEGER PRIMARY KEY,
    package_version_id  INTEGER NOT NULL,
    time                TIMESTAMP WITH TIME ZONE NOT NULL,
    downloads_weekly    INTEGER NOT NULL,
    downloads_monthly   INTEGER NOT NULL,
    downloads_total     BIGINT NOT NULL,
    stars               INTEGER NOT NULL,
    watchers            INTEGER NOT NULL,
    issues              INTEGER NOT NULL,
    forks               INTEGER NOT NULL,

    CONSTRAINT fk_package_snapshot_package_version_id FOREIGN KEY(package_version_id) REFERENCES package_version(id)
);

CREATE FUNCTION update_package_query_vector(in pid int, in description text, in readme text) RETURNS void
AS $$
    UPDATE package SET query_vector = (to_tsvector(name) || to_tsvector(readme) || to_tsvector(description)) WHERE id = pid;
$$
LANGUAGE SQL;

CREATE FUNCTION bump_package_update_time(in pid int) RETURNS void
AS $$
    UPDATE package SET next_update = now() + interval '1 week' WHERE id = pid;
$$
LANGUAGE SQL;

CREATE FUNCTION search_packages(in query text) RETURNS TABLE(id int, name text, rank int)
AS $$
    SELECT id, name, ts_rank_cd(query_vector, to_tsquery(query)) AS rank 
    FROM package 
    WHERE query_vector @@ to_tsquery(query)
    ORDER BY rank;
$$
LANGUAGE SQL;