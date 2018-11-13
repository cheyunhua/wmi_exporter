// Below code originally copied from prometheus/node_exporter/collector/textfile.go:
//
// Copyright 2015 The Prometheus Authors
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

package collector

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/log"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	textFileDirectory = kingpin.Flag(
		"collector.textfile.directory",
		"Directory to read text files with metrics from.",
	).Default("C:\\Program Files\\wmi_exporter\\textfile_inputs").String()

	mtimeDesc = prometheus.NewDesc(
		"wmi_textfile_mtime_seconds",
		"Unixtime mtime of textfiles successfully read.",
		[]string{"file"},
		nil,
	)
)

type textFileCollector struct {
	path string
	// Only set for testing to get predictable output.
	mtime *float64
}

func init() {
	Factories["textfile"] = NewTextFileCollector
}

// NewTextFileCollector returns a new Collector exposing metrics read from files
// in the given textfile directory.
func NewTextFileCollector() (Collector, error) {
	return &textFileCollector{
		path: *textFileDirectory,
	}, nil
}

func convertMetricFamily(metricFamily *dto.MetricFamily, ch chan<- prometheus.Metric, seen map[uint64]string, path string) {
	var valType prometheus.ValueType
	var val float64

	allLabelNames := map[string]struct{}{}
	for _, metric := range metricFamily.Metric {
		labels := metric.GetLabel()
		for _, label := range labels {
			if _, ok := allLabelNames[label.GetName()]; !ok {
				allLabelNames[label.GetName()] = struct{}{}
			}
		}
	}

	for _, metric := range metricFamily.Metric {
		if metric.TimestampMs != nil {
			log.Warnf("Ignoring unsupported custom timestamp on textfile collector metric %v", metric)
		}

		labels := metric.GetLabel()
		var names []string
		var values []string
		for _, label := range labels {
			names = append(names, label.GetName())
			values = append(values, label.GetValue())
		}

		for k := range allLabelNames {
			present := false
			for _, name := range names {
				if k == name {
					present = true
					break
				}
			}
			if present == false {
				names = append(names, k)
				values = append(values, "")
			}
		}

		h := hash(metricFamily, metric)
		if seenIn, ok := seen[h]; ok {
			repr := friendlyString(*metricFamily.Name, names, values)
			log.Warnf("Metric %s was read from %s, but has already been collected from file %s, skipping", repr, path, seenIn)
			continue
		}
		seen[h] = path

		metricType := metricFamily.GetType()
		switch metricType {
		case dto.MetricType_COUNTER:
			valType = prometheus.CounterValue
			val = metric.Counter.GetValue()

		case dto.MetricType_GAUGE:
			valType = prometheus.GaugeValue
			val = metric.Gauge.GetValue()

		case dto.MetricType_UNTYPED:
			valType = prometheus.UntypedValue
			val = metric.Untyped.GetValue()

		case dto.MetricType_SUMMARY:
			quantiles := map[float64]float64{}
			for _, q := range metric.Summary.Quantile {
				quantiles[q.GetQuantile()] = q.GetValue()
			}
			ch <- prometheus.MustNewConstSummary(
				prometheus.NewDesc(
					*metricFamily.Name,
					metricFamily.GetHelp(),
					names, nil,
				),
				metric.Summary.GetSampleCount(),
				metric.Summary.GetSampleSum(),
				quantiles, values...,
			)
		case dto.MetricType_HISTOGRAM:
			buckets := map[float64]uint64{}
			for _, b := range metric.Histogram.Bucket {
				buckets[b.GetUpperBound()] = b.GetCumulativeCount()
			}
			ch <- prometheus.MustNewConstHistogram(
				prometheus.NewDesc(
					*metricFamily.Name,
					metricFamily.GetHelp(),
					names, nil,
				),
				metric.Histogram.GetSampleCount(),
				metric.Histogram.GetSampleSum(),
				buckets, values...,
			)
		default:
			log.Errorf("unknown metric type for file")
			continue
		}
		if metricType == dto.MetricType_GAUGE || metricType == dto.MetricType_COUNTER || metricType == dto.MetricType_UNTYPED {
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					*metricFamily.Name,
					metricFamily.GetHelp(),
					names, nil,
				),
				valType, val, values...,
			)
		}
	}
}

func (c *textFileCollector) exportMTimes(mtimes map[string]time.Time, ch chan<- prometheus.Metric) {
	// Export the mtimes of the successful files.
	if len(mtimes) > 0 {
		// Sorting is needed for predictable output comparison in tests.
		filenames := make([]string, 0, len(mtimes))
		for filename := range mtimes {
			filenames = append(filenames, filename)
		}
		sort.Strings(filenames)

		for _, filename := range filenames {
			mtime := float64(mtimes[filename].UnixNano() / 1e9)
			if c.mtime != nil {
				mtime = *c.mtime
			}
			ch <- prometheus.MustNewConstMetric(mtimeDesc, prometheus.GaugeValue, mtime, filename)
		}
	}
}

