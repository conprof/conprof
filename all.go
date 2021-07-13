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

package main

import (
	"context"
	"time"

	"github.com/conprof/db/storage"
	"github.com/conprof/db/tsdb"
	"github.com/conprof/db/tsdb/wal"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	grpc_logging "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/tags"
	"github.com/oklog/run"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/thanos-io/thanos/pkg/component"
	"github.com/thanos-io/thanos/pkg/extflag"
	"github.com/thanos-io/thanos/pkg/extkingpin"
	"github.com/thanos-io/thanos/pkg/extprom"
	"github.com/thanos-io/thanos/pkg/logging"
	objstore "github.com/thanos-io/thanos/pkg/objstore/client"
	"github.com/thanos-io/thanos/pkg/prober"
	grpcserver "github.com/thanos-io/thanos/pkg/server/grpc"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"gopkg.in/alecthomas/kingpin.v2"

	conprofapi "github.com/conprof/conprof/api"
	"github.com/conprof/conprof/pkg/store"
	"github.com/conprof/conprof/pkg/store/storepb"
	"github.com/conprof/conprof/scrape"
	"github.com/conprof/conprof/symbol"
)

type grpcSettings struct {
	grpcBindAddr    string
	grpcGracePeriod time.Duration
	grpcCert        string
	grpcKey         string
	grpcClientCA    string
}

// registerAll registers the all command.
func registerAll(m map[string]setupFunc, app *kingpin.Application, name string, reloadCh chan struct{}, reloaders *configReloaders) {
	cmd := app.Command(name, "All in one command.")

	storagePath := cmd.Flag("storage.tsdb.path", "Directory to read storage from.").
		Default("./data").String()
	configFile := cmd.Flag("config.file", "Config file to use.").
		Default("conprof.yaml").String()
	retention := extkingpin.ModelDuration(cmd.Flag("storage.tsdb.retention.time", "How long to retain raw samples on local storage. 0d - disables this retention").Default("15d"))
	maxMergeBatchSize := cmd.Flag("max-merge-batch-size", "Bytes loaded in one batch for merging. This is to limit the amount of memory a merge query can use.").
		Default("64MB").Bytes()
	grpcBindAddr, grpcGracePeriod, grpcCert, grpcKey, grpcClientCA := extkingpin.RegisterGRPCFlags(cmd)
	queryTimeout := extkingpin.ModelDuration(cmd.Flag("query.timeout", "Maximum time to process query by query node.").
		Default("10s"))
	symbolServer := cmd.Flag("symbol-server", "Symbol server to request to symbolize native stacktraces. When not configured, non-symbolized stack traces will just show their memory address.").String()
	symbolCache := cmd.Flag("symbol-cache", "Directory to use to cache symbol data from object storage.").
		Default("/tmp").String()
	objStoreConfig := *extkingpin.RegisterCommonObjStoreFlags(cmd, "", false, "When not set, the gRPC server will be started without serving the symbol management service.")
	reqLogConfig := extkingpin.RegisterRequestLoggingFlags(cmd)

	m[name] = func(comp component.Component, g *run.Group, mux httpMux, probe prober.Probe, logger log.Logger, reg *prometheus.Registry, debugLogging bool) (prober.Probe, error) {
		httpLogOpts, err := logging.ParseHTTPOptions("", reqLogConfig)
		if err != nil {
			return probe, errors.Wrap(err, "error while parsing config for request logging")
		}

		tagOpts, grpcLogOpts, err := logging.ParsegRPCOptions("", reqLogConfig)
		if err != nil {
			return probe, errors.Wrap(err, "error while parsing config for request logging")
		}

		return runAll(
			comp,
			g,
			mux,
			probe,
			reg,
			logger,
			httpLogOpts,
			grpcLogOpts,
			tagOpts,
			*storagePath,
			*configFile,
			time.Duration(*retention),
			reloadCh,
			reloaders,
			int64(*maxMergeBatchSize),
			*queryTimeout,
			*symbolServer,
			*symbolCache,
			objStoreConfig,
			&grpcSettings{
				grpcBindAddr:    *grpcBindAddr,
				grpcGracePeriod: time.Duration(*grpcGracePeriod),
				grpcCert:        *grpcCert,
				grpcKey:         *grpcKey,
				grpcClientCA:    *grpcClientCA,
			},
		)
	}
}

