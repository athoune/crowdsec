package loki

/*
https://grafana.com/docs/loki/latest/api/#get-lokiapiv1tail
*/

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	leaky "github.com/crowdsecurity/crowdsec/pkg/leakybucket"

	"github.com/crowdsecurity/crowdsec/pkg/acquisition/configuration"
	lokiclient "github.com/crowdsecurity/crowdsec/pkg/acquisition/modules/loki/internal/lokiclient"
	"github.com/crowdsecurity/crowdsec/pkg/types"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	tomb "gopkg.in/tomb.v2"
	"gopkg.in/yaml.v2"
)

const (
	readyTimeout time.Duration = 3 * time.Second
	readyLoop    int           = 3
	readySleep   time.Duration = 10 * time.Second
	lokiLimit    int           = 100
)

var linesRead = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "cs_lokisource_hits_total",
		Help: "Total lines that were read.",
	},
	[]string{"source"})

type LokiConfiguration struct {
	URL                               string            `yaml:"url"`    // Loki url
	Prefix                            string            `yaml:"prefix"` // Loki prefix
	Query                             string            `yaml:"query"`  // LogQL query
	Limit                             int               `yaml:"limit"`  // Limit of logs to read
	DelayFor                          time.Duration     `yaml:"delay_for"`
	Since                             time.Duration     `yaml:"since"`
	Headers                           map[string]string `yaml:"headers"`        // HTTP headers for talking to Loki
	WaitForReady                      time.Duration     `yaml:"wait_for_ready"` // Retry interval, default is 10 seconds
	Username                          string            `yaml:"username"`
	Password                          string            `yaml:"password"`
	configuration.DataSourceCommonCfg `yaml:",inline"`
}

type LokiSource struct {
	Config LokiConfiguration

	client *lokiclient.LokiClient

	logger        *log.Entry
	lokiWebsocket string
}

func (l *LokiSource) GetMetrics() []prometheus.Collector {
	return []prometheus.Collector{linesRead}
}

func (l *LokiSource) GetAggregMetrics() []prometheus.Collector {
	return []prometheus.Collector{linesRead}
}

func (l *LokiSource) Configure(config []byte, logger *log.Entry) error {
	l.Config = LokiConfiguration{}
	l.logger = logger
	err := yaml.UnmarshalStrict(config, &l.Config)
	if err != nil {
		return errors.Wrap(err, "Cannot parse LokiAcquisition configuration")
	}

	if l.Config.Query == "" {
		return errors.New("Loki query is mandatory")
	}

	if l.Config.WaitForReady == 0 {
		l.Config.WaitForReady = 10 * time.Second
	}
	if l.Config.Mode == "" {
		l.Config.Mode = configuration.TAIL_MODE
	}
	if l.Config.Prefix == "" {
		l.Config.Prefix = "/"
	}

	if !strings.HasSuffix(l.Config.Prefix, "/") {
		l.Config.Prefix = l.Config.Prefix + "/"
	}

	if l.Config.Limit == 0 {
		l.Config.Limit = lokiLimit
	}

	if l.Config.Mode == configuration.TAIL_MODE {
		l.logger.Infof("Resetting since")
		l.Config.Since = 0
	}

	l.logger.Infof("Since value: %s", l.Config.Since.String())

	clientConfig := lokiclient.Config{
		LokiURL: l.Config.URL,
		Headers: l.Config.Headers,
		Limit:   l.Config.Limit,
		Query:   l.Config.Query,
		Since:   l.Config.Since,
	}

	l.client = lokiclient.NewLokiClient(clientConfig)
	l.client.Logger = logger.WithField("component", "lokiclient")
	return nil
}

