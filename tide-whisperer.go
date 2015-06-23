package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	httpgzip "github.com/daaku/go.httpgzip"
	"github.com/gorilla/pat"
	common "github.com/tidepool-org/go-common"
	"github.com/tidepool-org/go-common/clients"
	"github.com/tidepool-org/go-common/clients/disc"
	"github.com/tidepool-org/go-common/clients/hakken"
	"github.com/tidepool-org/go-common/clients/mongo"
	"github.com/tidepool-org/go-common/clients/shoreline"
	"labix.org/v2/mgo"
	"labix.org/v2/mgo/bson"
)

type (
	Config struct {
		clients.Config
		Service disc.ServiceListing `json:"service"`
		Mongo   mongo.Config        `json:"mongo"`
	}
	// so we can wrap and marshal the detailed error
	detailedError struct {
		Status     int    `json:"status"`
		Id         string `json:"id"`
		Message    string `json:"message"`
		RawMessage string `json:"-"` // not serializing out incase it leaks any details, we will log it though
	}
	//generic type as device data can be comprised of many things
	deviceData map[string]interface{}
)

var (
	error_status_check = detailedError{Status: http.StatusInternalServerError, Id: "data_status_check", Message: "checking of the status endpoint showed an error"}

	error_no_view_permisson = detailedError{Status: http.StatusForbidden, Id: "data_cant_view", Message: "user is not authorized to view data"}
	error_no_permissons     = detailedError{Status: http.StatusInternalServerError, Id: "data_perms_error", Message: "error finding permissons for user"}
	error_running_query     = detailedError{Status: http.StatusInternalServerError, Id: "data_store_error", Message: "error running query"}
	error_loading_events    = detailedError{Status: http.StatusInternalServerError, Id: "data_marshal_error", Message: "failed marshal data to return"}
)

const DATA_API_PREFIX = "api/data"

func (d *detailedError) setRaw(err error) {
	d.RawMessage = err.Error()
}