func runAll(
	comp component.Component,
	g *run.Group,
	mux httpMux,
	probe prober.Probe,
	reg *prometheus.Registry,
	logger log.Logger,
	httpLogOpts []logging.Option,
	grpcLogOpts []grpc_logging.Option,
	tagOpts []tags.Option,
	storagePath,
	configFile string,
	retention time.Duration,
	reloadCh chan struct{},
	reloaders *configReloaders,
	maxMergeBatchSize int64,
	queryTimeout model.Duration,
	symbolServer string,
	symbolCache string,
	objStoreConfig extflag.PathOrContent,
	srv *grpcSettings,
) (prober.Probe, error) {
	lazyDBOpener := &lazyDB{}
	scrapeManager := scrape.NewManager(log.With(logger, "component", "scrape-manager"), lazyDBOpener)

	confContentYaml, err := objStoreConfig.Content()
	if err != nil {
		return nil, err
	}

	var symStore storepb.SymbolStoreServer
	if len(confContentYaml) > 0 {
		bkt, err := objstore.NewBucket(logger, confContentYaml, reg, comp.String())
		if err != nil {
			return nil, errors.Wrap(err, "create object store bucket client")
		}
		symStore = symbol.NewSymbolStore(logger, bkt, symbolCache)
	}

	var sym *symbol.Symbolizer
	if symbolServer != "" {
		level.Debug(logger).Log("msg", "configuring symbol server", "url", symbolServer)
		conn, err := grpc.Dial(symbolServer, grpc.WithInsecure())
		if err != nil {
			return nil, err
		}
		c := storepb.NewSymbolizeClient(conn)
		sym = symbol.NewSymbolizer(logger, c)
	}
	if symStore != nil {
		level.Debug(logger).Log("msg", "using in-process symbol server")
		sym = symbol.NewSymbolizer(logger, symStore)
	}

	w := NewWeb(mux, lazyDBOpener, maxMergeBatchSize, queryTimeout,
		WebLogger(logger),
		WebRegistry(reg),
		WebReloaders(reloaders),
		WebTargets(func(ctx context.Context) conprofapi.TargetRetriever {
			return scrapeManager
		}),
		WebSymbolizer(sym),
		WebLogOpts(httpLogOpts...),
	)
	if err = w.Run(context.TODO(), reloadCh); err != nil {
		return nil, err
	}

	grpcProbe := prober.NewGRPC()
	statusProber := prober.Combine(
		probe,
		grpcProbe,
		prober.NewInstrumentation(comp, logger, extprom.WrapRegistererWithPrefix("conprof_", reg)),
	)
	maxBytesPerFrame := 1024 * 1024 * 2 // 2 Mb default, might need to be tuned later on.
	s := store.NewProfileStore(logger, lazyDBOpener, maxBytesPerFrame)

	gsrv := grpcserver.New(logger, reg, &opentracing.NoopTracer{}, grpcLogOpts, tagOpts, comp, grpcProbe,
		grpcserver.WithServer(store.RegisterReadableStoreServer(s)),
		grpcserver.WithServer(store.RegisterWritableStoreServer(s)),
		grpcserver.WithServer(store.RegisterSymbolStore(symStore)),
		grpcserver.WithListen(srv.grpcBindAddr),
		grpcserver.WithGracePeriod(srv.grpcGracePeriod),
		grpcserver.WithGRPCServerOption(
			grpc.ChainUnaryInterceptor(
				otelgrpc.UnaryServerInterceptor(),
			),
		),
		grpcserver.WithGRPCServerOption(
			grpc.ChainStreamInterceptor(
				otelgrpc.StreamServerInterceptor(),
			),
		),
	)

	ctx, cancelSampler := context.WithCancel(context.Background())

	g.Add(func() error {
		db, err := tsdb.Open(
			storagePath,
			logger,
			prometheus.DefaultRegisterer,
			&tsdb.Options{
				RetentionDuration:      retention.Milliseconds(),
				WALSegmentSize:         wal.DefaultSegmentSize,
				MinBlockDuration:       tsdb.DefaultBlockDuration,
				MaxBlockDuration:       retention.Milliseconds() / 10,
				NoLockfile:             true,
				AllowOverlappingBlocks: false,
				WALCompression:         true,
				StripeSize:             tsdb.DefaultStripeSize,
			},
		)
		if err != nil {
			return err
		}

		lazyDBOpener.appender = db
		lazyDBOpener.queryable = db
		lazyDBOpener.chunks = db

		sampler, err := NewSampler(db, reloaders,
			SamplerScraper(scrapeManager),
			SamplerConfig(configFile),
		)
		if err != nil {
			return err
		}

		if err := sampler.Run(ctx, g, reloadCh); err != nil {
			return err
		}

		// TODO: Don't pass *run.Group but rather run individual ones so we don't need this trickery
		// block this group until it's supposed to finish
		select {
		case <-ctx.Done():
			return nil
		}
	}, func(err error) {
		cancelSampler()
		level.Error(logger).Log("msg", "failed to start conprof components", "err", err)
	})

	g.Add(func() error {
		statusProber.Ready()
		return gsrv.ListenAndServe()
	}, func(err error) {
		grpcProbe.NotReady(err)
		gsrv.Shutdown(err)
	})

	return statusProber, nil
}

// lazyDB is a shim that returns noop implementations for TSDB interfaces until the actual TSDB is opened.
type lazyDB struct {
	appender  scrape.Appendable
	queryable storage.Queryable
	chunks    storage.ChunkQueryable
}

func (l *lazyDB) Querier(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
	if l.queryable == nil {
		return storage.NoopQuerier(), nil
	}
	return l.queryable.Querier(ctx, mint, maxt)
}

func (l *lazyDB) Appender(ctx context.Context) storage.Appender {
	if l.appender == nil {
		return &noopAppender{}
	}
	return l.appender.Appender(ctx)
}

func (l *lazyDB) ChunkQuerier(ctx context.Context, mint, maxt int64) (storage.ChunkQuerier, error) {
	if l.chunks == nil {
		return &noopChunkQuerier{}, nil
	}
	return l.chunks.ChunkQuerier(ctx, mint, maxt)
}

// TODO: Might make sense to move next to the storage.NoopQuerier

type noopAppender struct{}

func (n noopAppender) Add(l labels.Labels, t int64, v []byte) (uint64, error) { return 0, nil }

func (n noopAppender) AddFast(ref uint64, t int64, v []byte) error { return nil }

func (n noopAppender) Commit() error { return nil }

func (n noopAppender) Rollback() error { return nil }

type noopChunkQuerier struct{}

func (n noopChunkQuerier) LabelValues(name string) ([]string, storage.Warnings, error) {
	return nil, nil, nil
}

func (n noopChunkQuerier) LabelNames() ([]string, storage.Warnings, error) {
	return nil, nil, nil
}

func (n noopChunkQuerier) Close() error {
	return nil
}

func (n noopChunkQuerier) Select(sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.ChunkSeriesSet {
	return nil
}
