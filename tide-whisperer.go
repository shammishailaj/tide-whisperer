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
	uuid "github.com/satori/go.uuid"

	common "github.com/tidepool-org/go-common"
	"github.com/tidepool-org/go-common/clients"
	"github.com/tidepool-org/go-common/clients/disc"
	"github.com/tidepool-org/go-common/clients/hakken"
	"github.com/tidepool-org/go-common/clients/mongo"
	"github.com/tidepool-org/go-common/clients/shoreline"

	"github.com/tidepool-org/tide-whisperer/auth"
	"github.com/tidepool-org/tide-whisperer/store"
)

type (
	Config struct {
		clients.Config
		Auth                *auth.Config        `json:"auth"`
		Service             disc.ServiceListing `json:"service"`
		Mongo               mongo.Config        `json:"mongo"`
		store.SchemaVersion `json:"schemaVersion"`
	}

	// so we can wrap and marshal the detailed error
	detailedError struct {
		Status int `json:"status"`
		//provided to user so that we can better track down issues
		Id              string `json:"id"`
		Code            string `json:"code"`
		Message         string `json:"message"`
		InternalMessage string `json:"-"` //used only for logging so we don't want to serialize it out
	}
	//generic type as device data can be comprised of many things
	deviceData map[string]interface{}
)

var (
	error_status_check = detailedError{Status: http.StatusInternalServerError, Code: "data_status_check", Message: "checking of the status endpoint showed an error"}

	error_no_view_permisson  = detailedError{Status: http.StatusForbidden, Code: "data_cant_view", Message: "user is not authorized to view data"}
	error_no_permissons      = detailedError{Status: http.StatusInternalServerError, Code: "data_perms_error", Message: "error finding permissons for user"}
	error_running_query      = detailedError{Status: http.StatusInternalServerError, Code: "data_store_error", Message: "internal server error"}
	error_loading_events     = detailedError{Status: http.StatusInternalServerError, Code: "data_marshal_error", Message: "internal server error"}
	error_invalid_parameters = detailedError{Status: http.StatusInternalServerError, Code: "invalid_parameters", Message: "one or more parameters are invalid"}

	storage store.Storage
)

const (
	DATA_API_PREFIX           = "api/data"
	MedtronicLoopBoundaryDate = "2017-09-01"
)

//set the intenal message that we will use for logging
func (d detailedError) setInternalMessage(internal error) detailedError {
	d.InternalMessage = internal.Error()
	return d
}

