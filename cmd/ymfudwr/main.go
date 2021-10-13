package main

import (
	"fmt"
	"log"
	"os"
	"time"

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
	ses, err := session.NewSession(&aws.Config{
		Region: aws.String("eu-west-2"),
	})
	if err != nil {
		log.Fatal(err)
	}

	sesh := ssm.New(ses)
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
	ssmpass, err := sesh.GetParameter(&ssm.GetParameterInput{
		Name:           aws.String("db_lambda_pass"),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		log.Fatal(err)
	}

	host = *ssmhost.Parameter.Value
	user = *ssmuser.Parameter.Value
	pass = *ssmpass.Parameter.Value
}

func main() {
	err := run()
	if err != nil {
		log.Fatal(err)
	}
}

func run() error {
	db := os.Getenv("DB_DB")
	ssl := os.Getenv("DB_SSL")

	if db == "" {
		db = "dubstats"
	} else if ssl == "" {
		ssl = "require"
	}

	connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s", user, pass, host, db, ssl)
	m, err := migrate.New("file://ymfudiadau/", connStr)
	if err != nil {
		return err
	}

	err = m.Up()
	if err != nil && err != migrate.ErrNoChange {
		return err
	}
	log.Print("Migrated")

	return nil
}
