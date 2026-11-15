// Command amber_backfill writes the past prices of the Amber API as the
// samples that amber_exporter would have exported, for a load into
// Prometheus with "promtool tsdb create-blocks-from openmetrics".
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/prometheus/common/version"

	"github.com/goodtune/amber_exporter/amber"
	"github.com/goodtune/amber_exporter/collector"
)

const (
	name      = "amber_backfill"
	apiKeyEnv = "AMBER_API_KEY"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, name+":", err)
		os.Exit(1)
	}
}

func run() error {
	app := kingpin.New(name, "Write past Amber Electric prices in the OpenMetrics format, for a backfill of Prometheus.")
	var (
		apiURL = app.Flag("amber.api-url", "Base URL of the Amber API.").
			Default(amber.DefaultBaseURL).String()
		apiKeyFile = app.Flag("amber.api-key-file", "File that holds the Amber API key. Default: the key is read from the "+apiKeyEnv+" environment variable.").
				String()
		apiTimeout = app.Flag("amber.timeout", "Timeout of one request to the Amber API.").
				Default("1m").Duration()
		siteIDs = app.Flag("amber.site-id", "Site to backfill. Can be repeated. Default: all active sites.").
			Strings()
		job = app.Flag("job", "Value of the job label: the job_name of the scrape configuration.").
			Default("amber").String()
		instance = app.Flag("instance", "Value of the instance label: the target of the scrape configuration, for example amber_exporter:10062.").
				Required().String()
		end = app.Flag("end", "End of the backfill in the RFC 3339 format, for example 2026-11-15T03:00:00Z. Set it to the time of the first scrape. No sample is written at or after this time. Default: now.").
			String()
		days = app.Flag("days", "Number of days before the end to backfill.").
			Default("30").Int()
		step = app.Flag("step", "Time between two samples: the scrape interval.").
			Default("1m").Duration()
		output = app.Flag("output", "File to write. Default: the standard output.").
			Short('o').String()
	)
	app.Version(version.Print(name))
	app.HelpFlag.Short('h')
	kingpin.MustParse(app.Parse(os.Args[1:]))

	endTime := time.Now()
	if *end != "" {
		var err error
		if endTime, err = time.Parse(time.RFC3339, *end); err != nil {
			return fmt.Errorf("--end: %w", err)
		}
	}
	if *days < 1 {
		return errors.New("--days must be 1 or more")
	}
	if *step < time.Second {
		return errors.New("--step must be 1s or more")
	}
	startTime := endTime.AddDate(0, 0, -*days)

	apiKey, err := readAPIKey(*apiKeyFile)
	if err != nil {
		return err
	}
	client := amber.NewClient(apiKey,
		amber.WithBaseURL(*apiURL),
		amber.WithHTTPClient(&http.Client{Timeout: *apiTimeout}),
		amber.WithUserAgent(version.ComponentUserAgent(name)),
	)
	ctx := context.Background()

	if len(*siteIDs) == 0 {
		sites, err := client.Sites(ctx)
		if err != nil {
			return fmt.Errorf("get sites: %w", err)
		}
		for _, s := range sites {
			if s.Status == amber.StatusActive {
				*siteIDs = append(*siteIDs, s.ID)
			}
		}
		if len(*siteIDs) == 0 {
			return errors.New("the account has no active site")
		}
	}

	history := collector.NewPriceHistory(collector.HistoryOptions{
		Job:      *job,
		Instance: *instance,
		Start:    startTime,
		End:      endTime,
		Step:     *step,
	})
	for _, id := range *siteIDs {
		intervals, err := collector.FetchPriceHistory(ctx, client, id, startTime, endTime)
		if err != nil {
			return fmt.Errorf("site %s: get prices: %w", id, err)
		}
		fmt.Fprintf(os.Stderr, "%s: site %s: %d intervals from the API\n", name, id, len(intervals))
		history.Add(id, intervals)
	}

	var w io.Writer = os.Stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	if err := history.Write(w); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "%s: wrote samples from %s to %s\n", name,
		startTime.UTC().Format(time.RFC3339), endTime.UTC().Format(time.RFC3339))
	return nil
}

// readAPIKey returns the API key from the file, or from the environment
// when no file is given, as amber_exporter does.
func readAPIKey(file string) (string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		if key := strings.TrimSpace(string(b)); key != "" {
			return key, nil
		}
		return "", fmt.Errorf("%s is empty", file)
	}
	if key := strings.TrimSpace(os.Getenv(apiKeyEnv)); key != "" {
		return key, nil
	}
	return "", errors.New("set " + apiKeyEnv + " or --amber.api-key-file")
}
