package remoteread

import (
	"bytes"

	"github.com/conprof/conprof/pprof/report"
	"github.com/conprof/tsdb"
	"github.com/conprof/tsdb/labels"
	"github.com/google/pprof/profile"
	"github.com/prometheus/prometheus/prompb"
)

func Convert(profileSeriesSet tsdb.SeriesSet) ([]*prompb.TimeSeries, error) {
	labelToSeries := map[uint64]int{}
	metricTimeSeries := []*prompb.TimeSeries{}

	totalSamples := 0
	totalProfiles := 0
	for profileSeriesSet.Next() {
		s := profileSeriesSet.At()
		profileLabels := s.Labels()
		i := s.Iterator()

		for i.Next() {
			t, profileBytes := i.At()
			p, err := profile.Parse(bytes.NewBuffer(profileBytes))
			if err != nil {
				// continue as a non parseable profile should never make it into the database
				continue
			}

			r := report.NewDefault(p, report.Options{})
			ti, _ := report.TextItems(r)

			for _, item := range ti {
				l := labels.New(append(profileLabels, labels.Label{Name: "name", Value: item.Name}, labels.Label{Name: "__name__", Value: "pprof_cum"})...)
				h := l.Hash()

				var ts *prompb.TimeSeries
				k, found := labelToSeries[h]
				if found {
					ts = metricTimeSeries[k]
				} else {
					next := len(metricTimeSeries)

					pl := make([]prompb.Label, 0, len(l))
					for _, label := range l {
						pl = append(pl, prompb.Label{Name: label.Name, Value: label.Value})
					}

					ts = &prompb.TimeSeries{
						Labels:  pl,
						Samples: make([]prompb.Sample, 0, 10),
					}

					metricTimeSeries = append(metricTimeSeries, ts)
					labelToSeries[h] = next
				}

				ts.Samples = append(ts.Samples, prompb.Sample{
					Timestamp: t,
					Value:     float64(item.Cum),
				})
			}
			totalSamples += len(ti)
			totalProfiles++
		}

		if i.Err() != nil {
			return nil, i.Err()
		}
	}

	return metricTimeSeries, profileSeriesSet.Err()
}
