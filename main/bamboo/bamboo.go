package main

import (
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/kardianos/osext"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/natefinch/lumberjack"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/samuel/go-zookeeper/zk"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/zenazn/goji"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/zenazn/goji/bind"
	"github.com/QubitProducts/bamboo/Godeps/_workspace/src/github.com/zenazn/goji/graceful"
	"github.com/QubitProducts/bamboo/api"
	"github.com/QubitProducts/bamboo/configuration"
	"github.com/QubitProducts/bamboo/qzk"
	"github.com/QubitProducts/bamboo/services/event_bus"
)

/*
	Commandline arguments
*/
var configFilePath string
var logPath string

func init() {
	flag.StringVar(&configFilePath, "config", "config/development.json", "Full path of the configuration JSON file")
	flag.StringVar(&logPath, "log", "", "Log path to a file. Default logs to stdout")
}

func main() {
	flag.Parse()
	configureLog()

	// Load configuration
	conf, err := configuration.FromFile(configFilePath)
	if err != nil {
		log.Fatal(err)
	}

	eventBus := event_bus.New()

	// Wait for died children to avoid zombies
	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGCHLD)
	go func() {
		for {
			sig := <-signalChannel
			if sig == syscall.SIGCHLD {
				r := syscall.Rusage{}
				syscall.Wait4(-1, nil, 0, &r)
			}
		}
	}()

	// Create StatsD client
	conf.StatsD.CreateClient()

	// Create Zookeeper connection
	zkConn := listenToZookeeper(conf, eventBus)

	// Register handlers
	handlers := event_bus.Handlers{Conf: &conf, Zookeeper: zkConn}
	eventBus.Register(handlers.MarathonEventHandler)
	eventBus.Register(handlers.ServiceEventHandler)
	eventBus.Publish(event_bus.MarathonEvent { EventType: "bamboo_startup", Timestamp: time.Now().Format(time.RFC3339) })

	// Start server
	initServer(&conf, zkConn, eventBus)
}

func initServer(conf *configuration.Configuration, conn *zk.Conn, eventBus *event_bus.EventBus) {
	stateAPI := api.StateAPI{Config: conf, Zookeeper: conn}
	serviceAPI := api.ServiceAPI{Config: conf, Zookeeper: conn}
	eventSubAPI := api.EventSubscriptionAPI{Conf: conf, EventBus: eventBus}

	conf.StatsD.Increment(1.0, "restart", 1)
	// Status live information
	goji.Get("/status", api.HandleStatus)

	// State API
	goji.Get("/api/state", stateAPI.Get)

	// Service API
	goji.Get("/api/services", serviceAPI.All)
	goji.Post("/api/services", serviceAPI.Create)
	goji.Put("/api/services/:id", serviceAPI.Put)
	goji.Delete("/api/services/:id", serviceAPI.Delete)
	goji.Post("/api/marathon/event_callback", eventSubAPI.Callback)

	// Static pages
	goji.Get("/*", http.FileServer(http.Dir(path.Join(executableFolder(), "webapp"))))

	registerMarathonEvent(conf)

	serve(conf)
}

// Get current executable folder path
func executableFolder() string {
	folderPath, err := osext.ExecutableFolder()
	if err != nil {
		log.Fatal(err)
	}
	return folderPath
}

func registerMarathonEvent(conf *configuration.Configuration) {

	client := &http.Client{}
	// it's safe to register with multiple marathon nodes
	for _, marathon := range conf.Marathon.Endpoints() {
		url := marathon + "/v2/eventSubscriptions?callbackUrl=" + conf.Bamboo.Endpoint + "/api/marathon/event_callback"
		req, _ := http.NewRequest("POST", url, nil)
		req.Header.Add("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			errorMsg := "An error occurred while accessing Marathon callback system: %s\n"
			log.Printf(errorMsg, err)
			return
		}
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Fatal(err)
			return
		}
		body := string(bodyBytes)
		if strings.HasPrefix(body, "{\"message") {
			warningMsg := "Access to the callback system of Marathon seems to be failed, response: %s\n"
			log.Printf(warningMsg, body)
		}
	}
}

func createAndListen(conf configuration.Zookeeper) (chan zk.Event, *zk.Conn) {
	conn, _, err := zk.Connect(conf.ConnectionString(), time.Second*10)

	if err != nil {
		log.Panic(err)
	}

	ch, _ := qzk.ListenToConn(conn, conf.Path, true, conf.Delay())
	return ch, conn
}

func listenToZookeeper(conf configuration.Configuration, eventBus *event_bus.EventBus) *zk.Conn {
	serviceCh, serviceConn := createAndListen(conf.Bamboo.Zookeeper)

	go func() {
		for {
			select {
			case _ = <-serviceCh:
				eventBus.Publish(event_bus.ServiceEvent{EventType: "change"})
			}
		}
	}()
	return serviceConn
}

func configureLog() {
	if len(logPath) > 0 {
		log.SetOutput(io.MultiWriter(&lumberjack.Logger{
			Filename: logPath,
			// megabytes
			MaxSize:    100,
			MaxBackups: 3,
			//days
			MaxAge: 28,
		}, os.Stdout))
	}
}

func serve(conf *configuration.Configuration){
	goji.DefaultMux.Compile()
	http.Handle("/", goji.DefaultMux)
	listener := bind.Socket(conf.Bamboo.Bind)
	log.Println("Starting Bamboo backend listen on", listener.Addr())
	graceful.HandleSignals()
	bind.Ready()
	graceful.PreHook(func() { log.Printf("Goji received signal, gracefully stopping") })
	graceful.PostHook(func() { log.Printf("Goji stopped") })
	err := graceful.Serve(listener, http.DefaultServeMux)
	if err != nil {
		log.Fatal(err)
	}
	graceful.Wait()
}
