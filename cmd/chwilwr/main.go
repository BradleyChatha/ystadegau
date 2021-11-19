package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"github.com/rs/cors"
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

//
type QueryResult struct {
	Id   int     `json:"id"`
	Name string  `json:"name"`
	Rank float64 `json:"rank"`
}

//
type StatsResult struct {
	Time             time.Time `json:"time"`
	DownloadsWeekly  int       `json:"downloadsWeekly"`
	DownloadsMonthly int       `json:"downloadsMonthly"`
	DownloadsTotal   int       `json:"downloadsTotal"`
	Stars            int       `json:"stars"`
	Watchers         int       `json:"watchers"`
	Issues           int       `json:"issues"`
	Forks            int       `json:"forks"`
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
	r.Path("/stats").Methods("GET").Queries("package", "{package}", "weeks", "{weeks}").HandlerFunc(doStats)

	c := cors.New(cors.Options{
		AllowedOrigins: []string{"*"},
	})
	http.ListenAndServe(":5678", c.Handler(r))
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

func doStats(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	pkg := vars["package"]
	weeks := vars["weeks"]
	logger.Info("Stats", zap.String("package", pkg), zap.String("weeks", weeks), zap.String("ip", r.RemoteAddr))

	weeksAsNum, err := strconv.Atoi(weeks)
	if err != nil {
		logger.Error("User provided a bad week value", zap.String("package", pkg), zap.String("weeks", weeks), zap.String("ip", r.RemoteAddr), zap.Error(err))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	rows, err := conn.Query(`
		SELECT time, downloads_weekly, downloads_monthly, downloads_total, stars, watchers, issues, forks FROM package_snapshot 
		WHERE package_version_id = 
			(
				SELECT id FROM package_version 
				WHERE package_id = 
				(
					SELECT id FROM package
					WHERE name = $1
				)
				ORDER BY id DESC
				LIMIT 1
			) 
		AND time >= (now() - interval '7 days' * $2);`, pkg, weeksAsNum)
	if err != nil {
		logger.Error("Query failed", zap.String("package", pkg), zap.String("weeks", weeks), zap.String("ip", r.RemoteAddr), zap.Error(err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	arr := make([]StatsResult, 0, weeksAsNum)
	for rows.Next() {
		var value StatsResult
		err = rows.Scan(&value.Time, &value.DownloadsWeekly, &value.DownloadsMonthly, &value.DownloadsTotal, &value.Stars, &value.Watchers, &value.Issues, &value.Forks)
		if err != nil {
			logger.Error("Error scanning row", zap.String("package", pkg), zap.String("weeks", weeks), zap.String("ip", r.RemoteAddr), zap.Error(err))
			continue
		}
		arr = append(arr, value)
	}

	bytes, _ := json.Marshal(arr)
	w.Header().Add("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(bytes)
}
