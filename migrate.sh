docker run -v $PWD/ymfudiadau:/migrations --network host migrate/migrate -path=/migrations -database $1 up