func main() {
	var config Config

	if err := common.LoadEnvironmentConfig(
		[]string{"TIDEPOOL_TIDE_WHISPERER_SERVICE", "TIDEPOOL_TIDE_WHISPERER_ENV"},
		&config,
	); err != nil {
		log.Fatal(DATA_API_PREFIX, " Problem loading config: ", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}

	authClient, err := auth.NewClient(config.Auth, httpClient)
	if err != nil {
		log.Fatal(DATA_API_PREFIX, err)
	}

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

	gatekeeperClient := clients.NewGatekeeperClientBuilder().
		WithHostGetter(config.GatekeeperConfig.ToHostGetter(hakkenClient)).
		WithHttpClient(httpClient).
		WithTokenProvider(shorelineClient).
		Build()

	userCanViewData := func(authenticatedUserID string, targetUserID string) bool {
		if authenticatedUserID == targetUserID {
			return true
		}

		perms, err := gatekeeperClient.UserInGroup(authenticatedUserID, targetUserID)
		if err != nil {
			log.Println(DATA_API_PREFIX, "Error looking up user in group", err)
			return false
		}

		log.Println(perms)
		return !(perms["root"] == nil && perms["view"] == nil)
	}

	//log error detail and write as application/json
	jsonError := func(res http.ResponseWriter, err detailedError, startedAt time.Time) {

		err.Id = uuid.NewV4().String()

		log.Println(DATA_API_PREFIX, fmt.Sprintf("[%s][%s] failed after [%.5f]secs with error [%s][%s] ", err.Id, err.Code, time.Now().Sub(startedAt).Seconds(), err.Message, err.InternalMessage))

		jsonErr, _ := json.Marshal(err)

		res.Header().Add("content-type", "application/json")
		res.WriteHeader(err.Status)
		res.Write(jsonErr)
	}

	processResults := func(response http.ResponseWriter, iterator store.StorageIterator, startTime time.Time) {
		var writeCount int

		log.Printf("%s mongo processing started after %.5f seconds", DATA_API_PREFIX, time.Now().Sub(startTime).Seconds())

		response.Header().Add("Content-Type", "application/json")
		response.Write([]byte("["))

		var results map[string]interface{}
		for iterator.Next(&results) {
			if len(results) > 0 {
				if bytes, err := json.Marshal(results); err != nil {
					log.Printf("%s failed to marshal mongo result with error: %s", DATA_API_PREFIX, err)
				} else {
					if writeCount > 0 {
						response.Write([]byte(","))
					}
					response.Write([]byte("\n"))
					response.Write(bytes)
					writeCount += 1
				}
			}
		}

		if writeCount > 0 {
			response.Write([]byte("\n"))
		}
		response.Write([]byte("]"))

		log.Printf("%s mongo processing finished after %.5f seconds and returned %d records", DATA_API_PREFIX, time.Now().Sub(startTime).Seconds(), writeCount)
	}

	if err := shorelineClient.Start(); err != nil {
		log.Fatal(err)
	}

	storage := store.NewMongoStoreClient(&config.Mongo)

	router := pat.New()

	router.Add("GET", "/status", http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		start := time.Now()
		if err := storage.Ping(); err != nil {
			jsonError(res, error_status_check.setInternalMessage(err), start)
			return
		}
		res.Write([]byte("OK\n"))
		return
	}))

	// The /data/userId endpoint retrieves device/health data for a user based on a set of parameters
	// userid: the ID of the user you want to retrieve data for
	// type (optional) : The Tidepool data type to search for. Only objects with a type field matching the specified type param will be returned.
	//					can be /userid?type=smbg or a comma seperated list e.g /userid?type=smgb,cbg . If is a comma seperated
	//					list, then objects matching any of the sub types will be returned
	// subType (optional) : The Tidepool data subtype to search for. Only objects with a subtype field matching the specified subtype param will be returned.
	//					can be /userid?subtype=physicalactivity or a comma seperated list e.g /userid?subtypetype=physicalactivity,steps . If is a comma seperated
	//					list, then objects matching any of the types will be returned
	// startDate (optional) : Only objects with 'time' field equal to or greater than start date will be returned .
	//						  Must be in ISO date/time format e.g. 2015-10-10T15:00:00.000Z
	// endDate (optional) : Only objects with 'time' field less than to or equal to start date will be returned .
	//						  Must be in ISO date/time format e.g. 2015-10-10T15:00:00.000Z
	router.Add("GET", "/{userID}", httpgzip.NewHandler(http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
		start := time.Now()

		queryParams, err := store.GetParams(req.URL.Query(), &config.SchemaVersion)

		if err != nil {
			log.Println(DATA_API_PREFIX, fmt.Sprintf("Error parsing date: %s", err))
			jsonError(res, error_invalid_parameters, start)
			return
		}

		var td *shoreline.TokenData
		if sessionToken := req.Header.Get("x-tidepool-session-token"); sessionToken != "" {
			td = shorelineClient.CheckToken(sessionToken)
		} else if restrictedTokens, found := req.URL.Query()["restricted_token"]; found && len(restrictedTokens) == 1 {
			restrictedToken, restrictedTokenErr := authClient.GetRestrictedToken(req.Context(), restrictedTokens[0])
			if restrictedTokenErr == nil && restrictedToken != nil && restrictedToken.Authenticates(req) {
				td = &shoreline.TokenData{UserID: restrictedToken.UserID}
			}
		}

		if td == nil || !(td.IsServer || td.UserID == queryParams.UserId || userCanViewData(td.UserID, queryParams.UserId)) {
			jsonError(res, error_no_view_permisson, start)
			return
		}

		if _, ok := req.URL.Query()["carelink"]; !ok {
			if hasMedtronicDirectData, medtronicErr := storage.HasMedtronicDirectData(queryParams.UserId); medtronicErr != nil {
				log.Println(DATA_API_PREFIX, fmt.Sprintf("Error while querying for Medtronic Direct data: %s", medtronicErr))
				jsonError(res, error_running_query, start)
				return
			} else if !hasMedtronicDirectData {
				queryParams.Carelink = true
			}
		}
		if !queryParams.Dexcom {
			if dexcomDataSource, dexcomErr := storage.GetDexcomDataSource(queryParams.UserId); dexcomErr != nil {
				log.Println(DATA_API_PREFIX, fmt.Sprintf("Error while querying for Dexcom data source: %s", dexcomErr))
				jsonError(res, error_running_query, start)
				return
			} else {
				queryParams.DexcomDataSource = dexcomDataSource
			}
		}
		if _, ok := req.URL.Query()["medtronic"]; !ok {
			if hasMedtronicLoopData, medtronicErr := storage.HasMedtronicLoopDataAfter(queryParams.UserId, MedtronicLoopBoundaryDate); medtronicErr != nil {
				log.Println(DATA_API_PREFIX, fmt.Sprintf("Error while querying for Medtronic Loop data: %s", medtronicErr))
				jsonError(res, error_running_query, start)
				return
			} else if !hasMedtronicLoopData {
				queryParams.Medtronic = true
			}
		}
		if !queryParams.Medtronic {
			if medtronicUploadIds, medtronicErr := storage.GetLoopableMedtronicDirectUploadIdsAfter(queryParams.UserId, MedtronicLoopBoundaryDate); medtronicErr != nil {
				log.Println(DATA_API_PREFIX, fmt.Sprintf("Error while querying for Loopable Medtronic Direct upload ids: %s", medtronicErr))
				jsonError(res, error_running_query, start)
				return
			} else {
				queryParams.MedtronicDate = MedtronicLoopBoundaryDate
				queryParams.MedtronicUploadIds = medtronicUploadIds
			}
		}

		started := time.Now()

		iter := storage.GetDeviceData(queryParams)
		defer iter.Close()

		processResults(res, iter, started)
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
