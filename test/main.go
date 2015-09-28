package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/couchbase/gocb"
	consul "github.com/hashicorp/consul/api"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// these are populated by the flag variables
var (
	maxDocs         int // maximum document ID
	concurrency     int // number of goroutines to execute
	bucketName      string
	debug           bool   // flag to log debug statements
	cbCredsUsername string // for connecting to the Couchbase REST API
	cbCredsPassword string // for connecting to the Couchbase REST API
	clusterIps      []string
)

func main() {

	// set up our runtime environment
	runtime.GOMAXPROCS(runtime.NumCPU())
	rand.Seed(time.Now().Unix())

	flag.IntVar(&maxDocs, "i", 1000, "Number of documents for load script. [default 1000]")
	flag.IntVar(&concurrency, "c", 10, "Maximum number of goroutines to run. [default 10]")
	flag.StringVar(&bucketName, "b", "benchmark", "Name of bucket. [default benchmark]")
	flag.BoolVar(&debug, "debug", false, "Write debug-level logs")
	flag.StringVar(&cbCredsUsername, "u", "Administrator", "Couchbase REST API username")
	flag.StringVar(&cbCredsPassword, "p", "password", "Couchbase REST API password")

	doLoad := flag.Bool("load", false, "Load data into couchbase.")
	doIndex := flag.Bool("index", false, "Set up indexes and queries on couchbase.")
	doViewTest := flag.Bool("view", false, "Load test via making view queries")
	doN1QLTest := flag.Bool("n1ql", false, "Load test via making n1ql queries")
	doKeysTest := flag.Bool("keys", false, "Load test via making gets on keys")

	// parse arguments and assign to configuration
	flag.Parse()

	log.Println("Connecting to cluster...")
	cluster := getCluster()
	bucket := getBucket(cluster, bucketName)

	if *doLoad {
		log.Printf("Loading %v items w/ %v goroutines...", maxDocs, concurrency)
		preLoadData(bucket)
	} else if *doIndex {
		buildIndexes(bucket)
	} else if *doViewTest {
		log.Printf("Running view queries test w/ %v goroutines...", concurrency)
		runTest(bucket, getViewQuery)
	} else if *doN1QLTest {
		log.Printf("Running n1ql queries test w/ %v goroutines...", concurrency)
		runTest(bucket, getN1QLQuery)
	} else if *doKeysTest {
		log.Printf("Running get keys test w/ %v goroutines...", concurrency)
		runTest(bucket, getByKey)
	}
}

// ---------------------------------------------------
// cluster and bucket connections

// query consul for the IP of the first CB node we find
// equivalent of `curl -s http://consul:8500/v1/catalog/service/couchbase`
func getClusterIPs() []string {
	if len(clusterIps) != 0 {
		return clusterIps
	}
	var ips = []string{}
	config := &consul.Config{Address: "consul:8500"}
	if client, err := consul.NewClient(config); err != nil {
		log.Fatal(err)
	} else {
		catalog := client.Catalog()
		if service, _, err := catalog.Service("couchbase", "", nil); err != nil {
			log.Fatal(err)
		} else {
			if len(service) > 0 {
				for _, ip := range service {
					ips = append(ips, ip.ServiceAddress)
				}
			} else {
				log.Fatal("no IPs found for couchbase")
			}
		}
	}
	clusterIps = ips
	return ips
}

func getCluster() *gocb.Cluster {
	ips := getClusterIPs()
	url := fmt.Sprintf("couchbase://%v:8092", strings.Join(ips, ","))
	if cluster, err := gocb.Connect(url); err != nil {
		log.Fatal(err)
	} else {
		return cluster
	}
	return nil // stupid gofmt; we're just crashing here
}

func getBucket(cluster *gocb.Cluster, bucket string) *gocb.Bucket {
	if bucket, err := cluster.OpenBucket(bucket, ""); err != nil {
		log.Fatal(err)
	} else {
		return bucket
	}
	return nil // stupid gofmt; we're just crashing here
}

type testRunner func(bucket *gocb.Bucket, emailAddress string) error

