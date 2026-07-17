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

package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// fixtureMulti is a realistic status-json.xsl response with multiple
// sources and INVALID JSON as emitted by Icecast's template when a
// source client sends non-numeric values for "numeric" fields
// (the exact bug reported: `"bitrate": 128 kbps`).
const fixtureMulti = `{"icestats":{
  "admin": "icemaster@localhost",
  "host": "radio.example.org",
  "location": "Earth",
  "server_id": "Icecast 2.4.4",
  "server_start": "Sat, 06 Jul 2019 12:00:00 +0300",
  "server_start_iso8601": "2019-07-06T12:00:00+0300",
  "source": [
    {
      "audio_info": "ice-samplerate=44100;ice-bitrate=128;ice-channels=2",
      "bitrate": 128 kbps,
      "genre": "Various",
      "listener_peak": 7,
      "listeners": 3,
      "listenurl": "http://radio.example.org:8000/stream",
      "samplerate": 44 100,
      "server_description": "My station",
      "server_name": "Stream One",
      "server_type": "audio/mpeg",
      "stream_start": "Sun, 07 Jul 2019 10:00:00 +0300",
      "stream_start_iso8601": "2019-07-07T10:00:00+0300",
      "dummy": null
    },
    {
      "bitrate": 96,
      "listener_peak": 2,
      "listeners": 1,
      "listenurl": "http://radio.example.org:8000/stream2.ogg",
      "server_type": "application/ogg",
      "stream_start_iso8601": "2019-07-08T09:30:00+03:00"
    }
  ]
}}`

// fixtureSingle: exactly one active stream, "source" is an object.
const fixtureSingle = `{"icestats":{
  "host": "radio.example.org",
  "location": "Earth",
  "server_id": "Icecast 2.4.4",
  "server_start_iso8601": "2019-07-06T12:00:00+0300",
  "source": {
    "bitrate": 192 kbps,
    "listener_peak": 12,
    "listeners": 5,
    "listenurl": "http://radio.example.org:8000/only",
    "server_type": "audio/mpeg",
    "stream_start_iso8601": "2019-07-07T10:00:00+0300"
  }
}}`

// fixtureNone: no active streams, "source" absent.
const fixtureNone = `{"icestats":{
  "host": "radio.example.org",
  "location": "Earth",
  "server_id": "Icecast 2.4.4",
  "server_start_iso8601": "2019-07-06T12:00:00+0300"
}}`

// fixtureNullSource: no active streams, "source" explicitly null.
const fixtureNullSource = `{"icestats":{
  "server_id": "Icecast 2.4.4",
  "server_start_iso8601": "2019-07-06T12:00:00+0300",
  "source": null
}}`

func TestSanitizeJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "user reported bitrate bug",
			in:   `{"bitrate": 128 kbps, "listeners": 3}`,
			want: `{"bitrate": "128 kbps", "listeners": 3}`,
		},
		{
			name: "samplerate with thousands space",
			in:   `{"samplerate": 44 100}`,
			want: `{"samplerate": "44 100"}`,
		},
		{
			name: "valid json unchanged",
			in:   `{"a": 1, "b": "x y", "c": [1,2], "d": {"e": true}, "f": null, "g": -1.5e3}`,
			want: `{"a": 1, "b": "x y", "c": [1,2], "d": {"e": true}, "f": null, "g": -1.5e3}`,
		},
		{
			name: "string containing colon and braces untouched",
			in:   `{"url": "http://x:8000/a", "t": "a} b: c"}`,
			want: `{"url": "http://x:8000/a", "t": "a} b: c"}`,
		},
		{
			name: "escaped quote in string",
			in:   `{"s": "he said \"hi: 1 x\"", "n": 2 x}`,
			want: `{"s": "he said \"hi: 1 x\"", "n": "2 x"}`,
		},
		{
			name: "bare value at end of object",
			in:   `{"bitrate": 128 kbps}`,
			want: `{"bitrate": "128 kbps"}`,
		},
		{
			name: "empty value becomes null",
			in:   `{"x": , "y": 1}`,
			want: `{"x": null, "y": 1}`,
		},
		{
			name: "raw control char inside string escaped",
			in:   "{\"title\": \"A\x1cB\"}",
			want: `{"title": "A\u001cB"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(sanitizeJSON([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("sanitizeJSON(%q)\n got: %s\nwant: %s", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("sanitizeJSON produced invalid JSON: %s", got)
			}
		})
	}
}

func TestSanitizeJSONFixturesValid(t *testing.T) {
	for name, f := range map[string]string{
		"multi":  fixtureMulti,
		"single": fixtureSingle,
		"none":   fixtureNone,
		"null":   fixtureNullSource,
	} {
		if !json.Valid(sanitizeJSON([]byte(f))) {
			t.Errorf("fixture %s: sanitized output is not valid JSON", name)
		}
	}
}

func TestNumberUnmarshal(t *testing.T) {
	cases := []struct {
		in    string
		want  float64
		valid bool
	}{
		{`128`, 128, true},
		{`"128"`, 128, true},
		{`"128 kbps"`, 128, true},
		{`"44 100"`, 44, true},
		{`-1.5`, -1.5, true},
		{`"unknown"`, 0, false},
		{`null`, 0, false},
		{`""`, 0, false},
	}
	for _, tc := range cases {
		var n Number
		if err := json.Unmarshal([]byte(tc.in), &n); err != nil {
			t.Errorf("Number(%s): unexpected error: %v", tc.in, err)
			continue
		}
		if n.Valid != tc.valid || (tc.valid && n.Value != tc.want) {
			t.Errorf("Number(%s) = (%v, %v), want (%v, %v)", tc.in, n.Value, n.Valid, tc.want, tc.valid)
		}
	}
}

func TestISO8601(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{`"2019-07-06T12:00:00+0300"`, time.Date(2019, 7, 6, 12, 0, 0, 0, time.FixedZone("", 3*3600))},
		{`"2019-07-06T12:00:00+03:00"`, time.Date(2019, 7, 6, 12, 0, 0, 0, time.FixedZone("", 3*3600))},
		{`"2019-07-06T12:00:00Z"`, time.Date(2019, 7, 6, 12, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		var ts ISO8601
		if err := json.Unmarshal([]byte(tc.in), &ts); err != nil {
			t.Errorf("ISO8601(%s): unexpected error: %v", tc.in, err)
			continue
		}
		if !ts.Time().Equal(tc.want) {
			t.Errorf("ISO8601(%s) = %v, want %v", tc.in, ts.Time(), tc.want)
		}
	}
	var ts ISO8601
	if err := json.Unmarshal([]byte(`"garbage"`), &ts); err == nil {
		t.Error("ISO8601(garbage): expected error, got nil")
	}
}

func TestParseStatusMulti(t *testing.T) {
	s, err := parseStatus([]byte(fixtureMulti))
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if got := len(s.Icestats.Source); got != 2 {
		t.Fatalf("sources = %d, want 2", got)
	}
	src := s.Icestats.Source[0]
	if !src.Bitrate.Valid || src.Bitrate.Value != 128 {
		t.Errorf("bitrate = %+v, want 128 (from invalid `128 kbps`)", src.Bitrate)
	}
	if !src.Listeners.Valid || src.Listeners.Value != 3 {
		t.Errorf("listeners = %+v, want 3", src.Listeners)
	}
	if !src.ListenerPeak.Valid || src.ListenerPeak.Value != 7 {
		t.Errorf("listener_peak = %+v, want 7", src.ListenerPeak)
	}
	if src.Listenurl != "http://radio.example.org:8000/stream" {
		t.Errorf("listenurl = %q", src.Listenurl)
	}
	if src.ServerType != "audio/mpeg" {
		t.Errorf("server_type = %q", src.ServerType)
	}
	if src.StreamStart.Time().IsZero() {
		t.Error("stream_start not parsed")
	}
	if s.Icestats.ServerID != "Icecast 2.4.4" || s.Icestats.Host != "radio.example.org" || s.Icestats.Location != "Earth" {
		t.Errorf("server info = %q/%q/%q", s.Icestats.ServerID, s.Icestats.Host, s.Icestats.Location)
	}
	if s.Icestats.ServerStart.Time().IsZero() {
		t.Error("server_start not parsed")
	}
	// Second source uses -07:00 style offset.
	if s.Icestats.Source[1].StreamStart.Time().IsZero() {
		t.Error("second stream_start (with colon offset) not parsed")
	}
}

func TestParseStatusSingle(t *testing.T) {
	s, err := parseStatus([]byte(fixtureSingle))
	if err != nil {
		t.Fatalf("parseStatus: %v", err)
	}
	if got := len(s.Icestats.Source); got != 1 {
		t.Fatalf("sources = %d, want 1", got)
	}
	src := s.Icestats.Source[0]
	if !src.Bitrate.Valid || src.Bitrate.Value != 192 {
		t.Errorf("bitrate = %+v, want 192", src.Bitrate)
	}
	if src.Listenurl != "http://radio.example.org:8000/only" {
		t.Errorf("listenurl = %q", src.Listenurl)
	}
}

func TestParseStatusNoSources(t *testing.T) {
	for name, f := range map[string]string{"absent": fixtureNone, "null": fixtureNullSource} {
		s, err := parseStatus([]byte(f))
		if err != nil {
			t.Fatalf("parseStatus(%s): %v", name, err)
		}
		if got := len(s.Icestats.Source); got != 0 {
			t.Errorf("parseStatus(%s): sources = %d, want 0", name, got)
		}
	}
}

func TestParseStatusInvalid(t *testing.T) {
	if _, err := parseStatus([]byte(`not json at all {{{`)); err == nil {
		t.Error("expected error for unparseable input")
	}
}

// scrapeMetrics runs the exporter against uri and returns the rendered
// /metrics payload.
func scrapeMetrics(t *testing.T, uri string) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	exporter := NewExporter(uri, 2*time.Second, logger)
	registry := prometheus.NewRegistry()
	registry.MustRegister(exporter)

	rec := httptest.NewRecorder()
	promhttp.HandlerFor(registry, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", rec.Code)
	}
	return rec.Body.String()
}

func mustContain(t *testing.T, body string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(body, l) {
			t.Errorf("metrics output missing %q", l)
		}
	}
}

func TestIntegrationMultiSourceBrokenJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fixtureMulti)
	}))
	defer ts.Close()

	body := scrapeMetrics(t, ts.URL+"/status-json.xsl")
	mustContain(t, body,
		"icecast_up 1",
		"icecast_exporter_json_parse_failures 0",
		"icecast_exporter_total_scrapes 1",
		`icecast_listeners{listenurl="http://radio.example.org:8000/stream",server_type="audio/mpeg"} 3`,
		`icecast_listeners{listenurl="http://radio.example.org:8000/stream2.ogg",server_type="application/ogg"} 1`,
		`icecast_bitrate{listenurl="http://radio.example.org:8000/stream",server_type="audio/mpeg"} 128`,
		`icecast_bitrate{listenurl="http://radio.example.org:8000/stream2.ogg",server_type="application/ogg"} 96`,
		`icecast_listener_peak{listenurl="http://radio.example.org:8000/stream",server_type="audio/mpeg"} 7`,
		"icecast_listeners_total 4",
		"icecast_source_count 2",
		`icecast_server_info{host="radio.example.org",location="Earth",server_id="Icecast 2.4.4"} 1`,
		`icecast_stream_start{listenurl="http://radio.example.org:8000/stream",server_type="audio/mpeg"}`,
		"icecast_server_start 1.5624036e+09",
	)
}

func TestIntegrationSingleSource(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fixtureSingle)
	}))
	defer ts.Close()

	body := scrapeMetrics(t, ts.URL)
	mustContain(t, body,
		"icecast_up 1",
		"icecast_exporter_json_parse_failures 0",
		`icecast_listeners{listenurl="http://radio.example.org:8000/only",server_type="audio/mpeg"} 5`,
		`icecast_bitrate{listenurl="http://radio.example.org:8000/only",server_type="audio/mpeg"} 192`,
		"icecast_listeners_total 5",
		"icecast_source_count 1",
	)
}

func TestIntegrationNoSources(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, fixtureNone)
	}))
	defer ts.Close()

	body := scrapeMetrics(t, ts.URL)
	mustContain(t, body,
		"icecast_up 1",
		"icecast_exporter_json_parse_failures 0",
		"icecast_listeners_total 0",
		"icecast_source_count 0",
	)
	if strings.Contains(body, "icecast_listeners{") {
		t.Error("unexpected per-source listeners metric with no sources")
	}
}

func TestIntegrationIcecastDown(t *testing.T) {
	body := scrapeMetrics(t, "http://127.0.0.1:1/status-json.xsl")
	mustContain(t, body, "icecast_up 0")
}

func TestIntegrationGarbageBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<html>definitely not json</html>")
	}))
	defer ts.Close()

	body := scrapeMetrics(t, ts.URL)
	mustContain(t, body,
		"icecast_up 0",
		"icecast_exporter_json_parse_failures 1",
	)
}
