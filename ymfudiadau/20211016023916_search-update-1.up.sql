CREATE OR REPLACE FUNCTION search_packages(in query text) RETURNS TABLE(id int, name text, rank real)
AS $$
    SELECT DISTINCT ON (id)
        id, name, SUM(rank) AS rank
    FROM
    (
        SELECT 
            id, name, 10 AS rank 
        FROM 
            package 
        WHERE 
            name = query
        UNION ALL
        (
            SELECT
                id, name, 1 AS rank
            FROM
                package
            WHERE
                name LIKE (query || '%')
                OR
                name LIKE ('%' || query)
        )
        UNION ALL
        (
            SELECT 
                id, name, ts_rank_cd(query_vector, to_tsquery(query)) AS rank 
            FROM 
                package 
            WHERE 
                query_vector @@ to_tsquery(query)
        )
    ) AS _
    GROUP BY id, name;
$$
LANGUAGE SQL;