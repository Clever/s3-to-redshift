package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/kardianos/osext"
	env "github.com/segmentio/go-env"

	"github.com/Clever/discovery-go"
	"github.com/Clever/s3-to-redshift/logger"
	redshift "github.com/Clever/s3-to-redshift/redshift"
	s3filepath "github.com/Clever/s3-to-redshift/s3filepath"
	"github.com/hashicorp/go-multierror"
)

var (
	// things we are likely to change when running the worker normally are flags
	inputSchemaName = flag.String("schema", "mongo", "what target schema we load into")
	inputTables     = flag.String("tables", "", "target tables to run on, comma separated")
	inputBucket     = flag.String("bucket", "metrics", "bucket to load from, not including s3:// protocol")
	truncate        = flag.Bool("truncate", false, "do we truncate the table before inserting")
	force           = flag.Bool("force", false, "do we refresh the data even if it's already handled?")
	dataDate        = flag.String("date", "", "data date we should process, must be full RFC3339")
	configFile      = flag.String("config", "", "schema & table config to use in YAML format")
	gzip            = flag.Bool("gzip", true, "whether target files are gzipped, defaults to true")
	delimiter       = flag.String("delimiter", "", "delimiter for CSV files, usually pipe character. If empty then JSON will be assumed.")
	timeGranularity = flag.String("granularity", "day", "how often we expect to append new data")
	// things which will would strongly suggest launching as a second worker are env vars
	// also the secrets ... shhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh
	host               = os.Getenv("REDSHIFT_HOST")
	port               = os.Getenv("REDSHIFT_PORT")
	dbName             = env.MustGet("REDSHIFT_DB")
	user               = env.MustGet("REDSHIFT_USER")
	pwd                = env.MustGet("REDSHIFT_PASSWORD")
	awsAccessKeyID     = env.MustGet("AWS_ACCESS_KEY_ID")
	awsSecretAccessKey = env.MustGet("AWS_SECRET_ACCESS_KEY")
	vacuumWorker       = env.MustGet("VACUUM_WORKER")

	// payloadForSignalFx holds a subset of the job payload that
	// we want to alert on as a dimension in SignalFx.
	// This is necessary because we would like to selectively group
	// on job parameters - schema but not date, for instance, since
	// logging the date would overwhelm SignalFx
	payloadForSignalFx string

	gearmanAdminURL string
)

func init() {
	gearmanAdminUser := env.MustGet("GEARMAN_ADMIN_USER")
	gearmanAdminPass := env.MustGet("GEARMAN_ADMIN_PASS")
	gearmanAdminPath := env.MustGet("GEARMAN_ADMIN_PATH")
	gearmanAdminURL = generateServiceEndpoint(gearmanAdminUser, gearmanAdminPass, gearmanAdminPath)

	dir, err := osext.ExecutableFolder()
	if err != nil {
		log.Fatal(err)
	}
	err = logger.SetGlobalRouting(path.Join(dir, "kvconfig.yml"))
	if err != nil {
		log.Fatal(err)
	}
}

func generateServiceEndpoint(user, pass, path string) string {
	hostPort, err := discovery.HostPort("gearman-admin", "http")
	if err != nil {
		log.Fatal(err)
	}
	proto, err := discovery.Proto("gearman-admin", "http")
	if err != nil {
		log.Fatal(err)
	}

	return fmt.Sprintf("%s://%s:%s@%s%s", proto, user, pass, hostPort, path)
}

func fatalIfErr(err error, msg string) {
	if err != nil {
		logger.JobFinishedEvent(payloadForSignalFx, false)
		panic(fmt.Sprintf("%s: %s", msg, err)) // TODO: kayvee
	}
}

// helper function used for verifying inputs
func getMapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// Rounds down a dateTime to a granularity
// For instance, 11:50AM will be truncated to 11:00AM
// if given a granularity of an hour
func truncateDate(date time.Time, granularity string) time.Time {
	switch granularity {
	case "hour":
		return date.Truncate(time.Hour)
	default:
		// Round down to day granularity by default
		return date.Truncate(24 * time.Hour)
	}
}

