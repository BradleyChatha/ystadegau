package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"go.uber.org/zap"
)

var logger *zap.Logger
var conn *sql.DB

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

	host := *ssmhost.Parameter.Value
	user := *ssmuser.Parameter.Value
	pass := *ssmpass.Parameter.Value

	connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=%s", user, pass, host, "dubstats", "require")
	conn, err = sql.Open("postgres", connStr)
	if err != nil {
		log.Fatal("Could not connect to database", zap.Error(err))
	}
	err = conn.Ping()
	if err != nil {
		log.Fatal("Could not connect to database", zap.Error(err))
	}
}

type QueryResult struct {
	Id   int     `json:"id"`
	Name string  `json:"string"`
	Rank float64 `json:"rank"`
}

type Request struct {
	Query string `json:"query"`
}

func main() {
	var err error
	logger, err = zap.NewProduction()
	if err != nil {
		log.Fatal(err)
	}

	httpMain()
}

func httpMain() {
	r := mux.NewRouter()
	r.Path("/search").Methods("GET").Queries("query", "{query}").HandlerFunc(doSearch)
	http.ListenAndServe(":5678", r)
}

func doSearch(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	query := vars["query"]
	logger.Info("Query", zap.String("query", query), zap.String("ip", r.RemoteAddr))

	rows, err := conn.Query("SELECT id, name, rank FROM search_packages($1);", query)
	if err != nil {
		logger.Error("Query failed", zap.String("query", query), zap.String("ip", r.RemoteAddr), zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	arr := make([]QueryResult, 0, 50)
	for rows.Next() {
		var value QueryResult
		err := rows.Scan(&value.Id, &value.Name, &value.Rank)
		if err != nil {
			logger.Error("Failed to scan a row", zap.String("query", query), zap.String("ip", r.RemoteAddr), zap.Error(err))
			continue
		}

		arr = append(arr, value)
	}

	bytes, _ := json.Marshal(arr)
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(bytes)
}
