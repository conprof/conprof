// Copyright 2018 The conprof Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package pprofui

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/google/pprof/driver"
	"github.com/google/pprof/profile"
	"github.com/julienschmidt/httprouter"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/timestamp"
	"github.com/prometheus/prometheus/promql"
	"github.com/spf13/pflag"
)

type Storage interface {
	GetFile(timestamp time.Time, matchers ...*labels.Matcher) (io.ReadCloser, error)
}

type pprofUI struct {
	logger log.Logger
	db     Storage
}

// NewServer creates a new Server backed by the supplied Storage.
func New(logger log.Logger, db Storage) *pprofUI {
	s := &pprofUI{
		logger: logger,
		db:     db,
	}

	return s
}

func parsePath(reqPath string) (series string, timestamp string, remainingPath string) {
	parts := strings.Split(path.Clean(strings.TrimPrefix(reqPath, "/pprof/")), "/")
	if len(parts) < 2 {
		return "", "", ""
	}
	return parts[0], parts[1], strings.Join(parts[2:], "/")
}

func (p *pprofUI) PprofView(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	series, timestampGet, remainingPath := parsePath(r.URL.Path)
	if !strings.HasPrefix(remainingPath, "/") {
		remainingPath = "/" + remainingPath
	}
	level.Debug(p.logger).Log("msg", "parsed path", "series", series, "timestamp", timestampGet, "remainingPath", remainingPath)
	decodedSeriesName, err := base64.URLEncoding.DecodeString(series)
	if err != nil {
		msg := fmt.Sprintf("could not decode series name: %s", err)
		http.Error(w, msg, http.StatusNotFound)
		return
	}
	seriesLabelsString := string(decodedSeriesName)
	seriesLabels, err := promql.ParseMetricSelector(seriesLabelsString)
	if err != nil {
		msg := fmt.Sprintf("failed to parse series labels %v with error %v", seriesLabelsString, err)
		http.Error(w, msg, http.StatusNotFound)
		return
	}

	server := func(args *driver.HTTPServerArgs) error {
		handler, ok := args.Handlers[remainingPath]
		if !ok {
			return errors.Errorf("unknown endpoint %s", remainingPath)
		}
		handler.ServeHTTP(w, r)
		return nil
	}

	storageFetcher := func(_ string, _, _ time.Duration) (*profile.Profile, string, error) {
		var prof *profile.Profile

		t, err := stringToInt(timestampGet)
		if err != nil {
			return nil, "", err
		}

		f, err := p.db.GetFile(timestamp.Time(t), seriesLabels...)
		if err != nil {
			return nil, "", errors.Wrap(err, "couldn't get profile file")
		}

		prof, err = profile.Parse(f)
		f.Close()
		return prof, "", err
	}

	// Invoke the (library version) of `pprof` with a number of stubs.
	// Specifically, we pass a fake FlagSet that plumbs through the
	// given args, a UI that logs any errors pprof may emit, a fetcher
	// that simply reads the profile we downloaded earlier, and a
	// HTTPServer that pprof will pass the web ui handlers to at the
	// end (and we let it handle this client request).
	if err := driver.PProf(&driver.Options{
		Flagset: &pprofFlags{
			FlagSet: pflag.NewFlagSet("pprof", pflag.ExitOnError),
			args: []string{
				"--symbolize", "none",
				"--http", "localhost:0",
				"", // we inject our own target
			},
		},
		UI:         &fakeUI{},
		Fetch:      fetcherFn(storageFetcher),
		HTTPServer: server,
	}); err != nil {
		_, _ = w.Write([]byte(err.Error()))
	}

	return
}

type fetcherFn func(_ string, _, _ time.Duration) (*profile.Profile, string, error)

func (f fetcherFn) Fetch(s string, d, t time.Duration) (*profile.Profile, string, error) {
	return f(s, d, t)
}

func intToString(i int64) string {
	return strconv.FormatInt(i, 10)
}
func stringToInt(s string) (int64, error) {
	i, err := strconv.ParseInt(s, 10, 64)
	return int64(i), err
}