// Update implements the Collector interface.
func (c *textFileCollector) Collect(ch chan<- prometheus.Metric) error {
	error := 0.0
	mtimes := map[string]time.Time{}
	seenMetrics := make(map[uint64]string)

	// Iterate over files and accumulate their metrics.
	files, err := ioutil.ReadDir(c.path)
	if err != nil && c.path != "" {
		log.Errorf("Error reading textfile collector directory %q: %s", c.path, err)
		error = 1.0
	}

fileLoop:
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".prom") {
			continue
		}
		path := filepath.Join(c.path, f.Name())
		log.Debugf("Processing file %q", path)
		file, err := os.Open(path)
		if err != nil {
			log.Errorf("Error opening %q: %v", path, err)
			error = 1.0
			continue
		}
		var parser expfmt.TextParser
		parsedFamilies, err := parser.TextToMetricFamilies(carriageReturnFilteringReader{r: file})
		file.Close()
		if err != nil {
			log.Errorf("Error parsing %q: %v", path, err)
			error = 1.0
			continue
		}
		for _, mf := range parsedFamilies {
			for _, m := range mf.Metric {
				if m.TimestampMs != nil {
					log.Errorf("Textfile %q contains unsupported client-side timestamps, skipping entire file", path)
					error = 1.0
					continue fileLoop
				}
			}
			if mf.Help == nil {
				help := fmt.Sprintf("Metric read from %s", path)
				mf.Help = &help
			}
		}

		// Only set this once it has been parsed and validated, so that
		// a failure does not appear fresh.
		mtimes[f.Name()] = f.ModTime()

		for _, mf := range parsedFamilies {
			convertMetricFamily(mf, ch, seenMetrics, path)
		}
	}

	c.exportMTimes(mtimes, ch)

	// Export if there were errors.
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc(
			"wmi_textfile_scrape_error",
			"1 if there was an error opening or reading a file, 0 otherwise",
			nil, nil,
		),
		prometheus.GaugeValue, error,
	)
	return nil
}

//
// End of code copied from prometheus/node_exporter/collector/textfile.go
//

// Below code copied from prometheus/client_golang/prometheus/fnv.go:
//
// Copyright 2018 The Prometheus Authors
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

const (
	offset64           = 14695981039346656037
	prime64            = 1099511628211
	separatorByte byte = 255
)

// hashNew initializies a new fnv64a hash value.
func hashNew() uint64 {
	return offset64
}

// hashAdd adds a string to a fnv64a hash value, returning the updated hash.
func hashAdd(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// hashAddByte adds a byte to a fnv64a hash value, returning the updated hash.
func hashAddByte(h uint64, b byte) uint64 {
	h ^= uint64(b)
	h *= prime64
	return h
}

func hash(mf *dto.MetricFamily, m *dto.Metric) uint64 {
	h := hashNew()
	h = hashAdd(h, mf.GetName())
	h = hashAddByte(h, separatorByte)
	// Make sure label pairs are sorted. We depend on it for the consistency
	// check.
	sort.Sort(prometheus.LabelPairSorter(m.Label))
	for _, lp := range m.Label {
		h = hashAdd(h, lp.GetValue())
		h = hashAddByte(h, separatorByte)
	}

	return h
}

//
// End of code copied from prometheus/client_golang/prometheus/fnv.go
//

type carriageReturnFilteringReader struct {
	r io.Reader
}

// Read returns data from the underlying io.Reader, but with \r filtered out
func (cr carriageReturnFilteringReader) Read(p []byte) (int, error) {
	buf := make([]byte, len(p))
	n, err := cr.r.Read(buf)

	if err != nil && err != io.EOF {
		return n, err
	}

	pi := 0
	for i := 0; i < n; i++ {
		if buf[i] != '\r' {
			p[pi] = buf[i]
			pi++
		}
	}

	return pi, err
}

func friendlyString(name string, labelNames, labelValues []string) string {
	var bs bytes.Buffer

	sortedNames := make([]string, len(labelNames))
	copy(sortedNames, labelNames)
	sort.Strings(sortedNames)

	sortedValues := make([]string, len(labelValues))
	copy(sortedValues, labelValues)
	sort.Strings(sortedValues)

	bs.WriteString(name)
	bs.WriteRune('{')
	for idx := 0; idx < len(sortedNames); idx++ {
		bs.WriteString(fmt.Sprintf(`%s="%s"`, sortedNames[idx], sortedValues[idx]))
		if idx < len(sortedNames)-1 {
			bs.WriteRune(',')
		}
	}
	bs.WriteRune('}')
	return bs.String()
}
