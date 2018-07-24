package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "gearman" // For Prometheus metrics.
)

var (
	functionLabelNames = []string{"function"}
)

type Exporter struct {
	URI   string
	mutex sync.RWMutex
	fetch func() (io.ReadCloser, error)

	up              prometheus.Gauge
	functionMetrics map[string]*prometheus.GaugeVec
}

func newFunctionMetric(metricName string, docString string, constLabels prometheus.Labels) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "function_" + metricName,
			Help:        docString,
			ConstLabels: constLabels,
		},
		functionLabelNames,
	)
}

func NewExporter(uri string, timeout time.Duration) (*Exporter, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	var fetch func() (io.ReadCloser, error)

	switch u.Scheme {
	case "tcp":
		fetch = fetchUnix(u.Host, "tcp", timeout)
	case "unix":
		fetch = fetchUnix(u.Path, "unix", timeout)
	default:
		return nil, fmt.Errorf("unsupported scheme: %q", u.Scheme)
	}

	return &Exporter{
		URI:   uri,
		fetch: fetch,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of gearman successful.",
		}),
		functionMetrics: map[string]*prometheus.GaugeVec{
			"functionJobs":        newFunctionMetric("jobs", "number of jobs queued or running", nil),
			"functionJobsRunning": newFunctionMetric("jobs_running", "number of running jobs", nil),
			"functionJobsWaiting": newFunctionMetric("jobs_waiting", "number of jobs waiting for an available worker", nil),
			"functionWorkers":     newFunctionMetric("workers", "number of capable workers", nil),
		},
	}, nil
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	for _, m := range e.functionMetrics {
		m.Describe(ch)
	}
	ch <- e.up.Desc()
}

// Collect fetches the stats from configured Gearman location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	e.resetMetrics()
	e.scrape()

	ch <- e.up
	e.collectMetrics(ch)
}

func fetchUnix(u string, schema string, timeout time.Duration) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) {
		f, err := net.DialTimeout(schema, u, timeout)

		if err != nil {
			return nil, err
		}
		if err := f.SetDeadline(time.Now().Add(timeout)); err != nil {
			f.Close()
			return nil, err
		}
		cmd := "status\n"
		n, err := io.WriteString(f, cmd)
		if err != nil {
			f.Close()
			return nil, err
		}
		if n != len(cmd) {
			f.Close()
			return nil, errors.New("write error")
		}
		return f, nil
	}
}

func (e *Exporter) scrape() {

	body, err := e.fetch()
	if err != nil {
		e.up.Set(0)
		log.Errorf("Can't scrape Gearman: %v", err)
		return
	}
	defer body.Close()
	e.up.Set(1)

	// read body

	b, err := ioutil.ReadAll(body)
	data := string(b)

	log.Debugln("data:", data)

	for _, line := range strings.Split(data, "\n") {
		if line == "." {
			break
		}

		parts := strings.SplitN(line, "\t", 4)

		queueName := parts[0]
		total, _ := strconv.ParseFloat(parts[1], 64)
		running, _ := strconv.ParseFloat(parts[2], 64)
		workers, _ := strconv.ParseFloat(parts[3], 64)
		waiting := total - running

		log.Debugln(queueName, total, running, workers, waiting)

		e.functionMetrics["functionJobs"].WithLabelValues(queueName).Set(total)
		e.functionMetrics["functionJobsRunning"].WithLabelValues(queueName).Set(running)
		e.functionMetrics["functionJobsWaiting"].WithLabelValues(queueName).Set(waiting)
		e.functionMetrics["functionWorkers"].WithLabelValues(queueName).Set(workers)

	}

}

func (e *Exporter) resetMetrics() {
	for _, m := range e.functionMetrics {
		m.Reset()
	}
}

func (e *Exporter) collectMetrics(metrics chan<- prometheus.Metric) {
	for _, m := range e.functionMetrics {
		m.Collect(metrics)
	}
}

func main() {
	const pidFileHelpText = `Path to Gearman pid file.
	If provided, the standard process metrics get exported for the Gearman
	process, prefixed with "gearman_process_...". The gearman_process exporter
	needs to have read access to files owned by the Gearman process. Depends on
	the availability of /proc.
	https://prometheus.io/docs/instrumenting/writing_clientlibs/#process-metrics.`

	var (
		listenAddress    = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry.").Default(":9418").String()
		metricsPath      = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics.").Default("/metrics").String()
		gearmanScrapeURI = kingpin.Flag("gearman.scrape-uri", "URI on which to scrape Gearman.").Default("tcp://127.0.0.1:4730").String()
		gearmanTimeout   = kingpin.Flag("gearman.timeout", "Timeout for trying to get stats from Gearman.").Default("5s").Duration()
		gearmanPidFile   = kingpin.Flag("gearman.pid-file", pidFileHelpText).Default("").String()
	)

	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print("gearman_exporter"))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	log.Infoln("Starting gearman_exporter", version.Info())
	log.Infoln("Build context", version.BuildContext())

	exporter, err := NewExporter(*gearmanScrapeURI, *gearmanTimeout)
	if err != nil {
		log.Fatal(err)
	}
	prometheus.MustRegister(exporter)
	prometheus.MustRegister(version.NewCollector("gearman_exporter"))

	if *gearmanPidFile != "" {
		procExporter := prometheus.NewProcessCollectorPIDFn(
			func() (int, error) {
				content, err := ioutil.ReadFile(*gearmanPidFile)
				if err != nil {
					return 0, fmt.Errorf("Can't read pid file: %s", err)
				}
				value, err := strconv.Atoi(strings.TrimSpace(string(content)))
				if err != nil {
					return 0, fmt.Errorf("Can't parse pid file: %s", err)
				}
				return value, nil
			}, namespace)
		prometheus.MustRegister(procExporter)
	}

	log.Infoln("Listening on", *listenAddress)
	http.Handle(*metricsPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Gearman Exporter</title></head>
             <body>
             <h1>Gearman Exporter</h1>
             <p><a href="` + *metricsPath + `">Metrics</a></p>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
