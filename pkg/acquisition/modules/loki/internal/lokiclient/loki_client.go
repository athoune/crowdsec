package lokiclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"gopkg.in/tomb.v2"

	"github.com/crowdsecurity/crowdsec/pkg/cwversion"
)

type LokiClient struct {
	Logger *log.Entry

	config Config
}

type Config struct {
	LokiURL    string
	LokiPrefix string
	Query      string
	Headers    map[string]string

	Username string
	Password string

	Since        time.Duration
	Until        time.Duration
	WaitForReady time.Duration

	Limit int
}

func (lc *LokiClient) tailLogs(ctx context.Context, conn *websocket.Conn, c chan *LokiResponse, t *tomb.Tomb) error {
	tick := time.NewTicker(100 * time.Millisecond)

	for {
		select {
		case <-t.Dying():
			lc.Logger.Info("LokiClient tomb is dying, closing connection")
			tick.Stop()
			return conn.Close()
		case <-ctx.Done(): // this is technically useless, as the read from the websocket is blocking :(
			lc.Logger.Info("LokiClient context is done, closing connection")
			tick.Stop()
			return conn.Close()
		case <-tick.C:
			lc.Logger.Debug("Reading from WS")
			jsonResponse := &LokiResponse{}
			err := conn.ReadJSON(jsonResponse)
			if err != nil {
				lc.Logger.Errorf("Error reading from WS: %s", err)
				close(c)
				return err
			}
			lc.Logger.Tracef("Read from WS: %v", jsonResponse)
			c <- jsonResponse
			lc.Logger.Debug("Sent response to channel")
		}
	}
}

func (lc *LokiClient) queryRange(ctx context.Context, uri string, c chan *LokiQueryRangeResponse, t *tomb.Tomb) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.Dying():
			return t.Err()
		default:
			lc.Logger.Debugf("Querying Loki: %s", uri)
			resp, err := http.Get(uri)

			if err != nil {
				return errors.Wrapf(err, "error querying range")
			}
			if resp.StatusCode != 200 {
				body, _ := ioutil.ReadAll(resp.Body)
				resp.Body.Close()
				return errors.Wrapf(err, "bad HTTP response code: %d: %s", resp.StatusCode, string(body))
			}

			var lq LokiQueryRangeResponse

			json.NewDecoder(resp.Body).Decode(&lq)
			resp.Body.Close()

			lc.Logger.Tracef("Got response: %+v", lq)

			c <- &lq

			if len(lq.Data.Result) == 0 || len(lq.Data.Result[0].Entries) < lc.config.Limit {
				lc.Logger.Infof("Got less than %d results (%d), stopping", lc.config.Limit, len(lq.Data.Result))
				close(c)
				return nil
			}
			// Can we assume we will always have only one stream?
			lastTs := lq.Data.Result[0].Entries[len(lq.Data.Result[0].Entries)-1].Timestamp

			lc.Logger.Infof("Got %d results, last timestamp: %s (converted: %d)", len(lq.Data.Result[0].Entries), lastTs, strconv.Itoa(lastTs.Nanosecond()))
			u, err := url.Parse(uri) // we can ignore the error, we know it's valid
			if err != nil {
				return errors.Wrapf(err, "error parsing URL")
			}
			queryParams := u.Query()
			queryParams.Set("start", strconv.Itoa(int(lastTs.UnixNano())))
			u.RawQuery = queryParams.Encode()
			uri = u.String()
		}
	}
}

func (lc *LokiClient) getURLFor(endpoint string, params map[string]string) string {
	u, err := url.Parse(lc.config.LokiURL)
	if err != nil {
		return ""
	}
	queryParams := u.Query()
	for k, v := range params {
		queryParams.Set(k, v)
	}
	u.RawQuery = queryParams.Encode()

	u.Path = filepath.Join(lc.config.LokiPrefix, u.Path, endpoint)

	switch endpoint {
	case "loki/api/v1/tail":
		if u.Scheme == "http" {
			u.Scheme = "ws"
		} else {
			u.Scheme = "wss"
		}
	}
	return u.String()
}

