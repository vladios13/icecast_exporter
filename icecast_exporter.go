// Copyright 2016 Markus Lindenberg
// Copyright 2026 icecast_exporter fork contributors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Command icecast_exporter is a Prometheus exporter for the Icecast
// streaming media server. It scrapes the JSON status API
// (/status-json.xsl, Icecast >= 2.4.0) and exposes metrics.
//
// This is a modernized fork of
// https://github.com/markuslindenberg/icecast_exporter.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "icecast"

var labelNames = []string{"listenurl", "server_type"}

// ISO8601 is a time.Time that unmarshals Icecast's ISO 8601 timestamps.
// Icecast emits offsets both as -0700 and -07:00 depending on
// version/platform, so both are accepted.
type ISO8601 time.Time

// Time returns the underlying time.Time value.
func (ts ISO8601) Time() time.Time {
	return time.Time(ts)
}

// UnmarshalJSON implements json.Unmarshaler.
func (ts *ISO8601) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "null" || s == "" {
		*ts = ISO8601(time.Time{})
		return nil
	}
	var lastErr error
	for _, layout := range []string{
		"2006-01-02T15:04:05-0700",
		time.RFC3339, // 2006-01-02T15:04:05-07:00 (or Z)
		"2006-01-02T15:04:05",
	} {
		parsed, err := time.Parse(layout, s)
		if err == nil {
			*ts = ISO8601(parsed)
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// Number is a float64 that unmarshals tolerantly: it accepts a JSON
// number, a numeric string ("128"), or a string with a numeric prefix
// ("128 kbps", "44 100"). Non-numeric values leave it unset.
type Number struct {
	Value float64
	Valid bool
}

// UnmarshalJSON implements json.Unmarshaler.
func (n *Number) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(strings.Trim(string(data), `"`))
	if s == "" || s == "null" {
		return nil
	}
	// Fast path: plain number.
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		n.Value = v
		n.Valid = true
		return nil
	}
	// Tolerant path: leading numeric part of a string like "128 kbps".
	numEnd := 0
	for i, r := range s {
		if (r >= '0' && r <= '9') || r == '.' || (i == 0 && (r == '-' || r == '+')) {
			numEnd = i + len(string(r))
			continue
		}
		break
	}
	if numEnd > 0 {
		if v, err := strconv.ParseFloat(s[:numEnd], 64); err == nil {
			n.Value = v
			n.Valid = true
		}
	}
	// Never fail the whole document because of one odd value.
	return nil
}

// IcecastStatusSource holds per-mountpoint statistics.
type IcecastStatusSource struct {
	Listeners    Number  `json:"listeners"`
	ListenerPeak Number  `json:"listener_peak"`
	Bitrate      Number  `json:"bitrate"`
	Listenurl    string  `json:"listenurl"`
	ServerType   string  `json:"server_type"`
	StreamStart  ISO8601 `json:"stream_start_iso8601"`
}

// icestatsRaw mirrors the top-level "icestats" object with the
// polymorphic "source" field kept raw.
type icestatsRaw struct {
	Admin       string          `json:"admin"`
	Host        string          `json:"host"`
	Location    string          `json:"location"`
	ServerID    string          `json:"server_id"`
	ServerStart ISO8601         `json:"server_start_iso8601"`
	Source      json.RawMessage `json:"source,omitempty"`
}

// IcecastStatus is the normalized status document.
type IcecastStatus struct {
	Icestats struct {
		Admin       string
		Host        string
		Location    string
		ServerID    string
		ServerStart ISO8601
		Source      []IcecastStatusSource
	}
}

