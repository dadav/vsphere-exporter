package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/view"
	"github.com/vmware/govmomi/vim25/mo"
)

var (
	vcUsername    string
	vcPassword    string
	vcUrl         string
	enableMocking bool
	fetchInterval int64
)

type datastoreMetrics struct {
	capacity       *prometheus.GaugeVec
	free           *prometheus.GaugeVec
	scrapeFailures prometheus.Counter
}

func newDatastoreMetrics() *datastoreMetrics {
	return &datastoreMetrics{
		capacity: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_datastore_capacity_bytes",
			Help: "Total capacity of the datastore in bytes",
		}, []string{"url", "name"}),
		free: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vcenter_datastore_free_bytes",
			Help: "Total free space of the datastore in bytes",
		}, []string{"url", "name"}),
		scrapeFailures: promauto.NewCounter(prometheus.CounterOpts{
			Name: "vcenter_datastore_scrape_failures_total",
			Help: "Number of datastore scrape failures",
		}),
	}
}

func init() {
	flag.StringVar(&vcUsername, "username", os.Getenv("GOVC_USERNAME"), "Username for vCenter connection")
	flag.StringVar(&vcPassword, "password", os.Getenv("GOVC_PASSWORD"), "Password for vCenter connection")
	flag.StringVar(&vcUrl, "url", os.Getenv("GOVC_URL"), "URL for vCenter connection")
	flag.Int64Var(&fetchInterval, "interval", 30, "Interval in which data will be fetched")
	flag.BoolVar(&enableMocking, "mocking", os.Getenv("GOVC_MOCKING") == "1", "Enable vCenter mocking")
	flag.Parse()
}

func newVCenterClient(ctx context.Context) (*govmomi.Client, error) {
	if enableMocking {
		return newMockClient(ctx)
	}
	return newRealClient(ctx)
}

func newMockClient(ctx context.Context) (*govmomi.Client, error) {
	model := simulator.ESX()
	if err := model.Create(); err != nil {
		return nil, err
	}

	s := model.Service.NewServer()
	u, _ := url.Parse(s.URL.String())
	u.User = url.UserPassword("user", "pass")
	return govmomi.NewClient(ctx, u, false)
}

func newRealClient(ctx context.Context) (*govmomi.Client, error) {
	if vcUrl == "" || vcUsername == "" || vcPassword == "" {
		return nil, fmt.Errorf("GOVC_URL, GOVC_USERNAME, and GOVC_PASSWORD must be set")
	}
	u, err := url.Parse(vcUrl)
	if err != nil {
		return nil, err
	}
	u.User = url.UserPassword(vcUsername, vcPassword)
	return govmomi.NewClient(ctx, u, false)
}

func metricsLoop(ctx context.Context, c *govmomi.Client, interval int64, metrics *datastoreMetrics) {
	go func() {
		m := view.NewManager(c.Client)

		v, err := m.CreateContainerView(ctx, c.ServiceContent.RootFolder, []string{"Datastore"}, true)
		if err != nil {
			log.Fatalf("Error creating container view: %v\n", err)
		}
		defer v.Destroy(ctx)

		ticker := time.NewTicker(time.Duration(interval))
		defer ticker.Stop()

		metrics.scrapeFailures.Add(0)

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var datastores []mo.Datastore
				err = v.Retrieve(ctx, []string{"Datastore"}, []string{"summary"}, &datastores)
				if err != nil {
					metrics.scrapeFailures.Inc()
					log.Printf("Error retriving datastore: %v\n", err)
					time.Sleep(time.Duration(interval) * time.Second)
					continue
				}

				for _, ds := range datastores {
					metrics.capacity.WithLabelValues(ds.Summary.Url, ds.Summary.Name).Set(float64(ds.Summary.Capacity))
					metrics.free.WithLabelValues(ds.Summary.Url, ds.Summary.Name).Set(float64(ds.Summary.FreeSpace))
				}
			}
		}
	}()
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := newVCenterClient(ctx)
	if err != nil {
		log.Fatalf("Error while creating client: %v\n", err)
	}

	if fetchInterval <= 0 {
		log.Println("Invalid fetch interval, using default 30s")
		fetchInterval = 30
	}
	metrics := newDatastoreMetrics()
	metricsLoop(ctx, c, fetchInterval, metrics)

	http.Handle("/metrics", promhttp.Handler())
	fmt.Println("Listening on :2112")
	if err := http.ListenAndServe(":2112", nil); err != nil {
		log.Fatalf("HTTP server error: %v", err)
	}
}
