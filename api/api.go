// Copyright 2018 The conprof Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/conprof/conprof/remoteread"
	"github.com/conprof/tsdb"
	tsdbLabels "github.com/conprof/tsdb/labels"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/julienschmidt/httprouter"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/storage/remote"
)

type API struct {
	logger log.Logger
	db     *tsdb.DB
}

func New(logger log.Logger, db *tsdb.DB) *API {
	return &API{
		logger: logger,
		db:     db,
	}
}

type QueryResult struct {
	Series []Series `json:"series"`
}

type Series struct {
	Labels          map[string]string `json:"labels"`
	LabelSetEncoded string            `json:"labelsetEncoded"`
	Timestamps      []int64           `json:"timestamps"`
}

func (a *API) QueryRange(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	fromString := r.URL.Query().Get("from")
	from, err := strconv.ParseInt(fromString, 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad Request, unable to parse from %s", err.Error()), http.StatusBadRequest)
		return
	}

	toString := r.URL.Query().Get("to")
	to, err := strconv.ParseInt(toString, 10, 64)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad Request, unable to parse to %s", err.Error()), http.StatusBadRequest)
		return
	}

	q, err := a.db.Querier(from, to)
	if err != nil {
		level.Error(a.logger).Log("err", err)
	}

	queryString := r.URL.Query().Get("query")
	level.Debug(a.logger).Log("query", queryString, "from", from, "to", to)
	sel, err := promql.ParseMetricSelector(queryString)
	if err != nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	ms := make([]tsdbLabels.Matcher, 0, len(sel))

	for _, om := range sel {
		ms = append(ms, convertMatcher(om))
	}

	seriesSet, err := q.Select(ms...)
	if err != nil {
		level.Error(a.logger).Log("err", err)
	}

	res := &QueryResult{Series: []Series{}}
	for seriesSet.Next() {
		series := seriesSet.At()
		ls := series.Labels()
		filteredLabels := tsdbLabels.Labels{}
		m := make(map[string]string)
		for _, l := range ls {
			if l.Name != "" {
				filteredLabels = append(filteredLabels, l)
				m[l.Name] = l.Value
			}
		}

		resSeries := Series{Labels: m, LabelSetEncoded: base64.URLEncoding.EncodeToString([]byte(filteredLabels.String()))}
		i := series.Iterator()
		for i.Next() {
			t, _ := i.At()
			resSeries.Timestamps = append(resSeries.Timestamps, t)
		}
		err = i.Err()
		if err != nil {
			level.Error(a.logger).Log("err", err, "series", ls.String())
		}

		res.Series = append(res.Series, resSeries)
	}

	err = json.NewEncoder(w).Encode(res)
	if err != nil {
		level.Error(a.logger).Log("msg", "error marshaling json", "err", err)
	}
}

func convertMatcher(m *labels.Matcher) tsdbLabels.Matcher {
	switch m.Type {
	case labels.MatchEqual:
		return tsdbLabels.NewEqualMatcher(m.Name, m.Value)

	case labels.MatchNotEqual:
		return tsdbLabels.Not(tsdbLabels.NewEqualMatcher(m.Name, m.Value))

	case labels.MatchRegexp:
		res, err := tsdbLabels.NewRegexpMatcher(m.Name, "^(?:"+m.Value+")$")
		if err != nil {
			panic(err)
		}
		return res

	case labels.MatchNotRegexp:
		res, err := tsdbLabels.NewRegexpMatcher(m.Name, "^(?:"+m.Value+")$")
		if err != nil {
			panic(err)
		}
		return tsdbLabels.Not(res)
	}
	panic("storage.convertMatcher: invalid matcher type")
}

func (a *API) RemoteRead(w http.ResponseWriter, r *http.Request, ps httprouter.Params) {
	req, err := remote.DecodeReadRequest(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("Bad Request: %v", err), http.StatusBadRequest)
		return
	}

	resp := &prompb.ReadResponse{
		Results: make([]*prompb.QueryResult, len(req.Queries)),
	}

	for j, query := range req.Queries {
		q, err := a.db.Querier(query.StartTimestampMs, query.EndTimestampMs)
		if err != nil {
			http.Error(w, fmt.Sprintf("Bad Request: %v", err), http.StatusBadRequest)
			return
		}

		m := make([]tsdbLabels.Matcher, len(query.Matchers))
		for i, matcher := range query.Matchers {
			var match tsdbLabels.Matcher
			switch t := matcher.Type; t {
			case prompb.LabelMatcher_EQ:
				match = tsdbLabels.NewEqualMatcher(matcher.Name, matcher.Value)
			case prompb.LabelMatcher_NEQ:
				match = tsdbLabels.Not(tsdbLabels.NewEqualMatcher(matcher.Name, matcher.Value))
			case prompb.LabelMatcher_RE:
				match, err = tsdbLabels.NewRegexpMatcher(matcher.Name, matcher.Value)
				if err != nil {
					http.Error(w, fmt.Sprintf("Bad Request: %v", err), http.StatusBadRequest)
					return
				}
			case prompb.LabelMatcher_NRE:
				match = tsdbLabels.NewEqualMatcher(matcher.Name, matcher.Value)
				if err != nil {
					http.Error(w, fmt.Sprintf("Bad Request: %v", err), http.StatusBadRequest)
					return
				}
				match = tsdbLabels.Not(match)
			default:
				http.Error(w, "Bad Request: unknown matcher type", http.StatusBadRequest)
				return
			}
			m[i] = match
		}

		s, err := q.Select(m...)
		if err != nil {
			http.Error(w, fmt.Sprintf("Bad Request: %v", err), http.StatusBadRequest)
			return
		}

		ts, err := remoteread.Convert(s)
		if err != nil {
			http.Error(w, fmt.Sprintf("Bad Request: %v", err), http.StatusBadRequest)
			return
		}
		resp.Results[j] = &prompb.QueryResult{Timeseries: ts}
	}

	remote.EncodeReadResponse(resp, w)
}