// Run a test query function with a random email address in a continuous
// loop in `concurrency` number of goroutines
func runTest(bucket *gocb.Bucket, testFunc testRunner) {
	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func(*gocb.Bucket) {
			for {
				emailAddress := getRandomEmailAddress()
				if err := testFunc(bucket, emailAddress); err != nil {
					debugPrintf("%v", err)
				}
				if debug {
					if rand.Intn(10) == 1 {
						debugPrintf("goroutines:%v", runtime.NumGoroutine())
					}
				}
			}
		}(bucket)
	}
	wg.Wait()
}

// ---------------------------------------------------
// Loading test data and preparing queries and indexes

func preLoadData(bucket *gocb.Bucket) {

	// shared source of document IDs for all loading goroutines
	reads := make(chan int)
	go func(reads chan int) {
		for i := 0; i < maxDocs; i++ {
			reads <- i
		}
		close(reads)
	}(reads)

	// fan-out loading documents among `concurrency` goroutines
	var wg sync.WaitGroup
	wg.Add(concurrency)
	defer stopTimer(startTimer(fmt.Sprintf("preload")))
	for i := 0; i < concurrency; i++ {
		go loadDocuments(&wg, reads, bucket)
	}
	wg.Wait()
}

func buildIndexes(bucket *gocb.Bucket) {

	defer stopTimer(startTimer(fmt.Sprintf("prepareViews")))
	if err := createViewQuery(bucket); err != nil {
		log.Fatal(err)
	}

	defer stopTimer(startTimer(fmt.Sprintf("prepareN1QL")))
	if err := prepareN1QLIndex(bucket); err != nil {
		log.Fatal(err)
	}
}

func loadDocuments(wg *sync.WaitGroup, reads chan int, bucket *gocb.Bucket) {
	for docId := range reads {
		if err := loadDoc(bucket, docId); err != nil {
			log.Println(err)
		}
	}
	wg.Done()
	return
}

func loadDoc(bucket *gocb.Bucket, docId int) error {
	key := makeKey(docId)
	doc := makeDoc(key)
	if cas, err := timedInsert(bucket, key, doc, 0); err == nil {
	} else {
		// got a conflicting document; we're just going to overwrite
		// here so to uncomplicate having multiple runs of the benchmark
		if _, err := bucket.Replace(key, doc, cas, 0); err != nil {
			return err
		} else {
		}
	}
	return nil
}

func timedInsert(bucket *gocb.Bucket, key string, value interface{}, expiry uint32) (gocb.Cas, error) {
	defer stopTimer(startTimer(fmt.Sprintf("insert:%v", key)))
	return bucket.Insert(key, value, expiry)
}

// Hit the REST API to create a view that maps email to [id, name].
func createViewQuery(bucket *gocb.Bucket) error {
	url := fmt.Sprintf("http://%s:8092/%s/_design/viewByEmail", getClusterIPs()[0], bucketName)
	var body = `{
  "views": {
    "byEmail": {
      "map": "function (doc, meta) {emit(doc.email, [meta.id, doc.name]);}"
    }
  }
}`

	resp, err := restApiCall("PUT", url, body, "application/json")
	debugPrintf("createViewQuery: %v", resp)
	return err
}

// Hit the REST API to prepare a N1QL index
func prepareN1QLIndex(bucket *gocb.Bucket) error {

	nodes := getClusterIPs()
	url := fmt.Sprintf("http://%s:8093/query/service", nodes[0])
	var body = fmt.Sprintf("statement=create primary index on %v", bucketName)
	if resp, err := restApiFormPost(url, body); err != nil {
		debugPrintf("prepareN1QLIndex:primary:%v", resp)
		return err
	}

	var (
		bottom int = 0
		top    int = 0
	)
	statement := "statement=CREATE INDEX byEmail%v ON %v(email) WHERE email >= '%020d@joyent.com' AND email < '%020d@joyent.com' WITH {\"nodes\": [\"%v:8091\"]}&timeout=5m"

	step := int(maxDocs / len(nodes))
	for node, ip := range nodes {
		bottom = step * node
		top = bottom + step
		body = fmt.Sprintf(statement, node, bucketName, bottom, top, ip)

		// make sure there's no existing index
		resp, _ := restApiFormPost(url, fmt.Sprintf("statement=DROP INDEX benchmark.byEmail%v", node))
		debugPrintf("%v", resp)

		debugPrintf("prepareN1QLIndex:GSI:%v", body)
		if resp, err := restApiFormPost(url, body); err != nil {
			debugPrintf("prepareN1QLIndex:GSI:%v", resp)
			return err
		} else {
			debugPrintf("%v", resp)
		}
	}

	return nil
}