func main() {
	const deviceDataCollection = "deviceData"
	var config Config
	if err := common.LoadConfig([]string{"./config/env.json", "./config/server.json"}, &config); err != nil {
		log.Fatal(DATA_API_PREFIX, "Problem loading config: ", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}

	hakkenClient := hakken.NewHakkenBuilder().
		WithConfig(&config.HakkenConfig).
		Build()

	if err := hakkenClient.Start(); err != nil {
		log.Fatal(DATA_API_PREFIX, err)
	}
	defer func() {
		if err := hakkenClient.Close(); err != nil {
			log.Panic(DATA_API_PREFIX, "Error closing hakkenClient, panicing to get stacks: ", err)
		}
	}()

	shorelineClient := shoreline.NewShorelineClientBuilder().
		WithHostGetter(config.ShorelineConfig.ToHostGetter(hakkenClient)).
		WithHttpClient(httpClient).
		WithConfig(&config.ShorelineConfig.ShorelineClientConfig).
		Build()

	seagullClient := clients.NewSeagullClientBuilder().
		WithHostGetter(config.SeagullConfig.ToHostGetter(hakkenClient)).
		WithHttpClient(httpClient).
		Build()

	gatekeeperClient := clients.NewGatekeeperClientBuilder().
		WithHostGetter(config.GatekeeperConfig.ToHostGetter(hakkenClient)).
		WithHttpClient(httpClient).
		WithTokenProvider(shorelineClient).
		Build()

	userCanViewData := func(userID, groupID string) bool {
		if userID == groupID {
			return true
		}

		perms, err := gatekeeperClient.UserInGroup(userID, groupID)
		if err != nil {
			log.Println(DATA_API_PREFIX, "Error looking up user in group", err)
			return false
		}

		log.Println(perms)
		return !(perms["root"] == nil && perms["view"] == nil)
	}

	//log error detail and write as application/json
	jsonError := func(res http.ResponseWriter, detErr detailedError, startedAt time.Time) {

		if detErr.RawMessage == "" {
			detErr.RawMessage = detErr.Message
		}

		log.Println(DATA_API_PREFIX, fmt.Sprintf("[%s] failed after [%.5f]secs with error [%s] ", detErr.Id, time.Now().Sub(startedAt).Seconds(), detErr.RawMessage))

		jsonErr, _ = json.Marshal(err)

		res.Header().Add("content-type", "application/json")
		res.Write(jsonErr)
		res.WriteHeader(err.Status)
	}

	if err := shorelineClient.Start(); err != nil {
		log.Fatal(err)
	}

	session, err := mongo.Connect(&config.Mongo)
	if err != nil {
		log.Fatal(err)
	}
	//index based on sort and where kys
	index := mgo.Index{
		Key:        []string{"groupId", "_groupId", "time"},
		Background: true,
	}
	_ = session.DB("").C(deviceDataCollection).EnsureIndex(index)

	router := pat.New()
	router.Add("GET", "/status", http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		start := time.Now()

		mongoSession := session.Copy()
		defer mongoSession.Close()

		if err := mongoSession.Ping(); err != nil {
			jsonError(res, error_status_check.setRaw(err), start)
			return
		}
		res.Write([]byte("OK\n"))
		return
	}))
	router.Add("GET", "/{userID}", httpgzip.NewHandler(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		start := time.Now()

		userToView := req.URL.Query().Get(":userID")

		token := req.Header.Get("x-tidepool-session-token")
		td := shorelineClient.CheckToken(token)

		if td == nil || !(td.IsServer || td.UserID == userToView || userCanViewData(td.UserID, userToView)) {
			jsonError(res, error_no_view_permisson, start)
			return
		}

		pair := seagullClient.GetPrivatePair(userToView, "uploads", shorelineClient.TokenProvide())
		if pair == nil {
			jsonError(res, error_no_permissons, start)
			return
		}

		groupId := pair.ID

		mongoSession := session.Copy()
		defer mongoSession.Close()

		//select this data
		groupDataQuery := bson.M{"$or": []bson.M{bson.M{"groupId": groupId}, bson.M{"_groupId": groupId, "_active": true}}}
		//don't return these fields
		removeFieldsForReturn := bson.M{"_id": 0, "_groupId": 0, "_version": 0, "_active": 0, "createdTime": 0, "modifiedTime": 0, "groupId": 0}

		var results []interface{}

		log.Println(DATA_API_PREFIX, fmt.Sprintf("mongo query [%#v]", groupDataQuery))

		startQueryTime := time.Now()

		//return un-ordered (i.e. the order isn't guaranteed by mongo)
		err := mongoSession.DB("").C(deviceDataCollection).
			Find(groupDataQuery).
			Select(removeFieldsForReturn).
			All(&results)

		if err != nil {
			jsonError(res, error_running_query.setRaw(err))
			return
		}

		log.Println(DATA_API_PREFIX, fmt.Sprintf("mongo query took [%.5f]secs and returned [%d] records", time.Now().Sub(startQueryTime).Seconds(), len(results)))

		jsonResults := []byte("[]") //legit that there is no data

		if len(results) != 0 {
			jsonResults, err := json.Marshal(results)
			if err != nil {
				jsonError(res, error_loading_events.setRaw(err), start)
				return
			}
		}
		log.Println(DATA_API_PREFIX, fmt.Sprintf("completed in [%.5f]secs", time.Now().Sub(start).Seconds()))
		res.Header().Add("content-type", "application/json")
		res.Write(jsonResults)
		return

	})))

	done := make(chan bool)
	server := common.NewServer(&http.Server{
		Addr:    config.Service.GetPort(),
		Handler: router,
	})

	var start func() error
	if config.Service.Scheme == "https" {
		sslSpec := config.Service.GetSSLSpec()
		start = func() error { return server.ListenAndServeTLS(sslSpec.CertFile, sslSpec.KeyFile) }
	} else {
		start = func() error { return server.ListenAndServe() }
	}
	if err := start(); err != nil {
		log.Fatal(DATA_API_PREFIX, err)
	}
	hakkenClient.Publish(&config.Service)

	signals := make(chan os.Signal, 40)
	signal.Notify(signals)
	go func() {
		for {
			sig := <-signals
			log.Printf(DATA_API_PREFIX+" Got signal [%s]", sig)

			if sig == syscall.SIGINT || sig == syscall.SIGTERM {
				server.Close()
				done <- true
			}
		}
	}()

	<-done
}
