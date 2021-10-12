package main

import (
	"context"
	"fmt"
	"os"
	"time"

	runtime "github.com/aws/aws-lambda-go/lambda"
	"github.com/golang-migrate/migrate"

	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

var logger *zap.Logger

type Listing struct {
	Name       string
	Registered time.Time
}

type Downloads struct {
	Total   int `json:"total"`
	Monthly int `json:"monthly"`
	Weekly  int `json:"weekly"`
	Daily   int `json:"daily"`
}

type Repo struct {
	Stars    int `json:"stars"`
	Watchers int `json:"watchers"`
	Forks    int `json:"forks"`
	Issues   int `json:"issues"`
}

type PackageStats struct {
	Downloads Downloads `json:"downloads"`
	Repo      Repo      `json:"repo"`
	Score     float64
}

type PackageInfo struct {
	Version string `json:"version"`
	Readme  string `json:"readme"`
}

func main() {
	runtime.Start(handleRequest)
}

func handleRequest(ctx context.Context, event interface{}) (string, error) {
	host := os.Getenv("DB_HOST")
	port := os.Getenv("DB_PORT")
	user := os.Getenv("DB_USER")
	pass := os.Getenv("DB_PASS")
	db := os.Getenv("DB_DB")
	ssl := os.Getenv("DB_SSL")

	if host == "" {
		logger.Fatal("Missing DB_HOST env var")
	} else if port == "" {
		logger.Fatal("Missing DB_PORT env var")
	} else if user == "" {
		logger.Fatal("Missing DB_USER env var")
	} else if pass == "" {
		logger.Fatal("Missing DB_PASS env var")
	} else if db == "" {
		logger.Fatal("Missing DB_DB env var")
	} else if ssl == "" {
		ssl = "require"
	}

	connStr := fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s", user, pass, host, port, db, ssl)
	m, err := migrate.New("file://./ymfudiadau/", connStr)
	if err != nil {
		return "", err
	}

	err = m.Up()
	if err != nil {
		return "", err
	}

	return "Success", nil
}
