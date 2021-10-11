docker run -v $PWD/ymfudiadau:/migrations --network host migrate/migrate create -ext sql -dir /migrations $1 
sudo chown -hR $USER:$USER ./ymfudiadau