func (l *LokiSource) ConfigureByDSN(dsn string, labels map[string]string, logger *log.Entry) error {
	l.logger = logger
	l.Config = LokiConfiguration{}
	l.Config.Mode = configuration.CAT_MODE
	l.Config.Labels = labels

	u, err := url.Parse(dsn)
	if err != nil {
		return errors.Wrap(err, "can't parse dsn configuration : "+dsn)
	}
	if u.Scheme != "loki" {
		return fmt.Errorf("invalid DSN %s for loki source, must start with loki://", dsn)
	}
	if u.Host == "" {
		return errors.New("Empty loki host")
	}
	scheme := "http"
	// FIXME how can use http with container, in a private network?
	if u.Host == "localhost" || u.Host == "127.0.0.1" || u.Host == "[::1]" {
		scheme = "http"
	}

	l.Config.URL = fmt.Sprintf("%s://%s", scheme, u.Host)
	params := u.Query()
	if q := params.Get("query"); q != "" {
		l.Config.Query = q
	}
	if w := params.Get("wait_for_ready"); w != "" {
		l.Config.WaitForReady, err = time.ParseDuration(w)
		if err != nil {
			return err
		}
	} else {
		l.Config.WaitForReady = 10 * time.Second
	}
	if d := params.Get("delay_for"); d != "" {
		delayFor, err := time.ParseDuration(d)
		if err != nil {
			return err
		}
		l.Config.DelayFor = delayFor
	}
	if s := params.Get("since"); s != "" {
		l.Config.Since, err = time.ParseDuration(s)
		if err != nil {
			return errors.Wrap(err, "can't parse since in DSN configuration")
		}
	}

	if limit := params.Get("limit"); limit != "" {
		limit, err := strconv.Atoi(limit)
		if err != nil {
			return errors.Wrap(err, "can't parse limit in DSN configuration")
		}
		l.Config.Limit = limit
	} else {
		l.Config.Limit = 5000 // max limit allowed by loki
	}

	if logLevel := params.Get("log_level"); logLevel != "" {
		level, err := log.ParseLevel(logLevel)
		if err != nil {
			return errors.Wrap(err, "can't parse log_level in DSN configuration")
		}
		l.Config.LogLevel = &level
		l.logger.Logger.SetLevel(level)
	}

	clientConfig := lokiclient.Config{
		LokiURL:  l.Config.URL,
		Headers:  l.Config.Headers,
		Limit:    l.Config.Limit,
		Query:    l.Config.Query,
		Since:    l.Config.Since,
		Username: l.Config.Username,
		Password: l.Config.Password,
	}

	l.client = lokiclient.NewLokiClient(clientConfig)
	l.client.Logger = logger.WithField("component", "lokiclient")

	return nil
}

func (l *LokiSource) GetMode() string {
	return l.Config.Mode
}

func (l *LokiSource) GetName() string {
	return "loki"
}

// OneShotAcquisition reads a set of file and returns when done
func (l *LokiSource) OneShotAcquisition(out chan types.Event, t *tomb.Tomb) error {
	l.logger.Debug("Loki one shot acquisition")
	readyCtx, cancel := context.WithTimeout(context.Background(), l.Config.WaitForReady)
	defer cancel()
	err := l.client.Ready(readyCtx)
	if err != nil {
		return errors.Wrap(err, "loki is not ready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := l.client.QueryRange(ctx)

	for {
		select {
		case <-t.Dying():
			l.logger.Debug("Loki one shot acquisition stopped")
			cancel()
			return nil
		case resp, ok := <-c:
			if !ok {
				l.logger.Info("Loki acuiqisition done, chan closed")
				cancel()
				return nil
			}
			for _, stream := range resp.Data.Result {
				for _, entry := range stream.Entries {
					l.readOneEntry(entry, l.Config.Labels, out)
				}
			}
		}
	}
}

func (l *LokiSource) readOneEntry(entry lokiclient.Entry, labels map[string]string, out chan types.Event) {
	ll := types.Line{}
	ll.Raw = entry.Line
	ll.Time = entry.Timestamp
	ll.Src = l.Config.URL
	ll.Labels = labels
	ll.Process = true
	ll.Module = l.GetName()

	linesRead.With(prometheus.Labels{"source": l.Config.URL}).Inc()
	out <- types.Event{
		Line:       ll,
		Process:    true,
		Type:       types.LOG,
		ExpectMode: leaky.TIMEMACHINE,
	}
}

func (l *LokiSource) StreamingAcquisition(out chan types.Event, t *tomb.Tomb) error {
	readyCtx, cancel := context.WithTimeout(context.Background(), l.Config.WaitForReady)
	defer cancel()
	err := l.client.Ready(readyCtx)
	if err != nil {
		return errors.Wrap(err, "loki is not ready")
	}
	ll := l.logger.WithField("websocket url", l.lokiWebsocket)
	t.Go(func() error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		respChan, err := l.client.Tail(ctx)
		if err != nil {
			ll.Errorf("could not start loki tail: %s", err)
			return errors.Wrap(err, "could not start loki tail")
		}
		for {
			select {
			case resp := <-respChan:
				if resp == nil {
					ll.Warnf("got nil response from loki tail")
					continue
				}
				if len(resp.DroppedEntries) > 0 {
					ll.Warnf("%d entries dropped from loki response", len(resp.DroppedEntries))
				}
				for _, stream := range resp.Streams {
					for _, entry := range stream.Entries {
						l.readOneEntry(entry, l.Config.Labels, out)
					}
				}
			case <-t.Dying():
				return nil
			}
		}
	})
	return nil
}

func (l *LokiSource) CanRun() error {
	return nil
}

func (l *LokiSource) Dump() interface{} {
	return l
}

// SupportedModes returns the supported modes by the acquisition module
func (l *LokiSource) SupportedModes() []string {
	return []string{configuration.TAIL_MODE, configuration.CAT_MODE}
}
