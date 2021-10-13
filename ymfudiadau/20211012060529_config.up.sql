CREATE TABLE config(
    id                          INTEGER PRIMARY KEY,
    next_package_list_update    TIMESTAMP WITH TIME ZONE NOT NULL
);
INSERT INTO config(id, next_package_list_update) VALUES(1, now());