// getRegionForBucket looks up the region name for the given bucket
func getRegionForBucket(name string) (string, error) {
	// Any region will work for the region lookup, but the request MUST use
	// PathStyle
	config := aws.NewConfig().WithRegion("us-west-1").WithS3ForcePathStyle(true)
	session := session.New()
	client := s3.New(session, config)
	params := s3.GetBucketLocationInput{
		Bucket: aws.String(name),
	}
	resp, err := client.GetBucketLocation(&params)
	if err != nil {
		return "", fmt.Errorf("failed to get location for bucket '%s', %s", name, err)
	}
	if resp.LocationConstraint == nil {
		// "US Standard", returns an empty region. So return any region in the US
		return "us-east-1", nil
	}
	return *resp.LocationConstraint, nil
}

// in a transaction, truncate, create or update, and then copy from the s3 data file or manifest
// yell loudly if there is anything different in the target table compared to config (different distkey, etc)
func runCopy(db *redshift.Redshift, inputConf s3filepath.S3File, inputTable redshift.Table, targetTable *redshift.Table, truncate, gzip bool, delimiter, timeGranularity string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}

	// TRUNCATE for dimension tables, but not fact tables
	if truncate && targetTable != nil {
		log.Println("truncating table!")
		if err := db.Truncate(tx, inputConf.Schema, inputTable.Name); err != nil {
			return fmt.Errorf("err running truncate table: %s", err)
		}
	}
	if targetTable == nil {
		if err := db.CreateTable(tx, inputTable); err != nil {
			return fmt.Errorf("err running create table: %s", err)
		}
	} else {
		// To prevent duplicates, clear away any existing data within a certain time range as the data date
		// (that is, sharing the same data date up to a certain time granularity)
		if err := db.TruncateInTimeRange(tx, inputConf.Schema, inputTable.Name, inputConf.DataDate, timeGranularity, inputTable.Meta.DataDateColumn); err != nil {
			return fmt.Errorf("err truncating data for data refresh: %s", err)
		}

		if err := db.UpdateTable(tx, *targetTable, inputTable); err != nil {
			return fmt.Errorf("err running update table: %s", err)
		}
	}

	// COPY direct into it, ok to do since we're in a transaction
	// can't switch on file ending as manifest files b/c
	// manifest files obscure the underlying file types
	// instead just pass the delimiter along even if it's null
	if err := db.Copy(tx, inputConf, delimiter, true, gzip); err != nil {
		return fmt.Errorf("err running copy: %s", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("err committing transaction: %s", err)
	}

	if truncate {
		// If we've truncated the table we should run vacuum to clear out the old data
		// Only one vacuum can be run at a time, so we're going to throw this over the wall to
		// redshift-vacuum and use gearman-admin as a queueing service.
		if len(gearmanAdminURL) == 0 {
			log.Fatalf("Unable to post vacuum job to %s", vacuumWorker)
		} else {
			log.Println("Submitting job to Gearman admin")
			client := &http.Client{}
			endpoint := gearmanAdminURL + fmt.Sprintf("/%s", vacuumWorker)
			payload := fmt.Sprintf(`--delete %s."%s"`, inputConf.Schema, inputTable.Name)
			req, err := http.NewRequest("POST", endpoint, bytes.NewReader([]byte(payload)))
			if err != nil {
				log.Fatalf("Error creating new request: %s", err)
			}
			req.Header.Add("Content-Type", "text/plain")
			_, err = client.Do(req)
			if err != nil {
				log.Fatalf("Error submitting job: %s", err)
			}
		}
	}
	return nil
}

