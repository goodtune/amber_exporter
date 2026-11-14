// Command amber_exporter exports Amber Electric prices, forecasts and usage
// as Prometheus metrics.
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	versioncollector "github.com/prometheus/client_golang/prometheus/collectors/version"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promslog"
	"github.com/prometheus/common/promslog/flag"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	"github.com/prometheus/exporter-toolkit/web/kingpinflag"

	"github.com/goodtune/amber_exporter/amber"
	"github.com/goodtune/amber_exporter/collector"
)

const (
	exporterName = "amber_exporter"
	// apiKeyEnv is the environment variable that holds the API key.
	apiKeyEnv = "AMBER_API_KEY"
	// exitNoAPIKey is the exit status when there is no API key. The systemd
	// unit does not start the exporter again after this status.
	exitNoAPIKey = 2
)

func main() {
	setDefaultVersion()
	os.Exit(run())
}

// setDefaultVersion gives the build a version when the linker flags of
// GoReleaser or the Makefile did not set one, as with a plain "go build" or
// "go install". amber_exporter_build_info then always says
// which build made the data.
func setDefaultVersion() {
	if version.Version != "" {
		return
	}
	version.Version = "unknown"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		// A tagged version for "go install module@version", "(devel)" or a
		// pseudo-version for a build from a checkout.
		version.Version = info.Main.Version
	}
}

func run() int {
	app := kingpin.New(exporterName, "Prometheus exporter for Amber Electric prices, forecasts and usage.")
	var (
		metricsPath = app.Flag("web.telemetry-path", "Path under which to expose metrics.").
				Default("/metrics").String()
		disableExporterMetrics = app.Flag("web.disable-exporter-metrics", "Exclude metrics about the exporter itself (promhttp_*, process_*, go_*).").
					Bool()
		apiURL = app.Flag("amber.api-url", "Base URL of the Amber API.").
			Default(amber.DefaultBaseURL).String()
		apiKeyFile = app.Flag("amber.api-key-file", "File that holds the Amber API key. Default: the key is read from the "+apiKeyEnv+" environment variable.").
				String()
		apiTimeout = app.Flag("amber.timeout", "Timeout of one request to the Amber API.").
				Default("15s").Duration()
		toolkitFlags = kingpinflag.AddFlags(app, ":10062")
	)
	cfg := collector.DefaultConfig()
	cfg.RegisterFlags(app)

	promslogConfig := &promslog.Config{}
	flag.AddFlags(app, promslogConfig)
	app.Version(version.Print(exporterName))
	app.HelpFlag.Short('h')
	kingpin.MustParse(app.Parse(os.Args[1:]))
	logger := promslog.New(promslogConfig)

	logger.Info("Starting "+exporterName, "version", version.Info())
	logger.Info("Build context", "build_context", version.BuildContext())

	apiKey, err := readAPIKey(*apiKeyFile)
	if err != nil {
		logger.Error("Cannot read the Amber API key", "err", err)
		return exitNoAPIKey
	}

	apiMetrics := collector.NewAPIMetrics()
	client := amber.NewClient(apiKey,
		amber.WithBaseURL(*apiURL),
		amber.WithHTTPClient(&http.Client{Timeout: *apiTimeout}),
		amber.WithUserAgent(version.ComponentUserAgent(exporterName)),
		amber.WithRequestObserver(apiMetrics.Observe),
	)
	apiMetrics.SetRateLimitSource(client.RateLimit)

	amberCollector, err := collector.New(client, cfg, logger)
	if err != nil {
		logger.Error("Cannot create the collector", "err", err)
		return 1
	}
	logger.Info("Enabled collectors", "collectors", strings.Join(amberCollector.Names(), ","))

	registry := prometheus.NewRegistry()
	registry.MustRegister(amberCollector, apiMetrics, versioncollector.NewCollector(exporterName))
	handlerOpts := promhttp.HandlerOpts{
		ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandling: promhttp.ContinueOnError,
	}
	var metricsHandler http.Handler
	if *disableExporterMetrics {
		metricsHandler = promhttp.HandlerFor(registry, handlerOpts)
	} else {
		registry.MustRegister(
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
			collectors.NewGoCollector(),
		)
		handlerOpts.Registry = registry
		metricsHandler = promhttp.InstrumentMetricHandler(registry, promhttp.HandlerFor(registry, handlerOpts))
	}

	mux := http.NewServeMux()
	mux.Handle(*metricsPath, metricsHandler)
	if *metricsPath != "/" {
		landingPage, err := web.NewLandingPage(web.LandingConfig{
			Name:        "Amber Exporter",
			Description: "Prometheus exporter for Amber Electric prices, forecasts and usage",
			Version:     version.Info(),
			Links:       []web.LandingLinks{{Address: *metricsPath, Text: "Metrics"}},
		})
		if err != nil {
			logger.Error("Cannot create the landing page", "err", err)
			return 1
		}
		mux.Handle("/", landingPage)
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := web.ListenAndServe(server, toolkitFlags, logger); err != nil {
		logger.Error("The web server stopped", "err", err)
		return 1
	}
	return 0
}

// readAPIKey returns the API key from the file, or from the environment
// when no file is given. The key is not accepted as a flag, because flags
// are visible in the process list.
func readAPIKey(file string) (string, error) {
	var key string
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		key = strings.TrimSpace(string(b))
		if key == "" {
			return "", fmt.Errorf("%s is empty", file)
		}
		return key, nil
	}
	key = strings.TrimSpace(os.Getenv(apiKeyEnv))
	if key == "" {
		return "", errors.New("set " + apiKeyEnv + " or --amber.api-key-file")
	}
	return key, nil
}