// sanitizeJSON fixes the invalid JSON produced by Icecast's
// status-json.xsl template: fields the template believes to be numeric
// (bitrate, samplerate, ...) are emitted unquoted, but sources may send
// arbitrary strings, yielding e.g.
//
//	"bitrate": 128 kbps,
//
// which encoding/json rejects with "invalid character ' ' in numeric
// literal". sanitizeJSON quotes such bare values so the document parses.
// Valid JSON passes through unchanged (strings are respected, so values
// that are already quoted are never touched).
func sanitizeJSON(data []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(data) + 64)

	inString := false
	escaped := false
	i := 0
	for i < len(data) {
		c := data[i]
		if inString {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			i++
			continue
		}
		switch {
		case c == '"':
			inString = true
			out.WriteByte(c)
			i++
		case c == ':':
			out.WriteByte(c)
			i++
			// Skip whitespace after the colon.
			for i < len(data) && isJSONSpace(data[i]) {
				out.WriteByte(data[i])
				i++
			}
			if i >= len(data) {
				break
			}
			switch data[i] {
			case '"', '{', '[', 't', 'f', 'n':
				// String, object, array, true/false/null: leave as-is.
			default:
				// A bare value. Capture everything up to the next
				// structural delimiter at this nesting level.
				start := i
				for i < len(data) && data[i] != ',' && data[i] != '}' && data[i] != ']' && data[i] != '\n' && data[i] != '\r' {
					i++
				}
				raw := strings.TrimSpace(string(data[start:i]))
				trailing := string(data[start:i])[len(strings.TrimRight(string(data[start:i]), " \t")):]
				if raw == "" {
					// ":," or ":}" — emit null to keep the JSON valid.
					out.WriteString("null")
					out.WriteString(trailing)
					continue
				}
				if isValidJSONNumber(raw) {
					out.WriteString(raw)
				} else {
					out.WriteByte('"')
					out.WriteString(jsonEscape(raw))
					out.WriteByte('"')
				}
				out.WriteString(trailing)
			}
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.Bytes()
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isValidJSONNumber(s string) bool {
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil {
		return false
	}
	return dec.InputOffset() == int64(len(s))
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// parseStatus parses (after sanitizing) an Icecast status-json.xsl
// document, normalizing the "source" field which may be an array
// (multiple streams), a single object (one stream), or null/absent
// (no streams).
func parseStatus(data []byte) (*IcecastStatus, error) {
	data = sanitizeJSON(data)

	var doc struct {
		Icestats icestatsRaw `json:"icestats"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}

	var status IcecastStatus
	status.Icestats.Admin = doc.Icestats.Admin
	status.Icestats.Host = doc.Icestats.Host
	status.Icestats.Location = doc.Icestats.Location
	status.Icestats.ServerID = doc.Icestats.ServerID
	status.Icestats.ServerStart = doc.Icestats.ServerStart

	raw := bytes.TrimSpace(doc.Icestats.Source)
	switch {
	case len(raw) == 0 || bytes.Equal(raw, []byte("null")):
		// No active streams.
	case raw[0] == '[':
		if err := json.Unmarshal(raw, &status.Icestats.Source); err != nil {
			return nil, fmt.Errorf("parsing source array: %w", err)
		}
	case raw[0] == '{':
		var single IcecastStatusSource
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, fmt.Errorf("parsing source object: %w", err)
		}
		status.Icestats.Source = []IcecastStatusSource{single}
	default:
		return nil, fmt.Errorf("unexpected source field: %.32s", raw)
	}
	return &status, nil
}

// Exporter collects Icecast stats from the given URI and exports them
// using the prometheus metrics package.
type Exporter struct {
	URI    string
	logger *slog.Logger
	mutex  sync.Mutex
	client *http.Client

	up                              prometheus.Gauge
	totalScrapes, jsonParseFailures prometheus.Counter
	serverStart                     prometheus.Gauge
	listeners                       *prometheus.GaugeVec
	streamStart                     *prometheus.GaugeVec

	listenersTotal *prometheus.GaugeVec
	listenerPeak   *prometheus.GaugeVec
	bitrate        *prometheus.GaugeVec
	sourceCount    prometheus.Gauge
	serverInfo     *prometheus.GaugeVec
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri string, timeout time.Duration, logger *slog.Logger) *Exporter {
	return &Exporter{
		URI:    uri,
		logger: logger,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last scrape of Icecast successful.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_total_scrapes",
			Help:      "Current total Icecast scrapes.",
		}),
		jsonParseFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "exporter_json_parse_failures",
			Help:      "Number of errors while parsing JSON.",
		}),
		serverStart: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "server_start",
			Help:      "Timestamp of server startup.",
		}),
		listeners: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "listeners",
			Help:      "The number of currently connected listeners.",
		}, labelNames),
		streamStart: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "stream_start",
			Help:      "Timestamp of when the currently active source client connected to this mount point.",
		}, labelNames),
		listenersTotal: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "listeners_total",
			Help:      "The total number of currently connected listeners across all mount points.",
		}, nil),
		listenerPeak: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "listener_peak",
			Help:      "Peak concurrent number of listeners on this mount point.",
		}, labelNames),
		bitrate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "bitrate",
			Help:      "Source bitrate (kbps) as reported by the source client.",
		}, labelNames),
		sourceCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "source_count",
			Help:      "Number of active sources (mount points).",
		}),
		serverInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "server_info",
			Help:      "Icecast server information. Constant 1.",
		}, []string{"server_id", "host", "location"}),
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// Describe describes all the metrics ever exported by the Icecast
// exporter. It implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.up.Desc()
	ch <- e.totalScrapes.Desc()
	ch <- e.jsonParseFailures.Desc()
	ch <- e.serverStart.Desc()
	e.listeners.Describe(ch)
	e.streamStart.Describe(ch)
	e.listenersTotal.Describe(ch)
	e.listenerPeak.Describe(ch)
	e.bitrate.Describe(ch)
	ch <- e.sourceCount.Desc()
	e.serverInfo.Describe(ch)
}

// Collect fetches the stats from the configured Icecast location and
// delivers them as Prometheus metrics. It implements
// prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	s := e.scrape()

	e.listeners.Reset()
	e.streamStart.Reset()
	e.listenersTotal.Reset()
	e.listenerPeak.Reset()
	e.bitrate.Reset()
	e.serverInfo.Reset()

	if s != nil {
		if !s.Icestats.ServerStart.Time().IsZero() {
			e.serverStart.Set(float64(s.Icestats.ServerStart.Time().Unix()))
		}
		e.sourceCount.Set(float64(len(s.Icestats.Source)))
		if s.Icestats.ServerID != "" || s.Icestats.Host != "" || s.Icestats.Location != "" {
			e.serverInfo.WithLabelValues(s.Icestats.ServerID, s.Icestats.Host, s.Icestats.Location).Set(1)
		}
		var totalListeners float64
		var haveListeners bool
		for _, source := range s.Icestats.Source {
			if source.Listeners.Valid {
				e.listeners.WithLabelValues(source.Listenurl, source.ServerType).Set(source.Listeners.Value)
				totalListeners += source.Listeners.Value
				haveListeners = true
			}
			if !source.StreamStart.Time().IsZero() {
				e.streamStart.WithLabelValues(source.Listenurl, source.ServerType).Set(float64(source.StreamStart.Time().Unix()))
			}
			if source.ListenerPeak.Valid {
				e.listenerPeak.WithLabelValues(source.Listenurl, source.ServerType).Set(source.ListenerPeak.Value)
			}
			if source.Bitrate.Valid {
				e.bitrate.WithLabelValues(source.Listenurl, source.ServerType).Set(source.Bitrate.Value)
			}
		}
		if haveListeners || len(s.Icestats.Source) == 0 {
			e.listenersTotal.WithLabelValues().Set(totalListeners)
		}
	}

	ch <- e.up
	ch <- e.totalScrapes
	ch <- e.jsonParseFailures
	ch <- e.serverStart
	e.listeners.Collect(ch)
	e.streamStart.Collect(ch)
	e.listenersTotal.Collect(ch)
	e.listenerPeak.Collect(ch)
	e.bitrate.Collect(ch)
	ch <- e.sourceCount
	e.serverInfo.Collect(ch)
}

func (e *Exporter) scrape() *IcecastStatus {
	e.totalScrapes.Inc()

	resp, err := e.client.Get(e.URI)
	if err != nil {
		e.up.Set(0)
		e.logger.Error("Can't scrape Icecast", "err", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		e.up.Set(0)
		e.logger.Error("Unexpected status from Icecast", "status", resp.Status)
		return nil
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		e.up.Set(0)
		e.logger.Error("Can't read response body", "err", err)
		return nil
	}

	s, err := parseStatus(bodyBytes)
	if err != nil {
		e.up.Set(0)
		e.logger.Error("Can't read JSON", "err", err)
		e.jsonParseFailures.Inc()
		return nil
	}

	e.up.Set(1)
	return s
}

func main() {
	var (
		listenAddress    = flag.String("web.listen-address", ":9146", "Address to listen on for web interface and telemetry.")
		metricsPath      = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
		icecastScrapeURI = flag.String("icecast.scrape-uri", "http://localhost:8000/status-json.xsl", "URI on which to scrape Icecast.")
		icecastTimeout   = flag.Duration("icecast.timeout", 5*time.Second, "Timeout for trying to get stats from Icecast.")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	exporter := NewExporter(*icecastScrapeURI, *icecastTimeout, logger)
	prometheus.MustRegister(exporter)

	mux := http.NewServeMux()
	mux.Handle(*metricsPath, promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>
             <head><title>Icecast Exporter</title></head>
             <body>
             <h1>Icecast Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})

	server := &http.Server{
		Addr:    *listenAddress,
		Handler: mux,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("Starting server", "address", *listenAddress)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.Info("Received shutdown signal, terminating")
	case err := <-errCh:
		logger.Error("HTTP server failed", "err", err)
		os.Exit(1)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("Graceful shutdown failed", "err", err)
		os.Exit(1)
	}
}
