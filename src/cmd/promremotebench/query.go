// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/util/stats"

	xerrors "github.com/m3db/m3/src/x/errors"
	"go.uber.org/zap"
)

type queryExecutor struct {
	queryExecutorOptions
	client *http.Client
}

type queryExecutorOptions struct {
	URLs          []string
	Concurrency   int
	NumWriteHosts int
	NumSeries     int
	LoadStep      time.Duration
	LoadRange     time.Duration
	AccuracyStep  time.Duration
	AccuracyRange time.Duration
	Aggregation   string
	Labels        map[string]string
	Headers       map[string]string
	Sleep         time.Duration
	Debug         bool
	DebugLength   int
	Logger        *zap.Logger
}

func newQueryExecutor(opts queryExecutorOptions) *queryExecutor {
	return &queryExecutor{
		queryExecutorOptions: opts,
		client:               http.DefaultClient,
	}
}

func (q *queryExecutor) Run(checker Checker) {
	q.Logger.Info("query load configured",
		zap.Int("concurrency", q.Concurrency))
	for i := 0; i < q.Concurrency; i++ {
		go q.alertLoad(checker)
	}

	go q.accuracyCheck(checker)
}

// accuracyCheck checks the accuracy of data for one
// host at a time.
func (q *queryExecutor) accuracyCheck(checker Checker) {
	type label struct {
		name  string
		value string
	}
	labels := make([]label, 0, len(q.Labels))
	for k, v := range q.Labels {
		labels = append(labels, label{name: k, value: v})
	}

	query := new(strings.Builder)
	for i := 0; ; i++ {
		func() {
			// Exec in func to be able to use defer.
			if i > 0 {
				time.Sleep(q.Sleep)
			}

			query.Reset()
			if q.Aggregation != "" {
				mustWriteString(query, q.Aggregation)
				mustWriteString(query, "({")
			}

			curHostnames := checker.GetHostNames()
			if len(curHostnames) == 0 {
				if i > 0 {
					q.Logger.Error("no hosts returned in the checker, skipping accuracy check.")
				}
				return
			}

			var (
				dps          []Datapoint
				selectedHost string
			)

			for j := 0; j < 5; j++ {
				selectedHost = curHostnames[rand.Intn(len(curHostnames))]
				dps = checker.GetDatapoints(selectedHost)
				if len(dps) > 1 {
					break
				}
			}

			if len(dps) <= 1 && i > 1 {
				q.Logger.Error("couldn't find a host with more than 1 datapoint. Skipping accuracy check")
			}

			mustWriteString(query, "hostname=\""+selectedHost+"\"")

			// Write the common labels.
			for j := 0; j < len(labels); j++ {
				mustWriteString(query, ",")

				l := labels[j]
				mustWriteString(query, l.name)
				mustWriteString(query, "=\"")
				mustWriteString(query, l.value)
				mustWriteString(query, "\"")
			}

			if q.Aggregation != "" {
				mustWriteString(query, "})")
			}

			res, err := q.fanoutQuery(query, true, q.AccuracyRange, q.AccuracyStep)
			if len(res) == 0 {
				q.Logger.Error("invalid response for accuracy query")
			} else if err != nil {
				q.Logger.Error("fanout execution failed", zap.Error(err))
			} else {
				for _, result := range res {
					q.validateQuery(dps, result)
				}
			}
		}()
	}
}

func (q *queryExecutor) alertLoad(checker Checker) {
	// Select number of write hosts to select metrics from.
	numHosts := int(math.Ceil(float64(q.NumSeries) / 101.0))
	if numHosts < 1 {
		numHosts = 1
	}

	if numHosts > q.NumWriteHosts {
		q.Logger.Fatal("num series exceeds metrics emitted by write load num hosts",
			zap.Int("query-num-series", q.NumSeries),
			zap.Int("max-valid-query-num-series", q.NumWriteHosts*101),
			zap.Int("num-write-hosts", q.NumWriteHosts))
	}

	type label struct {
		name  string
		value string
	}
	labels := make([]label, 0, len(q.Labels))
	for k, v := range q.Labels {
		labels = append(labels, label{name: k, value: v})
	}

	pickedHosts := make(map[string]struct{})

	query := new(strings.Builder)
	for i := 0; ; i++ {
		func() {
			// Exec in func to be able to use defer.
			if i > 0 {
				time.Sleep(q.Sleep)
			}

			query.Reset()
			if q.Aggregation != "" {
				mustWriteString(query, q.Aggregation)
				mustWriteString(query, "({")
			}

			curHostnames := checker.GetHostNames()
			if len(curHostnames) == 0 {
				q.Logger.Error("no hosts returned in the checker, skipping load test round")
				return
			}

			// Now we pick a few hosts to select metrics from, each should return 101 metrics.
			for k := range pickedHosts {
				delete(pickedHosts, k) // Reuse pickedHosts
			}
			mustWriteString(query, "hostname=~\"(")
			for j := 0; j < numHosts; j++ {
				hostIndex := rand.Intn(len(curHostnames))
				if _, ok := pickedHosts[curHostnames[hostIndex]]; ok {
					j-- // Try again.
					continue
				}
				pickedHosts[curHostnames[hostIndex]] = struct{}{}
				mustWriteString(query, curHostnames[hostIndex])
				if j < numHosts-1 {
					mustWriteString(query, "|")
				}
			}
			mustWriteString(query, ")\"")

			// Write the common labels.
			for j := 0; j < len(labels); j++ {
				mustWriteString(query, ",")

				l := labels[j]
				mustWriteString(query, l.name)
				mustWriteString(query, "=\"")
				mustWriteString(query, l.value)
				mustWriteString(query, "\"")
			}

			if q.Aggregation != "" {
				mustWriteString(query, "})")
			}

			q.fanoutQuery(query, false, q.LoadRange, q.LoadStep)
		}()
	}
}

