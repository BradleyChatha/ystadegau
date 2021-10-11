cd ../..
sh test-migrations.sh
cd cmd/gwyliwr
export DB_HOST=localhost
export DB_PORT=5432
export DB_USER=postgres
export DB_PASS=test
export DB_DB=test
export DB_SSL=disable
export MODE=test
go run .