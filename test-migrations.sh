docker rm --force ystadegau-postgres
docker run -d --name ystadegau-postgres --rm --network host -e POSTGRES_PASSWORD=test -e POSTGRES_DB=test postgres
sleep 4
sh ./migrate.sh postgres://postgres:test@127.0.0.1:5432/test?sslmode=disable