func (q *queryExecutor) fanoutQuery(
	query *strings.Builder,
	retResult bool,
	queryRange time.Duration,
	queryStep time.Duration,
) ([][]byte, error) {
	now := time.Now()
	values := make(url.Values)
	values.Set("query", query.String())
	values.Set("start", strconv.Itoa(int(now.Add(-1*queryRange).Unix())))
	values.Set("end", strconv.Itoa(int(now.Unix())))
	values.Set("step", queryStep.String())

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		multiErr xerrors.MultiError
		qs       = values.Encode()
	)

	results := make([][]byte, 0, len(q.URLs))
	for _, url := range q.URLs {
		wg.Add(1)
		reqURL := fmt.Sprintf("%s?%s", url, qs)

		if q.Debug {
			q.Logger.Info("fanout query",
				zap.String("url", reqURL),
				zap.Any("values", values))
		}

		go func() {
			res, err := q.executeQuery(reqURL, retResult)
			mu.Lock()
			multiErr = multiErr.Add(err)
			results = append(results, res)
			mu.Unlock()
		}()
	}

	wg.Wait()

	if err := multiErr.FinalError(); err != nil {
		q.Logger.Error("fanout error", zap.Error(err))
		return results, err
	}

	// NB: If less than 2 results returned, no need to compare for equality.
	if !retResult || len(results) < 2 {
		return results, nil
	}

	firstResult := results[0]
	for i, res := range results[1:] {
		if bytes.Equal(res, firstResult) {
			continue
		}

		q.Logger.Error("mismatch in returned data", zap.Int("index", i))
		return nil, errors.New("mismatch in returned data")
	}

	return results, nil
}

func (q *queryExecutor) executeQuery(
	reqURL string,
	retResult bool,
) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create request error: %v", err)
	}

	if len(q.Headers) != 0 {
		for k, v := range q.Headers {
			req.Header.Set(k, v)
		}
	}

	resp, err := q.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}

	defer func() {
		io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode/100 != 2 {
		q.Logger.Warn("response from query non-2XX status code",
			zap.String("url", reqURL),
			zap.Int("code", resp.StatusCode),
		)
	}

	if q.Debug || retResult {
		reader := io.Reader(resp.Body)
		if q.Debug && q.DebugLength > 0 {
			reader = io.LimitReader(resp.Body, int64(q.DebugLength))
		}

		data, err := ioutil.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %v", err)
		}

		if retResult {
			return data, nil
		}

		q.Logger.Info("response body",
			zap.Int("limit", q.DebugLength),
			zap.ByteString("body", data))
	}

	return nil, nil
}

// PromQueryResult is a prom query result.
type PromQueryResult struct {
	Status string        `json:"status"`
	Data   PromQueryData `json:"data"`
}

// PromQueryData is a prom query data.
type PromQueryData struct {
	ResultType promql.ValueType  `json:"resultType"`
	Result     []PromQueryMatrix `json:"result"`
	Stats      *stats.QueryStats `json:"stats,omitempty"`
}

// PromQueryMatrix is a prom query matrix.
type PromQueryMatrix struct {
	Values []model.SamplePair `json:"values"`
}

func (q *queryExecutor) validateQuery(dps Datapoints, data []byte) bool {
	res := PromQueryResult{}
	err := json.Unmarshal(data, &res)
	if err != nil {
		q.Logger.Error("unable to unmarshal PromQL query result",
			zap.Error(err))
		return false
	}

	matrix := res.Data.Result
	if len(matrix) != 1 {
		q.Logger.Error("expecting one result series, but got "+strconv.Itoa(len(matrix)),
			zap.Any("results", matrix))
		return false
	}

	i, matches := 0, 0

	if len(matrix[0].Values) == 0 {
		q.Logger.Warn("No results returned from query. There may be a slight delay in ingestion")
		return false
	}

	for _, value := range matrix[0].Values {
		for i < len(dps) {
			if float64(value.Value) == dps[i].Value {
				i++
				matches++
				break
			}

			i++
		}

		i = 0
	}

	if matches == 0 {
		q.Logger.Error("no values matched at all.")
		return false
	}

	return true
}

func mustWriteString(w *strings.Builder, v string) {
	_, err := w.WriteString(v)
	if err != nil {
		panic(err)
	}
}