func (lc *LokiClient) Ready(ctx context.Context, t *tomb.Tomb) error {
	tick := time.NewTicker(500 * time.Millisecond)
	url := lc.getURLFor("ready", nil)
	for {
		select {
		case <-ctx.Done():
			tick.Stop()
			return ctx.Err()
		case <-t.Dying():
			tick.Stop()
			return t.Err()
		case <-tick.C:
			lc.Logger.Debug("Checking if Loki is ready")
			resp, err := http.Get(url)
			if err != nil {
				lc.Logger.Warnf("Error checking if Loki is ready: %s", err)
				continue
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				lc.Logger.Debugf("Loki is not ready, status code: %d", resp.StatusCode)
				continue
			}
			lc.Logger.Info("Loki is ready")
			return nil
		}
	}
}

func (lc *LokiClient) Tail(ctx context.Context, t *tomb.Tomb) (chan *LokiResponse, error) {
	responseChan := make(chan *LokiResponse)
	dialer := &websocket.Dialer{} // TODO: TLS support
	u := lc.getURLFor("loki/api/v1/tail", map[string]string{
		"limit": strconv.Itoa(lc.config.Limit),
		"start": strconv.Itoa(int(time.Now().Add(-lc.config.Since).UnixNano())),
		"query": lc.config.Query,
	})

	lc.Logger.Debugf("Since: %s (%s)", lc.config.Since, time.Now().Add(-lc.config.Since))

	if lc.config.Username != "" || lc.config.Password != "" {
		dialer.Proxy = func(req *http.Request) (*url.URL, error) {
			req.SetBasicAuth(lc.config.Username, lc.config.Password)
			return nil, nil
		}
	}

	requestHeader := http.Header{}
	for k, v := range lc.config.Headers {
		requestHeader.Add(k, v)
	}
	requestHeader.Set("User-Agent", "Crowdsec "+cwversion.Version)
	lc.Logger.Infof("Connecting to %s", u)
	conn, resp, err := dialer.DialContext(ctx, u, requestHeader)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			buf, err2 := ioutil.ReadAll(resp.Body)
			if err2 != nil {
				return nil, fmt.Errorf("error reading response body while handling WS error: %s (%s)", err, err2)
			}
			return nil, fmt.Errorf("error dialing WS: %s: %s", err, string(buf))
		}
		return nil, err
	}
	defer resp.Body.Close()
	t.Go(func() error {
		defer conn.Close()
		return lc.tailLogs(ctx, conn, responseChan, t)
	})

	return responseChan, nil
}

func (lc *LokiClient) QueryRange(ctx context.Context, t *tomb.Tomb) chan *LokiQueryRangeResponse {
	url := lc.getURLFor("loki/api/v1/query_range", map[string]string{
		"query":     lc.config.Query,
		"start":     strconv.Itoa(int(time.Now().Add(-lc.config.Since).UnixNano())),
		"end":       strconv.Itoa(int(time.Now().UnixNano())),
		"limit":     strconv.Itoa(lc.config.Limit),
		"direction": "forward",
	})

	c := make(chan *LokiQueryRangeResponse)

	lc.Logger.Debugf("Since: %s (%s)", lc.config.Since, time.Now().Add(-lc.config.Since))

	requestHeader := http.Header{}
	for k, v := range lc.config.Headers {
		requestHeader.Add(k, v)
	}

	if lc.config.Username != "" || lc.config.Password != "" {
		requestHeader.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(lc.config.Username+":"+lc.config.Password)))
	}

	requestHeader.Set("User-Agent", "Crowdsec "+cwversion.Version)
	lc.Logger.Infof("Connecting to %s", url)
	t.Go(func() error {
		return lc.queryRange(ctx, url, c, t)
	})
	return c
}

func NewLokiClient(config Config) *LokiClient {
	return &LokiClient{Logger: log.WithField("component", "lokiclient"), config: config}
}
