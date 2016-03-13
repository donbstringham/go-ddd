package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/metrics"
	kitprometheus "github.com/go-kit/kit/metrics/prometheus"

	"github.com/marcusolsson/goddd/booking"
	"github.com/marcusolsson/goddd/cargo"
	"github.com/marcusolsson/goddd/handling"
	"github.com/marcusolsson/goddd/inspection"
	"github.com/marcusolsson/goddd/location"
	"github.com/marcusolsson/goddd/repository"
	"github.com/marcusolsson/goddd/routing"
	"github.com/marcusolsson/goddd/tracking"
)

const (
	defaultPort              = "8080"
	defaultRoutingServiceURL = "http://localhost:7878"
)

func main() {
	var (
		addr  = envString("PORT", defaultPort)
		rsurl = envString("ROUTINGSERVICE_URL", defaultRoutingServiceURL)

		httpAddr          = flag.String("http.addr", ":"+addr, "HTTP listen address")
		routingServiceURL = flag.String("service.routing", rsurl, "routing service URL")

		ctx = context.Background()
	)

	flag.Parse()

	var logger log.Logger
	logger = log.NewLogfmtLogger(os.Stderr)
	logger = &serializedLogger{Logger: logger}
	logger = log.NewContext(logger).With("ts", log.DefaultTimestampUTC)

	fieldKeys := []string{"method"}

	requestCount := kitprometheus.NewCounter(stdprometheus.CounterOpts{
		Namespace: "api",
		Subsystem: "booking_service",
		Name:      "request_count",
		Help:      "Number of requests received.",
	}, fieldKeys)

	requestLatency := metrics.NewTimeHistogram(time.Microsecond, kitprometheus.NewSummary(stdprometheus.SummaryOpts{
		Namespace: "api",
		Subsystem: "booking_service",
		Name:      "request_latency_microseconds",
		Help:      "Total duration of requests in microseconds.",
	}, fieldKeys))

	var (
		cargos         = repository.NewCargo()
		locations      = repository.NewLocation()
		voyages        = repository.NewVoyage()
		handlingEvents = repository.NewHandlingEvent()
	)

	// Configure some questionable dependencies.
	var (
		handlingEventFactory = cargo.HandlingEventFactory{
			CargoRepository:    cargos,
			VoyageRepository:   voyages,
			LocationRepository: locations,
		}
		handlingEventHandler = handling.NewEventHandler(
			inspection.NewService(cargos, handlingEvents, nil),
		)
	)

	// Facilitate testing by adding some cargos.
	storeTestData(cargos)

	var rs routing.Service
	rs = routing.NewProxyingMiddleware(*routingServiceURL, ctx)(rs)

	var bs booking.Service
	bs = booking.NewService(cargos, locations, handlingEvents, rs)
	bs = booking.NewLoggingService(log.NewContext(logger).With("component", "booking"), bs)
	bs = booking.NewInstrumentingService(requestCount, requestLatency, bs)

	var ts tracking.Service
	ts = tracking.NewService(cargos, handlingEvents)
	ts = tracking.NewLoggingService(log.NewContext(logger).With("component", "tracking"), ts)

	var hs handling.Service
	hs = handling.NewService(handlingEvents, handlingEventFactory, handlingEventHandler)
	hs = handling.NewLoggingService(log.NewContext(logger).With("component", "handling"), hs)

	httpLogger := log.NewContext(logger).With("component", "http")

	mux := http.NewServeMux()

	mux.Handle("/booking/v1/", booking.MakeHandler(ctx, bs, httpLogger))
	mux.Handle("/tracking/v1/", tracking.MakeHandler(ctx, ts, httpLogger))
	mux.Handle("/handling/v1/", handling.MakeHandler(ctx, hs, httpLogger))

	http.Handle("/", accessControl(mux))
	http.Handle("/metrics", stdprometheus.Handler())

	errs := make(chan error, 2)
	go func() {
		logger.Log("transport", "http", "address", *httpAddr, "msg", "listening")
		errs <- http.ListenAndServe(*httpAddr, nil)
	}()
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT)
		errs <- fmt.Errorf("%s", <-c)
	}()

	logger.Log("terminated", <-errs)
}

func accessControl(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Origin, Content-Type")

		if r.Method == "OPTIONS" {
			return
		}

		h.ServeHTTP(w, r)
	})
}

func envString(env, fallback string) string {
	e := os.Getenv(env)
	if e == "" {
		return fallback
	}
	return e
}

func storeTestData(r cargo.Repository) {
	test1 := cargo.New("FTL456", cargo.RouteSpecification{
		Origin:          location.AUMEL,
		Destination:     location.SESTO,
		ArrivalDeadline: time.Now().AddDate(0, 0, 7),
	})
	_ = r.Store(test1)

	test2 := cargo.New("ABC123", cargo.RouteSpecification{
		Origin:          location.SESTO,
		Destination:     location.CNHKG,
		ArrivalDeadline: time.Now().AddDate(0, 0, 14),
	})
	_ = r.Store(test2)
}

type serializedLogger struct {
	mtx sync.Mutex
	log.Logger
}

func (l *serializedLogger) Log(keyvals ...interface{}) error {
	l.mtx.Lock()
	defer l.mtx.Unlock()
	return l.Logger.Log(keyvals...)
}