// This worker finds the latest file in s3 and uploads it to redshift
// If the destination table does not exist, the worker creates it
// If the destination table lacks columns, the worker creates those as well
// The worker also uses a column in the data to figure out whether the s3 data is
// newer than what already exists.
func main() {
	flag.Parse()

	payloadForSignalFx = fmt.Sprintf("--schema %s --tables %s --bucket %s --truncate=%t --force=%t --gzip=%t --delimiter %s --granularity %s",
		*inputSchemaName, *inputTables, *inputBucket, *truncate, *force, *gzip, *delimiter, *timeGranularity)
	defer logger.JobFinishedEvent(payloadForSignalFx, true)

	// verify that timeGranularity is a supported value. for convenience,
	// we use the convention that granularities must be valid PostgreSQL dateparts
	// (see: http://www.postgresql.org/docs/8.1/static/functions-datetime.html#FUNCTIONS-DATETIME-TRUNC)
	supportedGranularities := map[string]bool{"hour": true, "day": true}
	if !supportedGranularities[*timeGranularity] {
		logger.JobFinishedEvent(payloadForSignalFx, false)
		panic(fmt.Sprintf("Unsupported granularity, must be one of %v", getMapKeys(supportedGranularities)))
	}

	awsRegion, locationErr := getRegionForBucket(*inputBucket)
	fatalIfErr(locationErr, "error getting location for bucket "+*inputBucket)
	// use an custom bucket type for testablitity
	bucket := s3filepath.S3Bucket{*inputBucket, awsRegion, awsAccessKeyID, awsSecretAccessKey}

	timeout := 60 // can parameterize later if this is an issue
	if host == "" {
		host = "localhost"
	}
	if port == "" {
		port = "5439"
	}
	db, err := redshift.NewRedshift(host, port, dbName, user, pwd, timeout)
	fatalIfErr(err, "error getting redshift instance")

	var copyErrors error
	// for each table passed in - likely we could goroutine this out
	for _, t := range strings.Split(*inputTables, ",") {
		log.Printf("attempting to run on schema: %s table: %s", *inputSchemaName, t)
		// override most recent data file
		if *dataDate == "" {
			logger.JobFinishedEvent(payloadForSignalFx, false)
			panic("No date provided")
		}
		parsedDate, err := time.Parse(time.RFC3339, *dataDate)
		fatalIfErr(err, fmt.Sprintf("issue parsing date: %s", *dataDate))
		inputConf, err := s3filepath.CreateS3File(s3filepath.S3PathChecker{}, bucket, *inputSchemaName, t, *configFile, parsedDate)
		fatalIfErr(err, "Issue getting data file from s3")
		inputTable, err := db.GetTableFromConf(*inputConf) // allow passing explicit config later
		fatalIfErr(err, "Issue getting table from input")

		// figure out what the current state of the table is to determine if the table is already up to date
		targetTable, lastTargetData, err := db.GetTableMetadata(inputConf.Schema, inputConf.Table, inputTable.Meta.DataDateColumn)
		if err != nil {
			fatalIfErr(err, "Error getting existing latest table metadata") // use fatalIfErr to stay the same
		}

		// unless --force, don't update unless input data is new
		// Since lastTargetData comes from the columns, and
		// inputConf.DataDate comes from the filename, we round to
		// compare at the time granularity level
		if lastTargetData != nil && (truncateDate(*lastTargetData, *timeGranularity)).After(truncateDate(inputConf.DataDate, *timeGranularity)) {
			if *force == false {
				log.Printf("Recent data already exists in db: %s", *lastTargetData)
				return
			}
			log.Printf("Forcing update of inputTable: %s", inputConf.Table)
		}

		if err := runCopy(db, *inputConf, *inputTable, targetTable, *truncate, *gzip, *delimiter, *timeGranularity); err != nil {
			log.Printf("error running copy for table %s: %s", t, err)
			copyErrors = multierror.Append(copyErrors, err)
		} else {
			// DON'T NEED TO CREATE VIEWS - will be handled by the refresh script
			log.Printf("done with table: %s.%s", inputConf.Schema, t)
		}
	}
	if copyErrors != nil {
		log.Fatalf("error loading tables: %s", copyErrors)
	}
	log.Println("done with full run")
}
