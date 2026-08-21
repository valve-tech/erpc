package erpc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/architecture/svm"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/erpc/erpc/telemetry"
	"github.com/erpc/erpc/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
)

// ErrServerFailed marks an Init failure that came from one of the transports
// eRPC exposes: the HTTP server, the gRPC server or the metrics server. Init is
// library code, so it reports the failure instead of ending the process;
// cmd/erpc matches this and exits with util.ExitCodeHttpServerFailed.
var ErrServerFailed = errors.New("erpc server failed")

func Init(
	appCtx context.Context,
	cfg *common.Config,
	logger zerolog.Logger,
) error {
	//
	// 1) Set the right log level depending on the configuration
	//
	level, err := zerolog.ParseLevel(cfg.LogLevel)
	if err != nil {
		logger.Warn().Msgf("invalid log level '%s', defaulting to 'debug': %s", cfg.LogLevel, err)
		level = zerolog.DebugLevel
	} else {
		logger = logger.Level(level)
	}

	if logger.GetLevel() <= zerolog.InfoLevel {
		finalCfgJson, err := common.SonicCfg.Marshal(cfg)
		if err != nil {
			logger.Warn().Msgf("failed to marshal final configuration for tracing: %v", err)
		} else {
			logger.Info().RawJSON("config", finalCfgJson).Msg("")
		}
	}

	//
	// 2) Set the right histogram buckets and label filter
	//
	bucketStr := ""
	if cfg.Metrics != nil {
		if cfg.Metrics.HistogramBuckets != "" {
			bucketStr = cfg.Metrics.HistogramBuckets
		}
		// Must run before SetHistogramBuckets so the new Vecs are built with the filter applied.
		telemetry.SetHistogramLabelFilter(cfg.Metrics.HistogramDropLabels, cfg.Metrics.HistogramLabelOverrides)
		if cfg.Metrics.CounterIdleEvictionAfter != nil {
			telemetry.SetCounterIdleEvictionAfter(cfg.Metrics.CounterIdleEvictionAfter.Duration())
		}
		// Counters are built unregistered at package init (Prometheus freezes
		// a metric's label-set hash for the life of the registry, so we cannot
		// register first and filter later). Install any filter, then register
		// once under it. Only when metrics config is present — processes that
		// never scrape do not need these collectors on DefaultRegisterer, and
		// tests that call Init without metrics must not freeze the full label
		// set before a later Init can apply counterDropLabels.
		if len(cfg.Metrics.CounterDropLabels) > 0 || len(cfg.Metrics.CounterLabelOverrides) > 0 {
			telemetry.SetCounterLabelFilter(cfg.Metrics.CounterDropLabels, cfg.Metrics.CounterLabelOverrides)
		}
		telemetry.RebuildFilteredCounters()
	}
	if err := telemetry.SetHistogramBuckets(bucketStr); err != nil {
		logger.Warn().Err(err).Msg("failed to set histogram buckets, using defaults")
	}

	// Install a global networkId -> alias resolver so network-labeled metrics from
	// components that only know the raw networkId (e.g. the gRPC cache connector,
	// which discovers networks by chainId) use the same alias as every other metric.
	if cfg != nil {
		aliasByNetworkId := make(map[string]string)
		for _, p := range cfg.Projects {
			if p == nil {
				continue
			}
			for _, n := range p.Networks {
				if n != nil && n.Evm != nil && n.Evm.ChainId != 0 && n.Alias != "" {
					aliasByNetworkId[util.EvmNetworkId(n.Evm.ChainId)] = n.Alias
				}
			}
		}
		if len(aliasByNetworkId) > 0 {
			common.SetNetworkAliasResolver(func(networkId string) string { return aliasByNetworkId[networkId] })
		}
	}

	//
	// 3) Initialize eRPC
	//
	logger.Info().Msg("initializing eRPC core")
	var evmJsonRpcCache *evm.EvmJsonRpcCache
	var svmJsonRpcCache *svm.SvmJsonRpcCache
	var sharedState data.SharedStateRegistry
	if cfg.Database != nil {
		if cfg.Database.EvmJsonRpcCache != nil {
			evmJsonRpcCache, err = evm.NewEvmJsonRpcCache(appCtx, &logger, cfg.Database.EvmJsonRpcCache)
			if err != nil {
				logger.Warn().Msgf("failed to initialize evm json rpc cache: %v", err)
			}
		}
		if cfg.Database.SvmJsonRpcCache != nil {
			svmJsonRpcCache, err = svm.NewSvmJsonRpcCache(appCtx, &logger, cfg.Database.SvmJsonRpcCache)
			if err != nil {
				logger.Warn().Msgf("failed to initialize svm json rpc cache: %v", err)
			}
		}
		if cfg.Database.SharedState != nil {
			sharedState, err = data.NewSharedStateRegistry(appCtx, &logger, cfg.Database.SharedState)
			if err != nil {
				logger.Warn().Msgf("failed to initialize shared state registry: %v", err)
			}
		}
	}
	erpcInstance, err := NewERPC(appCtx, &logger, sharedState, evmJsonRpcCache, svmJsonRpcCache, cfg)
	if err != nil {
		return err
	}

	// Bootstrap core before starting servers so routes are ready
	erpcInstance.Bootstrap(appCtx)

	//
	// 4) Expose Transports
	//
	// Each transport serves in its own goroutine, so its failure has to travel
	// back to whoever called Init. serverFailed carries it. The buffer holds
	// one slot per transport, so a goroutine that fails after Init has already
	// returned writes its error and exits instead of leaking.
	serverFailed := make(chan error, 3)
	logger.Info().Msg("initializing transports")
	if cfg.Server != nil {
		httpServer, err := NewHttpServer(appCtx, &logger, cfg.Server, cfg.HealthCheck, cfg.Admin, cfg.Indexer, erpcInstance)
		if err != nil {
			return err
		}
		go func() {
			if err := httpServer.Start(&logger); err != nil {
				if err != http.ErrServerClosed {
					serverFailed <- fmt.Errorf("%w: http: %w", ErrServerFailed, err)
				}
			}
		}()
	}
	if cfg.Server != nil && cfg.Server.GrpcEnabled != nil && *cfg.Server.GrpcEnabled && !grpcSharesHttpV4(cfg.Server) {
		grpcServer, err := NewGrpcServer(appCtx, &logger, cfg.Server, erpcInstance)
		if err != nil {
			return err
		}
		go func() {
			if err := grpcServer.Start(&logger); err != nil {
				serverFailed <- fmt.Errorf("%w: grpc: %w", ErrServerFailed, err)
			}
		}()
	}
	if cfg.Metrics != nil && cfg.Metrics.Enabled != nil && *cfg.Metrics.Enabled {
		if cfg.Metrics.ErrorLabelMode != "" {
			common.SetErrorLabelMode(cfg.Metrics.ErrorLabelMode)
		}
		if cfg.Metrics.Port == nil {
			return fmt.Errorf("metrics.port is not configured")
		}
		logger.Info().Msgf("starting metrics server on port: %d", *cfg.Metrics.Port)
		srv := &http.Server{
			BaseContext: func(ln net.Listener) context.Context {
				return appCtx
			},
			Addr:              fmt.Sprintf(":%d", *cfg.Metrics.Port),
			Handler:           promhttp.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				serverFailed <- fmt.Errorf("%w: metrics: %w", ErrServerFailed, err)
			}
		}()
		go func() {
			<-appCtx.Done()
			logger.Info().Msg("shutting down metrics server...")
			shutdownCtx, cancel := context.WithTimeout(appCtx, 5*time.Second)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				logger.Error().Msgf("metrics server forced to shutdown: %s", err)
			} else {
				logger.Info().Msg("metrics server stopped")
			}
		}()
	}

	// Wait until the context is cancelled, or a transport fails. A transport
	// failure ends Init with the error: Init is library code, so the caller
	// decides what a dead listener means. cmd/erpc exits with
	// util.ExitCodeHttpServerFailed; an embedder can do something else.
	var serverErr error
	select {
	case <-appCtx.Done():
		logger.Info().Msg("shutting down gracefully...")
	case serverErr = <-serverFailed:
		logger.Error().Err(serverErr).Msg("a server failed, returning to the caller")
	}
	// Flush buffered integrity forensics before the process goes away; the S3
	// exporter otherwise loses everything written since its last interval.
	evm.CloseIntegrityExporters()
	if serverErr != nil {
		return serverErr
	}
	// Give the http server some time to finish draining.
	if cfg.Server != nil && cfg.Server.WaitAfterShutdown != nil {
		time.Sleep(cfg.Server.WaitAfterShutdown.Duration())
	}

	return nil
}
