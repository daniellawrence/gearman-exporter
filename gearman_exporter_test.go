package main

import (
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"
)

type gearman struct {
	*httptest.Server
	response []byte
}

func newDisk(response []byte) *gearman {
	h := &gearman{response: response}
	h.Server = httptest.NewServer(handler(h))
	return h
}

func handler(h *gearman) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Write(h.response)
	}
}

func handlerStale(exit chan bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		<-exit
	}
}

func readCounter(m prometheus.Counter) float64 {
	// TODO: Revisit this once client_golang offers better testing tools.
	pb := &dto.Metric{}
	m.Write(pb)
	return pb.GetCounter().GetValue()
}

func readGauge(m prometheus.Gauge) float64 {
	// TODO: Revisit this once client_golang offers better testing tools.
	pb := &dto.Metric{}
	m.Write(pb)
	return pb.GetGauge().GetValue()
}

func BenchmarkExtract(b *testing.B) {
	config, err := ioutil.ReadFile("test/gearman_agents.json")
	if err != nil {
		b.Fatalf("could not read config file: %v", err.Error())
	}

	h := newDisk(config)
	defer h.Close()

	e, _ := NewExporter("/dev/null", 1*time.Second)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		ch := make(chan prometheus.Metric)
		go func(ch chan prometheus.Metric) {
			for range ch {
			}
		}(ch)

		e.Collect(ch)
		close(ch)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	b.Logf("%d bytes used after %d runs", after.Alloc-before.Alloc, b.N)
}
