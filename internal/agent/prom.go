package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseProm parses the Prometheus text exposition format into points.
// Counters (and *_total/_count/_sum/_bucket series) become cumulative sums, the rest gauges.
// NaN and Inf samples are skipped because they cannot be represented in JSON.
func ParseProm(r io.Reader, service string, now int64) []Point {
	types := map[string]string{}
	var out []Point
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if f := strings.Fields(line); len(f) >= 4 && f[1] == "TYPE" {
				types[f[2]] = f[3]
			}
			continue
		}
		name, labels, rest, ok := splitSample(line)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		typ := "gauge"
		if types[name] == "counter" || hasAnySuffix(name, "_total", "_count", "_sum", "_bucket") {
			typ = "sum"
		}
		out = append(out, Point{Service: service, Name: name, Type: typ, Value: v, Attrs: labels, TimeNs: now})
	}
	return out
}

func hasAnySuffix(s string, suf ...string) bool {
	for _, x := range suf {
		if strings.HasSuffix(s, x) {
			return true
		}
	}
	return false
}

// splitSample splits `name{a="b",c="d"} 1.5 [ts]` into its parts.
func splitSample(line string) (name string, labels map[string]string, rest string, ok bool) {
	i := strings.IndexAny(line, "{ \t")
	if i < 0 {
		return "", nil, "", false
	}
	name = line[:i]
	if line[i] != '{' {
		return name, nil, line[i:], true
	}
	labels = map[string]string{}
	j := i + 1
	for j < len(line) && line[j] != '}' {
		eq := strings.IndexByte(line[j:], '=')
		if eq < 0 || j+eq+1 >= len(line) || line[j+eq+1] != '"' {
			return "", nil, "", false
		}
		key := strings.TrimSpace(strings.TrimPrefix(line[j:j+eq], ","))
		k := j + eq + 2
		var val strings.Builder
		for k < len(line) && line[k] != '"' {
			if line[k] == '\\' && k+1 < len(line) {
				k++
				if line[k] == 'n' {
					val.WriteByte('\n')
				} else {
					val.WriteByte(line[k])
				}
			} else {
				val.WriteByte(line[k])
			}
			k++
		}
		if k >= len(line) {
			return "", nil, "", false
		}
		labels[key] = val.String()
		j = k + 1
		for j < len(line) && (line[j] == ',' || line[j] == ' ') {
			j++
		}
	}
	if j >= len(line) {
		return "", nil, "", false
	}
	return name, labels, line[j+1:], true
}

// Scrape fetches one Prometheus endpoint.
func Scrape(ctx context.Context, url, service string) ([]Point, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("scrape %s: %s", url, resp.Status)
	}
	return ParseProm(io.LimitReader(resp.Body, 32<<20), service, time.Now().UnixNano()), nil
}
