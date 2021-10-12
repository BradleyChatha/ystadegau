package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	runtime "github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/golang-migrate/migrate/v4"

	_ "github.com/lib/pq"

	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

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

var host string
var user string
var pass string

func init() {
	log.Print("Init")
	sesh := ssm.New(session.New())
	log.Print("Made session")
	ssmhost, err := sesh.GetParameter(&ssm.GetParameterInput{
		Name: aws.String("db_url"),
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Print("Got db_url")
	ssmuser, err := sesh.GetParameter(&ssm.GetParameterInput{
		Name: aws.String("db_lambda_user"),
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Print("Got db_lambda_user")
	ssmpass, err := sesh.GetParameter(&ssm.GetParameterInput{
		Name:           aws.String("db_lambda_pass"),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Print("Got db_lambda_pass")

	host = *ssmhost.Parameter.Value
	user = *ssmuser.Parameter.Value
	pass = *ssmpass.Parameter.Value
	log.Print("Finish init")
}

func main() {
	runtime.Start(handleRequest)
}

func handleRequest(ctx context.Context, event interface{}) (string, error) {
	log.Print("handleRequest")
	db := os.Getenv("DB_DB")
	ssl := os.Getenv("DB_SSL")

	if db == "" {
		db = "dubstats"
	} else if ssl == "" {
		ssl = "require"
	}

	log.Print("Got env vars")
	connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s", user, pass, host, db, ssl)
	m, err := migrate.New("file://ymfudiadau/", connStr)
	if err != nil {
		return "", err
	}
	log.Print("Created migrator")

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		return "", err
	}
	log.Print("Migrated")

	return "Success", nil
}