// ---------------------------------------------------
// testing view queries

// Get the document associated with the emailAddress by hitting
// the view query we created above
func getViewQuery(bucket *gocb.Bucket, emailAddress string) error {

	byEmailQuery := gocb.NewViewQuery("viewByEmail", "byEmail")
	byEmailQuery.Key(emailAddress)
	var row interface{}

	defer stopTimer(startTimer(fmt.Sprintf("viewQuery:%v", emailAddress)))
	if rows, err := bucket.ExecuteViewQuery(byEmailQuery); err != nil {
		return err
	} else {
		rows.One(&row)
		debugPrintf("%v", row)
		return rows.Close()
	}
}

// ---------------------------------------------------
// testing n1ql queries

type N1QLResult struct {
	Results []json.RawMessage `json:"results"`
}

// Get the document associated with the emailAddress by making a N1QL query
func getN1QLQuery(bucket *gocb.Bucket, emailAddress string) error {

	nodes := getClusterIPs()
	id, _ := strconv.Atoi(emailAddress[0:20])
	numNodes := len(nodes)
	step := maxDocs / numNodes
	node := id / step
	if node >= numNodes {
		node = numNodes - 1
	}

	qbody := fmt.Sprintf("SELECT id, name FROM %v USE INDEX (byEmail%v) WHERE email = '%v' ", bucketName, node, emailAddress)
	debugPrintf("%v", qbody)
	byEmailQuery := gocb.NewN1qlQuery(qbody) // defaults to adhoc=true
	var row interface{}

	defer stopTimer(startTimer(fmt.Sprintf("n1qlQuery:%v", emailAddress)))
	if rows, err := bucket.ExecuteN1qlQuery(byEmailQuery, nil); err != nil {
		return err
	} else {
		rows.One(&row)
		debugPrintf("%v", row)
		return rows.Close()
	}
}

// ---------------------------------------------------
// testing gets on keys

// Get the document by key
func getByKey(bucket *gocb.Bucket, emailAddress string) error {
	key := string(emailAddress[0:20])
	var row interface{}

	defer stopTimer(startTimer(fmt.Sprintf("get:%v", emailAddress)))
	if _, err := bucket.Get(key, &row); err != nil {
		return err
	} else {
		debugPrintf("%v", row)
	}
	return nil
}

// ---------------------------------------------------
// utilities

// defer this function and pass the startTimer function in as its
// arguments and it will wrap the scope with a timer that writes
// out to log

func restApiFormPost(url, body string) (string, error) {
	return restApiCall("POST", url, body, "application/x-www-form-urlencoded")
}

// wrap the REST API calls to Couchbase
func restApiCall(method, url, body, contentType string) (string, error) {
	req, _ := http.NewRequest(method, url, bytes.NewBuffer([]byte(body)))
	req.SetBasicAuth(cbCredsUsername, cbCredsPassword)
	req.Header.Set("Content-Type", contentType)

	client := &http.Client{Timeout: time.Duration(5 * time.Minute)}
	if resp, err := client.Do(req); err != nil {
		return "", err
	} else {
		defer resp.Body.Close()
		body, _ := ioutil.ReadAll(resp.Body)
		return string(body), nil
	}
}

func stopTimer(s string, startTime time.Time) {
	endTime := time.Now()
	log.Printf("time,%v,%vms", s, int64(endTime.Sub(startTime)/time.Millisecond))
}

func startTimer(s string) (string, time.Time) {
	return s, time.Now()
}

func makeKey(docId int) string {
	return fmt.Sprintf("%020d", docId)
}

func makeDoc(docId string) map[string]string {
	var doc = map[string]string{
		"email": fmt.Sprintf("%v@joyent.com", docId),
		"name":  getRandomString(20),
	}
	return doc
}

func getRandomString(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}

// generates a random email address within the maxDocs range, for use
// in testing queries
func getRandomEmailAddress() string {
	docId := rand.Intn(maxDocs)
	return fmt.Sprintf("%020d@joyent.com", docId)
}

func debugPrintf(fmtString string, vals ...interface{}) {
	if debug {
		log.Printf(fmtString, vals...)
	}
}
