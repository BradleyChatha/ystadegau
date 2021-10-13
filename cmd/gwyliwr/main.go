package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sqs"
	"github.com/aws/aws-sdk-go/service/ssm"

	"github.com/PuerkitoBio/goquery"
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

type SQSRaw struct {
	Command string          `json:"command"`
	Args    json.RawMessage `json:"args"`
}

var conn *sql.DB

var host string
var user string
var pass string
var ses *session.Session

func init() {
	var err error
	ses, err = session.NewSession(&aws.Config{
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
	db := os.Getenv("DB_DB")
	ssl := os.Getenv("DB_SSL")
	mode := os.Getenv("MODE")

	if mode == "" {
		mode = "prod"
	}

	var err error
	if mode == "prod" {
		logger, err = zap.NewProduction()
	} else {
		logger, err = zap.NewDevelopment()
	}
	if err != nil {
		print("Could not create logger")
		return
	}

	if db == "" {
		db = "dubstats"
	} else if ssl == "" {
		ssl = "require"
	}

	connStr := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s", strings.Split(host, ":")[0], strings.Split(host, ":")[1], user, pass, db, ssl)
	conn, err = sql.Open("postgres", connStr)
	if err != nil {
		logger.Fatal("Could not connect to database", zap.Error(err))
	}
	err = conn.Ping()
	if err != nil {
		logger.Fatal("Could not connect to database", zap.Error(err))
	}
	defer conn.Close()

	if mode == "test" {
		doTest(conn)
	} else if mode == "test-live" {
		doLiveTest(conn)
	} else {
		run()
	}
}

func run() {
	sqsc := sqs.New(ses)
	for {
		msgs, err := sqsc.ReceiveMessage(&sqs.ReceiveMessageInput{
			WaitTimeSeconds: aws.Int64(5),
			QueueUrl:        aws.String("https://sqs.eu-west-2.amazonaws.com/563553540449/ystadegau"),
		})
		if err != nil {
			logger.Error("Issue recieving message", zap.Error(err))
			return
		}

		if len(msgs.Messages) == 0 {
			continue
		}

		for _, msg := range msgs.Messages {
			var info SQSRaw
			err = json.Unmarshal([]byte(*msg.Body), &info)
			if err != nil {
				logger.Error("Error deserialising message", zap.Error(err))
				continue
			}
			logger.Info("Recieved command", zap.String("command", info.Command))

			success := true

			switch info.Command {
			case "update_package_list":
				err = updatePackageList(0, 10)
				if err != nil {
					logger.Error("Error refreshing package list", zap.Error(err))
					success = false
				}
			default:
				logger.Error("Invalid command", zap.String("command", info.Command))
			}

			if success {
				sqsc.DeleteMessage(&sqs.DeleteMessageInput{
					QueueUrl:      aws.String("https://sqs.eu-west-2.amazonaws.com/563553540449/ystadegau"),
					ReceiptHandle: msg.ReceiptHandle,
				})
			}
		}
	}
}

func doTest(conn *sql.DB) {
	logger.Info("Performing test")
	listings, err := parsePackageListing(bytes.NewBufferString(TEST_PACKAGE_LISTING))
	if err != nil {
		logger.Fatal("Error while parsing package listing", zap.Error(err))
	}
	for _, listing := range listings {
		logger.Debug("Listing", zap.String("name", listing.Name), zap.Time("time", listing.Registered))
	}

	stats, info, err := parsePackageInfo(bytes.NewBufferString(TEST_PACKAGE_STATS), bytes.NewBufferString(TEST_PACKAGE_VERSION_INFO))
	if err != nil {
		logger.Fatal("Error parsing package info", zap.Error(err))
	}

	logger.Sync()
	fmt.Printf("stats: %v\n", stats)
	fmt.Printf("info: %v\n", info)
}

func doLiveTest(conn *sql.DB) {
	ver, err := getLatestVersion("jioc")
	if err != nil {
		logger.Fatal("Error fetching version", zap.Error(err))
	}
	logger.Info("Version", zap.String("version", ver))

	stats, info, err := getStatsAndInfo("jioc", ver)
	if err != nil {
		logger.Fatal("Error fetching stats and info", zap.Error(err))
	}
	fmt.Printf("stats: %v\n", stats)
	fmt.Printf("info: %v\n", info)
}

func updatePackageList(skip int, limit int) error {
	resp, err := http.Get(fmt.Sprintf("https://code.dlang.org/?sort=registered&category=&skip=%d&limit=%d", skip, limit))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	listings, err := parsePackageListing(resp.Body)
	if err != nil {
		return err
	}

	stmt, err := conn.Prepare("INSERT INTO package(name, next_update) VALUES (@name, now()) ON CONFLICT DO NOTHING")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, listing := range listings {
		_, err := stmt.Exec(sql.Named("name", listing.Name))
		if err != nil {
			logger.Error("Failed to add package into database", zap.String("package", listing.Name), zap.Error(err))
		}
	}

	logger.Info("Packages list has been refreshed.")
	return nil
}

func getLatestVersion(pkg string) (string, error) {
	resp, err := http.Get("https://code.dlang.org/api/packages/" + pkg + "/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	semver := ""
	err = json.NewDecoder(resp.Body).Decode(&semver)
	return semver, err
}

func getStatsAndInfo(pkg string, ver string) (stats PackageStats, info PackageInfo, err error) {
	iresp, err := http.Get("https://code.dlang.org/api/packages/" + pkg + "/" + ver + "/info")
	if err != nil {
		return
	}
	defer iresp.Body.Close()

	sresp, err := http.Get("https://code.dlang.org/api/packages/" + pkg + "/stats")
	if err != nil {
		return
	}
	defer sresp.Body.Close()

	return parsePackageInfo(sresp.Body, iresp.Body)
}

func parsePackageListing(listing io.Reader) ([]Listing, error) {
	arr := make([]Listing, 0, 20)

	doc, err := goquery.NewDocumentFromReader(listing)
	if err != nil {
		return nil, err
	}

	doc.Find("tr").Each(func(_ int, s *goquery.Selection) {
		a := s.Find("td a")
		if a.Length() == 0 {
			return
		}
		span := s.Find("td span.dull")
		if span.Length() == 0 {
			return
		}

		timeAttr, _ := span.Attr("title")
		time, err := time.Parse("2006-Jan-02 15:04:05Z", timeAttr)
		if err != nil {
			logger.Error("Error parsing a date", zap.Error(err))
			return
		}
		arr = append(arr, Listing{Name: a.Text(), Registered: time})
	})

	return arr, nil
}

func parsePackageInfo(pstats io.Reader, pinfo io.Reader) (stats PackageStats, info PackageInfo, err error) {
	err = json.NewDecoder(pstats).Decode(&stats)
	if err != nil {
		return
	}
	err = json.NewDecoder(pinfo).Decode(&info)
	if err != nil {
		return
	}
	return
}

// So we don't hit up code.dlang.org during development/testing.
// Keep our impact as low as possible, as is courtesy of a web scraping tool.
const TEST_PACKAGE_LISTING = `
<!DOCTYPE html>
<html>
	<head>
		<link rel="stylesheet" type="text/css" href="./styles/common.css"/>
		<link rel="stylesheet" type="text/css" href="./styles/top.css"/>
		<link rel="stylesheet" type="text/css" href="./styles/top_p.css"/>
		<link rel="shortcut icon" href="./favicon.ico"/>
		<link rel="search" href="./opensearch.xml" type="application/opensearchdescription+xml" title="DUB"/>
		<meta name="viewport" content="width=device-width, initial-scale=1.0, minimum-scale=0.1, maximum-scale=10.0"/>
		<script type="application/javascript" src="scripts/home.js"></script><script type="application/javascript">window.categories = [{"description":"Stand-alone applications","indentedDescription":"Stand-alone applications","subCategories":[{"description":"Desktop applications","indentedDescription":" └ Desktop applications","subCategories":[{"description":"Development tools","indentedDescription":"     └ Development tools","subCategories":[{"description":"Profilers, Code analyzers, etc.","indentedDescription":"         └ Profilers, Code analyzers, etc.","subCategories":[],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development.analyzer"},{"description":"Build tools","indentedDescription":"         └ Build tools","subCategories":[],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development.build"},{"description":"Compilers","indentedDescription":"         └ Compilers","subCategories":[],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development.compiler"},{"description":"Debuggers","indentedDescription":"         └ Debuggers","subCategories":[],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development.debugger"},{"description":"Documentation processors","indentedDescription":"         └ Documentation processors","subCategories":[],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development.documentation"},{"description":"Packaging tools","indentedDescription":"         └ Packaging tools","subCategories":[],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development.packaging"},{"description":"Plugin/modification","indentedDescription":"         └ Plugin/modification","subCategories":[],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development.plugin"},{"description":"Integrated development environments","indentedDescription":"         └ Integrated development environments","subCategories":[],"imageName":"editor","imageDescription":"application/desktop/development/ide","name":"application.desktop.development.ide"}],"imageName":"development","imageDescription":"application/desktop/development","name":"application.desktop.development"},{"description":"Text editors","indentedDescription":"     └ Text editors","subCategories":[],"imageName":"editor","imageDescription":"application/desktop/editor","name":"application.desktop.editor"},{"description":"Games","indentedDescription":"     └ Games","subCategories":[],"imageName":"game","imageDescription":"application/desktop/game","name":"application.desktop.game"},{"description":"Graphics editing","indentedDescription":"     └ Graphics editing","subCategories":[],"imageName":"graphics","imageDescription":"application/desktop/graphics","name":"application.desktop.graphics"},{"description":"Network tools","indentedDescription":"     └ Network tools","subCategories":[],"imageName":"network","imageDescription":"application/desktop/network","name":"application.desktop.network"},{"description":"Photographic applications","indentedDescription":"     └ Photographic applications","subCategories":[],"imageName":"photo","imageDescription":"application/desktop/photo","name":"application.desktop.photo"},{"description":"Office and productivity","indentedDescription":"     └ Office and productivity","subCategories":[],"imageName":"productivity","imageDescription":"application/desktop/productivity","name":"application.desktop.productivity"},{"description":"Web tools","indentedDescription":"     └ Web tools","subCategories":[],"imageName":"web","imageDescription":"application/desktop/web","name":"application.desktop.web"}],"imageName":"desktop","imageDescription":"application/desktop","name":"application.desktop"},{"description":"Mobile device applications","indentedDescription":" └ Mobile device applications","subCategories":[],"imageName":"mobile","imageDescription":"application/mobile","name":"application.mobile"},{"description":"Server software","indentedDescription":" └ Server software","subCategories":[{"description":"Chat/email servers","indentedDescription":"     └ Chat/email servers","subCategories":[],"imageName":"communication","imageDescription":"application/server/messaging","name":"application.server.messaging"},{"description":"Database servers","indentedDescription":"     └ Database servers","subCategories":[],"imageName":"database","imageDescription":"application/server/database","name":"application.server.database"},{"description":"Game servers","indentedDescription":"     └ Game servers","subCategories":[],"imageName":"server","imageDescription":"application/server","name":"application.server.game"},{"description":"Web servers","indentedDescription":"     └ Web servers","subCategories":[],"imageName":"server","imageDescription":"application/server","name":"application.server.web"}],"imageName":"server","imageDescription":"application/server","name":"application.server"},{"description":"Web applications/sites","indentedDescription":" └ Web applications/sites","subCategories":[{"description":"Development tools","indentedDescription":"     └ Development tools","subCategories":[],"imageName":"editor","imageDescription":"application/web/development","name":"application.web.development"},{"description":"Blogs, chats, forums, etc.","indentedDescription":"     └ Blogs, chats, forums, etc.","subCategories":[],"imageName":"communication","imageDescription":"application/web/communication","name":"application.web.communication"},{"description":"Office and productivity","indentedDescription":"     └ Office and productivity","subCategories":[],"imageName":"productivity","imageDescription":"application/web/productivity","name":"application.web.productivity"}],"imageName":"","imageDescription":"","name":"application.web"}],"imageName":"","imageDescription":"","name":"application"},{"description":"Development library","indentedDescription":"Development library","subCategories":[{"description":"Candidate for inclusion in the D standard library","indentedDescription":" └ Candidate for inclusion in the D standard library","subCategories":[],"imageName":"std","imageDescription":"library/std_aspirant","name":"library.std_aspirant"},{"description":"Audio libraries","indentedDescription":" └ Audio libraries","subCategories":[],"imageName":"audio","imageDescription":"library/audio","name":"library.audio"},{"description":"D language bindings","indentedDescription":" └ D language bindings","subCategories":[{"description":"Deimos header-only binding","indentedDescription":"     └ Deimos header-only binding","subCategories":[],"imageName":"binding","imageDescription":"library/binding","name":"library.binding.deimos"}],"imageName":"binding","imageDescription":"library/binding","name":"library.binding"},{"description":"Cryptographic libraries","indentedDescription":" └ Cryptographic libraries","subCategories":[],"imageName":"crypto","imageDescription":"library/crypto","name":"library.crypto"},{"description":"Development support libraries","indentedDescription":" └ Development support libraries","subCategories":[{"description":"Parsers and lexers","indentedDescription":"     └ Parsers and lexers","subCategories":[],"imageName":"development","imageDescription":"library/development","name":"library.development.parsing"}],"imageName":"development","imageDescription":"library/development","name":"library.development"},{"description":"Data storage and format support","indentedDescription":" └ Data storage and format support","subCategories":[],"imageName":"data","imageDescription":"library/data","name":"library.data"},{"description":"Data Structures and Containers","indentedDescription":" └ Data Structures and Containers","subCategories":[],"imageName":"data","imageDescription":"library/data_structures","name":"library.data_structures"},{"description":"Database clients or implementations","indentedDescription":" └ Database clients or implementations","subCategories":[],"imageName":"database","imageDescription":"library/database","name":"library.database"},{"description":"File system libraries","indentedDescription":" └ File system libraries","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.filesystem"},{"description":"Libraries aimed at game development","indentedDescription":" └ Libraries aimed at game development","subCategories":[],"imageName":"game","imageDescription":"library/gamedev","name":"library.gamedev"},{"description":"Geospatial and geographic libraries","indentedDescription":" └ Geospatial and geographic libraries","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.geospatial"},{"description":"2D/3D graphics","indentedDescription":" └ 2D/3D graphics","subCategories":[],"imageName":"3d","imageDescription":"library/graphics","name":"library.graphics"},{"description":"Graphical user interfaces","indentedDescription":" └ Graphical user interfaces","subCategories":[],"imageName":"desktop","imageDescription":"library/gui","name":"library.gui"},{"description":"Text user interfaces","indentedDescription":" └ Text user interfaces","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.tui"},{"description":"Automated testing helper libraries","indentedDescription":" └ Automated testing helper libraries","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.testing"},{"description":"Memory allocation, managment and other related routines","indentedDescription":" └ Memory allocation, managment and other related routines","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.memory"},{"description":"Networking libraries","indentedDescription":" └ Networking libraries","subCategories":[{"description":"Network messaging library","indentedDescription":"     └ Network messaging library","subCategories":[],"imageName":"network","imageDescription":"library/network","name":"library.network.messaging"}],"imageName":"network","imageDescription":"library/network","name":"library.network"},{"description":"Scripting language support","indentedDescription":" └ Scripting language support","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.scripting"},{"description":"Version control","indentedDescription":" └ Version control","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.vcs"},{"description":"Video libraries","indentedDescription":" └ Video libraries","subCategories":[],"imageName":"video","imageDescription":"library/video","name":"library.video"},{"description":"vibe.d compatible library","indentedDescription":" └ vibe.d compatible library","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.vibed"},{"description":"Web related","indentedDescription":" └ Web related","subCategories":[{"description":"Authentication and user management","indentedDescription":"     └ Authentication and user management","subCategories":[],"imageName":"auth","imageDescription":"library/web/auth","name":"library.web.auth"},{"description":"Communication components","indentedDescription":"     └ Communication components","subCategories":[],"imageName":"communication","imageDescription":"library/web/communication","name":"library.web.communication"},{"description":"Web framework","indentedDescription":"     └ Web framework","subCategories":[],"imageName":"web","imageDescription":"library/web","name":"library.web.framework"},{"description":"Content management system","indentedDescription":"     └ Content management system","subCategories":[],"imageName":"web","imageDescription":"library/web","name":"library.web.cms"}],"imageName":"web","imageDescription":"library/web","name":"library.web"},{"description":"Base libraries and code collections","indentedDescription":" └ Base libraries and code collections","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.general"},{"description":"Library with generic code","indentedDescription":" └ Library with generic code","subCategories":[],"imageName":"generic","imageDescription":"library/generic","name":"library.generic"},{"description":"String encoding and conversion","indentedDescription":" └ String encoding and conversion","subCategories":[],"imageName":"library","imageDescription":"library","name":"library.encoding"},{"description":"Internationalization and localization","indentedDescription":" └ Internationalization and localization","subCategories":[],"imageName":"i18n","imageDescription":"library/i18n","name":"library.i18n"},{"description":"Scientific","indentedDescription":" └ Scientific","subCategories":[{"description":"Linear algebra","indentedDescription":"     └ Linear algebra","subCategories":[],"imageName":"scientific","imageDescription":"library/scientific","name":"library.scientific.linalg"},{"description":"Numerical methods and algorithms","indentedDescription":"     └ Numerical methods and algorithms","subCategories":[],"imageName":"scientific","imageDescription":"library/scientific","name":"library.scientific.numeric"},{"description":"Newton physics","indentedDescription":"     └ Newton physics","subCategories":[],"imageName":"scientific","imageDescription":"library/scientific","name":"library.scientific.newton"},{"description":"Bioinformatics","indentedDescription":"     └ Bioinformatics","subCategories":[],"imageName":"scientific","imageDescription":"library/scientific","name":"library.scientific.bioinformatics"}],"imageName":"scientific","imageDescription":"library/scientific","name":"library.scientific"},{"description":"Optimized for fast execution","indentedDescription":" └ Optimized for fast execution","subCategories":[],"imageName":"cpu","imageDescription":"library/optimized_cpu","name":"library.optimized_cpu"},{"description":"Optimized for low memory usage","indentedDescription":" └ Optimized for low memory usage","subCategories":[],"imageName":"mem","imageDescription":"library/optimized_mem","name":"library.optimized_mem"},{"description":"Suitable for @nogc use","indentedDescription":" └ Suitable for @nogc use","subCategories":[],"imageName":"gc","imageDescription":"library/nogc","name":"library.nogc"}],"imageName":"library","imageDescription":"library","name":"library"}];
</script><title>Find, Use and Share DUB Packages - DUB - The D package registry</title>
	</head>
	<body id="Home" class="doc">
		<script>document.body.className += ' have-javascript'</script>
		<div id="top">
			<div class="helper">
				<div class="helper expand-container active">
					<div class="logo">
						<a href="./">
							<img id="logo" alt="DUB Logo" src="./images/dub-header.png"/>
						</a>
					</div>
					<a href="#" title="Menu" class="hamburger expand-toggle">
						<span>Menu</span>
					</a>
					<div id="cssmenu">
						<ul>
							<li class="active">
								<a href="./">
									<span>Packages</span>
								</a>
							</li>
							<li class="expand-container">
								<a class="expand-toggle" href="#">
									<span>Documentation</span>
								</a>
								<ul class="expand-content">
									<li class="">
										<a href="https://dub.pm/getting_started">
											<span>Getting Started</span>
										</a>
									</li>
									<li class="">
										<a href="https://dub.pm/advanced_usage">
											<span>Advanced Usage</span>
										</a>
									</li>
									<li class="">
										<a href="https://dub.pm/commandline">
											<span>Command line</span>
										</a>
									</li>
									<li class="">
										<a href="https://dub.pm/develop">
											<span>Development</span>
										</a>
									</li>
									<li class="">
										<a href="https://dub.pm/package-format-json">
											<span>Package format (JSON)</span>
										</a>
									</li>
									<li class="">
										<a href="https://dub.pm/package-format-sdl">
											<span>Package format (SDLang)</span>
										</a>
									</li>
									<li class="">
										<a href="https://dub.pm/package-suppliers">
											<span>Package suppliers</span>
										</a>
									</li>
									<li class="">
										<a href="https://dub.pm/settings">
											<span>Settings</span>
										</a>
									</li>
								</ul>
							</li>
							<li class="expand-container">
								<a class="expand-toggle" href="#">
									<span>About</span>
								</a>
								<ul class="expand-content">
									<li class="">
										<a href="http://forum.rejectedsoftware.com/groups/rejectedsoftware.dub">
											<span>Forums</span>
										</a>
									</li>
									<li class="">
										<a href="https://github.com/dlang/dub-registry/issues">
											<span>Bug tracker (website)</span>
										</a>
									</li>
									<li class="">
										<a href="https://github.com/dlang/dub/issues">
											<span>Bug tracker (DUB)</span>
										</a>
									</li>
									<li class="">
										<a href="https://github.com/dlang/dub-registry">
											<span>Github repository (website)</span>
										</a>
									</li>
									<li class="">
										<a href="https://github.com/dlang/dub">
											<span>GitHub repository (DUB)</span>
										</a>
									</li>
								</ul>
							</li>
							<li class="">
								<a href="https://github.com/dlang/dub/releases">
									<span>Download</span>
								</a>
							</li>
							<li class="">
								<a href="./login?redirect=/?sort=added&amp;category=&amp;skip=0&amp;limit=20">
									<span>Log in</span>
								</a>
							</li>
						</ul>
					</div>
					<div class="search-container expand-container">
						<a class="expand-toggle" href="search.html" title="Search">
							<span>Search</span>
						</a>
						<div id="search-box">
							<form method="GET" action="./search">
								<span id="search-query">
									<input id="q" name="q" placeholder="Search for a package"/>
								</span>
								<span id="search-submit">
									<button type="submit">
										<i class="fa fa-search"></i>
									</button>
								</span>
							</form>
						</div>
					</div>
				</div>
			</div>
		</div>
		<div id="content">
			<p>Welcome to DUB, the D package registry. The following list shows all available packages:</p>
			<form id="category-form" method="GET" action="">
				<input type="hidden" name="sort" value="added"/>
				<input type="hidden" name="limit" value="20"/>
				<p>
					Select category:
					<select id="category" name="category" size="1" onChange="document.getElementById(&quot;category-form&quot;).submit()">
						<option value="">All packages</option><option value="application">Stand-alone applications</option><option value="application.desktop"> └ Desktop applications</option><option value="application.desktop.development">     └ Development tools</option><option value="application.desktop.development.analyzer">         └ Profilers, Code analyzers, etc.</option><option value="application.desktop.development.build">         └ Build tools</option><option value="application.desktop.development.compiler">         └ Compilers</option><option value="application.desktop.development.debugger">         └ Debuggers</option><option value="application.desktop.development.documentation">         └ Documentation processors</option><option value="application.desktop.development.packaging">         └ Packaging tools</option><option value="application.desktop.development.plugin">         └ Plugin/modification</option><option value="application.desktop.development.ide">         └ Integrated development environments</option><option value="application.desktop.editor">     └ Text editors</option><option value="application.desktop.game">     └ Games</option><option value="application.desktop.graphics">     └ Graphics editing</option><option value="application.desktop.network">     └ Network tools</option><option value="application.desktop.photo">     └ Photographic applications</option><option value="application.desktop.productivity">     └ Office and productivity</option><option value="application.desktop.web">     └ Web tools</option><option value="application.mobile"> └ Mobile device applications</option><option value="application.server"> └ Server software</option><option value="application.server.messaging">     └ Chat/email servers</option><option value="application.server.database">     └ Database servers</option><option value="application.server.game">     └ Game servers</option><option value="application.server.web">     └ Web servers</option><option value="application.web"> └ Web applications/sites</option><option value="application.web.development">     └ Development tools</option><option value="application.web.communication">     └ Blogs, chats, forums, etc.</option><option value="application.web.productivity">     └ Office and productivity</option><option value="library">Development library</option><option value="library.std_aspirant"> └ Candidate for inclusion in the D standard library</option><option value="library.audio"> └ Audio libraries</option><option value="library.binding"> └ D language bindings</option><option value="library.binding.deimos">     └ Deimos header-only binding</option><option value="library.crypto"> └ Cryptographic libraries</option><option value="library.development"> └ Development support libraries</option><option value="library.development.parsing">     └ Parsers and lexers</option><option value="library.data"> └ Data storage and format support</option><option value="library.data_structures"> └ Data Structures and Containers</option><option value="library.database"> └ Database clients or implementations</option><option value="library.filesystem"> └ File system libraries</option><option value="library.gamedev"> └ Libraries aimed at game development</option><option value="library.geospatial"> └ Geospatial and geographic libraries</option><option value="library.graphics"> └ 2D/3D graphics</option><option value="library.gui"> └ Graphical user interfaces</option><option value="library.tui"> └ Text user interfaces</option><option value="library.testing"> └ Automated testing helper libraries</option><option value="library.memory"> └ Memory allocation, managment and other related routines</option><option value="library.network"> └ Networking libraries</option><option value="library.network.messaging">     └ Network messaging library</option><option value="library.scripting"> └ Scripting language support</option><option value="library.vcs"> └ Version control</option><option value="library.video"> └ Video libraries</option><option value="library.vibed"> └ vibe.d compatible library</option><option value="library.web"> └ Web related</option><option value="library.web.auth">     └ Authentication and user management</option><option value="library.web.communication">     └ Communication components</option><option value="library.web.framework">     └ Web framework</option><option value="library.web.cms">     └ Content management system</option><option value="library.general"> └ Base libraries and code collections</option><option value="library.generic"> └ Library with generic code</option><option value="library.encoding"> └ String encoding and conversion</option><option value="library.i18n"> └ Internationalization and localization</option><option value="library.scientific"> └ Scientific</option><option value="library.scientific.linalg">     └ Linear algebra</option><option value="library.scientific.numeric">     └ Numerical methods and algorithms</option><option value="library.scientific.newton">     └ Newton physics</option><option value="library.scientific.bioinformatics">     └ Bioinformatics</option><option value="library.optimized_cpu"> └ Optimized for fast execution</option><option value="library.optimized_mem"> └ Optimized for low memory usage</option><option value="library.nogc"> └ Suitable for @nogc use</option>
					</select>
					<button id="category-submit" type="submit">Update</button>
				</p>
			</form>
			<div id="category-dynamic-form" style="display: none">
				<p>
					Select category:<select id="categories_0" name="categories_0" onChange="setCategoryFromSelector(0)"></select><select id="categories_1" name="categories_1" onChange="setCategoryFromSelector(1)"></select><select id="categories_2" name="categories_2" onChange="setCategoryFromSelector(2)"></select><select id="categories_3" name="categories_3" onChange="setCategoryFromSelector(3)"></select><select id="categories_4" name="categories_4" onChange="setCategoryFromSelector(4)"></select><select id="categories_5" name="categories_5" onChange="setCategoryFromSelector(5)"></select>
				</p>
				
<script>
	//<![CDATA[
	setupCategoryForm();
	//]]>
</script>
			</div>
			<table>
				<tr>
					<th>
						<a href="?sort=name&amp;category=&amp;skip=0&amp;limit=20">Name</a>
					</th>
					<th>
						<a href="?sort=updated&amp;category=&amp;skip=0&amp;limit=20">Last update</a>
					</th>
					<th>
						<a href="?sort=score&amp;category=&amp;skip=0&amp;limit=20">Score</a>
					</th><th class="selected">Registered</th>
					<th>Description</th>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/slack-d">slack-d</a>
					</td>
					<td class="nobreak">
						0.0.1<span class="dull nobreak" title="2021-Oct-10 14:50:58Z">, 10 hours ago</span></td>
					<td class="nobreak" title="0.3&#10;&#10;#downloads / m: 0&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.3</a></td><td class="nobreak" title="2021-Oct-10 17:11:11">2021-Oct-10</td><td>Slack API for D</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/dlsplus">dlsplus</a>
					</td>
					<td class="nobreak">
						1.0.2<span class="dull nobreak" title="2021-Oct-07 18:09:52Z">, 3 days ago</span></td>
					<td class="nobreak" title="0.0&#10;&#10;#downloads / m: 1&#10;#stars: 0&#10;#watchers: 0&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.0</a></td><td class="nobreak" title="2021-Oct-07 19:48:33">2021-Oct-07</td><td>D Language Server</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/lifegame">lifegame</a>
					</td>
					<td class="nobreak">
						1.2.0<span class="dull nobreak" title="2021-Oct-08 13:24:30Z">, 2 days ago</span></td>
					<td class="nobreak" title="0.2&#10;&#10;#downloads / m: 10&#10;#stars: 0&#10;#watchers: 0&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.2</a></td><td class="nobreak" title="2021-Oct-07 15:59:17">2021-Oct-07</td><td>game Life on ncurses</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/idl2d">idl2d</a>
					</td>
					<td class="nobreak">
						1.0.0<span class="dull nobreak" title="2021-Oct-04 13:13:33Z">, 6 days ago</span></td>
					<td class="nobreak" title="0.4&#10;&#10;#downloads / m: 2&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.4</a></td><td class="nobreak" title="2021-Oct-04 15:11:23">2021-Oct-04</td><td>Helper for converting Microsoft IDL files to D extracted from https://github.com/dlang/visuald/tre&hellip;</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-library " title="library"></span><a href="packages/dutils">dutils</a>
					</td>
					<td class="nobreak">
						0.1.1<span class="dull nobreak" title="2021-Oct-02 14:53:13Z">, 8 days ago</span></td>
					<td class="nobreak" title="0.5&#10;&#10;#downloads / m: 10&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.5</a></td><td class="nobreak" title="2021-Oct-01 22:33:05">2021-Oct-01</td><td>A collection of packages in the D Programming Language made by me.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-development " title="library/development"></span><a href="packages/d-properties">d-properties</a>
					</td>
					<td class="nobreak">
						1.0.0<span class="dull nobreak" title="2021-Oct-01 11:22:11Z">, 9 days ago</span></td>
					<td class="nobreak" title="0.3&#10;&#10;#downloads / m: 1&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.3</a></td><td class="nobreak" title="2021-Oct-01 13:24:21">2021-Oct-01</td><td>D-language parser and serializer for Java-style properties files.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/rebindable">rebindable</a>
					</td>
					<td class="nobreak">
						0.0.3<span class="dull nobreak" title="2021-Sep-29 13:25:24Z">, 11 days ago</span></td>
					<td class="nobreak" title="0.5&#10;&#10;#downloads / m: 9&#10;#stars: 1&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.5</a></td><td class="nobreak" title="2021-Sep-29 12:11:55">2021-Sep-29</td><td>D data structures that work for any type.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-scientific " title="library/scientific"></span><a href="packages/minerva">minerva</a>
					</td>
					<td class="nobreak">
						0.0.3<span class="dull nobreak" title="2021-Sep-28 00:54:32Z">, 13 days ago</span></td>
					<td class="nobreak" title="0.1&#10;&#10;#downloads / m: 5&#10;#stars: 5&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 3&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.1</a></td><td class="nobreak" title="2021-Sep-25 01:22:52">2021-Sep-25</td><td>Small linear algebra library.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/keepalive">keepalive</a>
					</td>
					<td class="nobreak">
						1.1.1<span class="dull nobreak" title="2021-Sep-24 07:55:38Z">, 16 days ago</span></td>
					<td class="nobreak" title="0.6&#10;&#10;#downloads / m: 2&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 1&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.6</a></td><td class="nobreak" title="2021-Sep-23 22:02:23">2021-Sep-23</td><td>Tools to ensure pointers are kept on the stack.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-network application" title="application/desktop/network"></span><a href="packages/websocketd">websocketd</a>
					</td>
					<td class="nobreak">
						0.1.0<span class="dull nobreak" title="2021-Sep-22 13:44:46Z">, 18 days ago</span></td>
					<td class="nobreak" title="0.5&#10;&#10;#downloads / m: 3&#10;#stars: 2&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.5</a></td><td class="nobreak" title="2021-Sep-22 15:47:48">2021-Sep-22</td><td>Web socket server</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-library " title="library"></span><a href="packages/scheduled">scheduled</a>
					</td>
					<td class="nobreak">
						1.0.2<span class="dull nobreak" title="2021-Sep-22 20:57:06Z">, 18 days ago</span></td>
					<td class="nobreak" title="0.5&#10;&#10;#downloads / m: 1&#10;#stars: 3&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.5</a></td><td class="nobreak" title="2021-Sep-21 17:07:16">2021-Sep-21</td><td>Simple CRON-style scheduling library.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-game " title="library/gamedev"></span><a href="packages/vsignal">vsignal</a>
					</td>
					<td class="nobreak">
						0.1.0<span class="dull nobreak" title="2021-Sep-17 00:09:50Z">, 24 days ago</span></td>
					<td class="nobreak" title="2.4&#10;&#10;#downloads / m: 338&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">2.4</a></td><td class="nobreak" title="2021-Sep-17 02:56:36">2021-Sep-17</td><td>A multipurpose Signal library</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/tasky">tasky</a>
					</td>
					<td class="nobreak">
						0.0.5<span class="dull nobreak" title="2021-Sep-26 08:42:31Z">, 14 days ago</span></td>
					<td class="nobreak" title="0.6&#10;&#10;#downloads / m: 3&#10;#stars: 0&#10;#watchers: 2&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.6</a></td><td class="nobreak" title="2021-Sep-15 15:05:20">2021-Sep-15</td><td>Tagged network-message task engine</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/psd-d">psd-d</a>
					</td>
					<td class="nobreak">
						0.6.0<span class="dull nobreak" title="2021-Sep-15 15:58:46Z">, 25 days ago</span></td>
					<td class="nobreak" title="0.5&#10;&#10;#downloads / m: 7&#10;#stars: 3&#10;#watchers: 2&#10;#forks: 0&#10;#issues: 1&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.5</a></td><td class="nobreak" title="2021-Sep-15 11:42:11">2021-Sep-15</td><td>D PSD parsing library</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/symmetry_libssh-d">symmetry_libssh-d</a>
					</td>
					<td class="nobreak">
						~master<span class="dull nobreak" title="2021-Sep-14 18:02:22Z">, 26 days ago</span></td>
					<td class="nobreak" title="0.3&#10;&#10;#downloads / m: 1&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.3</a></td><td class="nobreak" title="2021-Sep-14 20:05:23">2021-Sep-14</td><td>D Programming Language binding for libssh: mulitplatform library implementing the SSHv2 and SSHv1 &hellip;</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/d_tree_sitter">d_tree_sitter</a>
					</td>
					<td class="nobreak">
						1.0.1<span class="dull nobreak" title="2021-Sep-13 02:17:55Z">, 27 days ago</span></td>
					<td class="nobreak" title="0.5&#10;&#10;#downloads / m: 9&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.5</a></td><td class="nobreak" title="2021-Sep-13 03:51:57">2021-Sep-13</td><td>The D bindings for tree-sitter</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-library " title="library"></span><a href="packages/dlearn">dlearn</a>
					</td>
					<td class="nobreak">
						0.0.2<span class="dull nobreak" title="2021-Sep-11 17:33:25Z">, 29 days ago</span></td>
					<td class="nobreak" title="0.0&#10;&#10;#downloads / m: 1&#10;#stars: 2&#10;#watchers: 2&#10;#forks: 0&#10;#issues: 7&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.0</a></td><td class="nobreak" title="2021-Sep-11 19:34:08">2021-Sep-11</td><td>A high-level linear algebra package in D. Based on NumPy.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-binding " title="library/binding"></span><a href="packages/d_mpdecimal">d_mpdecimal</a>
					</td>
					<td class="nobreak">
						0.4.0<span class="dull nobreak" title="2021-Sep-19 23:52:45Z">, 21 days ago</span></td>
					<td class="nobreak" title="0.6&#10;&#10;#downloads / m: 17&#10;#stars: 0&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.6</a></td><td class="nobreak" title="2021-Sep-11 06:07:00">2021-Sep-11</td><td>Bindings and a wrapper for using limpdec, a package for correctly-rounded arbitrary precision deci&hellip;</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/obj-wavefront">obj-wavefront</a>
					</td>
					<td class="nobreak">
						0.0.4<span class="dull nobreak" title="2021-Sep-12 17:56:56Z">, 28 days ago</span></td>
					<td class="nobreak" title="0.1&#10;&#10;#downloads / m: 3&#10;#stars: 0&#10;#watchers: 0&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.1</a></td><td class="nobreak" title="2021-Sep-09 10:30:52">2021-Sep-09</td><td>A very small Wavefront .obj file format library.</td>
				</tr>
				<tr>
					<td>
						<span class="category-icon icon-category-unknown " title="unknown"></span><a href="packages/eventy">eventy</a>
					</td>
					<td class="nobreak">
						0.1.4<span class="dull nobreak" title="2021-Sep-15 12:55:11Z">, 25 days ago</span></td>
					<td class="nobreak" title="0.4&#10;&#10;#downloads / m: 3&#10;#stars: 1&#10;#watchers: 1&#10;#forks: 0&#10;#issues: 0&#10;" style="color: #B03931;"><a href="https://dub.pm/develop#package-scoring">0.4</a></td><td class="nobreak" title="2021-Sep-08 12:44:01">2021-Sep-08</td><td>Easy to use event-loop dispatcher mechanism</td>
				</tr>
			</table>
			<ul class="pageNav">
				<li class="selected">1</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=20&amp;limit=20">2</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=40&amp;limit=20">3</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=60&amp;limit=20">4</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=80&amp;limit=20">5</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=100&amp;limit=20">6</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=120&amp;limit=20">7</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=140&amp;limit=20">8</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=160&amp;limit=20">9</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=180&amp;limit=20">10</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=200&amp;limit=20">11</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=220&amp;limit=20">12</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=240&amp;limit=20">13</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=260&amp;limit=20">14</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=280&amp;limit=20">15</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=300&amp;limit=20">16</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=320&amp;limit=20">17</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=340&amp;limit=20">18</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=360&amp;limit=20">19</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=380&amp;limit=20">20</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=400&amp;limit=20">21</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=420&amp;limit=20">22</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=440&amp;limit=20">23</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=460&amp;limit=20">24</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=480&amp;limit=20">25</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=500&amp;limit=20">26</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=520&amp;limit=20">27</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=540&amp;limit=20">28</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=560&amp;limit=20">29</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=580&amp;limit=20">30</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=600&amp;limit=20">31</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=620&amp;limit=20">32</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=640&amp;limit=20">33</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=660&amp;limit=20">34</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=680&amp;limit=20">35</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=700&amp;limit=20">36</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=720&amp;limit=20">37</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=740&amp;limit=20">38</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=760&amp;limit=20">39</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=780&amp;limit=20">40</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=800&amp;limit=20">41</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=820&amp;limit=20">42</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=840&amp;limit=20">43</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=860&amp;limit=20">44</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=880&amp;limit=20">45</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=900&amp;limit=20">46</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=920&amp;limit=20">47</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=940&amp;limit=20">48</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=960&amp;limit=20">49</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=980&amp;limit=20">50</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1000&amp;limit=20">51</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1020&amp;limit=20">52</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1040&amp;limit=20">53</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1060&amp;limit=20">54</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1080&amp;limit=20">55</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1100&amp;limit=20">56</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1120&amp;limit=20">57</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1140&amp;limit=20">58</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1160&amp;limit=20">59</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1180&amp;limit=20">60</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1200&amp;limit=20">61</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1220&amp;limit=20">62</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1240&amp;limit=20">63</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1260&amp;limit=20">64</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1280&amp;limit=20">65</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1300&amp;limit=20">66</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1320&amp;limit=20">67</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1340&amp;limit=20">68</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1360&amp;limit=20">69</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1380&amp;limit=20">70</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1400&amp;limit=20">71</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1420&amp;limit=20">72</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1440&amp;limit=20">73</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1460&amp;limit=20">74</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1480&amp;limit=20">75</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1500&amp;limit=20">76</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1520&amp;limit=20">77</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1540&amp;limit=20">78</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1560&amp;limit=20">79</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1580&amp;limit=20">80</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1600&amp;limit=20">81</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1620&amp;limit=20">82</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1640&amp;limit=20">83</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1660&amp;limit=20">84</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1680&amp;limit=20">85</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1700&amp;limit=20">86</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1720&amp;limit=20">87</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1740&amp;limit=20">88</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1760&amp;limit=20">89</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1780&amp;limit=20">90</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1800&amp;limit=20">91</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1820&amp;limit=20">92</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1840&amp;limit=20">93</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1860&amp;limit=20">94</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1880&amp;limit=20">95</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1900&amp;limit=20">96</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1920&amp;limit=20">97</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1940&amp;limit=20">98</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1960&amp;limit=20">99</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=1980&amp;limit=20">100</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=2000&amp;limit=20">101</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=2020&amp;limit=20">102</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=2040&amp;limit=20">103</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=2060&amp;limit=20">104</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=2080&amp;limit=20">105</a>
				</li>
			</ul>
			<ul class="pageNav perPage">
				<li>
					<a href="?sort=added&amp;category=&amp;skip=0&amp;limit=10">10</a>
				</li>
				<li class="selected">20</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=0&amp;limit=50">50</a>
				</li>
				<li>
					<a href="?sort=added&amp;category=&amp;skip=0&amp;limit=100">100</a>
				</li>
			</ul>
			<p>Displaying results 1 to 20 of 2083 packages found.</p>
			<p>
				Please <a href="login?redirect=/my_packages">log in</a> to manage your own packages.
			</p>
		</div>
	</body>
	<script type="application/javascript" src="/scripts/menu.js"></script>
</html>
`

const TEST_PACKAGE_STATS = `
{
	"updatedAt": "2021-10-11T01:26:56.692Z",
	"downloads": {
	  "total": 1,
	  "monthly": 2,
	  "weekly": 3,
	  "daily": 4
	},
	"repo": {
	  "stars": 1,
	  "watchers": 2,
	  "forks": 3,
	  "issues": 4
	},
	"score": 0.3192428946495056
  }
`
const TEST_PACKAGE_VERSION_INFO = `
{
	"info": {
	  "packageDescriptionFile": "dub.json",
	  "authors": [
		"Sinisa Susnjar"
	  ],
	  "license": "MIT",
	  "copyright": "Copyright © 2021, Sinisa Susnjar",
	  "name": "slack-d",
	  "description": "Slack API for D",
	  "configurations": [
		{
		  "targetType": "staticLibrary",
		  "name": "lib"
		}
	  ]
	},
	"readmeMarkdown": true,
	"date": "2021-10-10T14:50:58Z",
	"commitID": "18e2cb3635c3102f389f17e227b30b0a3ec72cdc",
	"readme": "[![ubuntu](https://github.com/sinisa-susnjar/slack-d/actions/workflows/ubuntu.yml/badge.svg)](https://github.com/sinisa-susnjar/slack-d/actions/workflows/ubuntu.yml) [![macos](https://github.com/sinisa-susnjar/slack-d/actions/workflows/macos.yml/badge.svg)](https://github.com/sinisa-susnjar/slack-d/actions/workflows/macos.yml) [![windows](https://github.com/sinisa-susnjar/slack-d/actions/workflows/windows.yml/badge.svg)](https://github.com/sinisa-susnjar/slack-d/actions/workflows/windows.yml) [![coverage](https://codecov.io/gh/sinisa-susnjar/slack-d/branch/main/graph/badge.svg?token=8IJIAOGVRZ)](https://codecov.io/gh/sinisa-susnjar/slack-d)\n\n# Slack Web API for D\n\n## Example\n\nd\nimport std.stdio;\nimport slack;\nvoid main() {\n\tauto slack = Slack(\"xoxb-YOUR-BOT-TOKEN-HERE\");\n\tauto r = slack.postMessage(\"Hello from slack-d !\");\n\twriteln(r);\n}\n\n\n# Implemented methods (so far)[^1]\n\n## Chat\n\n| Method                                         |\n| ---------------------------------------------- |\n| https://api.slack.com/methods/chat.postMessage |\n\n## Conversations\n\n| Method                                                   |\n| -------------------------------------------------------- |\n| https://api.slack.com/methods/conversations.list         |\n| https://api.slack.com/methods/conversations.history      |\n\n[^1]: see https://api.slack.com/methods for a full list of methods offered by the Slack API.\n",
	"docFolder": "",
	"version": "0.0.1"
  }
`
