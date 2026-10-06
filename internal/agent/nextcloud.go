package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var ncClient = &http.Client{Timeout: 20 * time.Second}

// num extracts a number at a nested JSON path. Nextcloud sometimes returns numbers as strings.
func num(m map[string]any, path ...string) (float64, bool) {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		if cur, ok = mm[k]; !ok {
			return 0, false
		}
	}
	switch v := cur.(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func ncGet(ctx context.Context, u string, set func(*http.Request)) ([]byte, int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if set != nil {
		set(req)
	}
	start := time.Now()
	resp, err := ncClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.StatusCode, time.Since(start), err
}

type ncMetric struct {
	path  []string
	name  string
	attrs map[string]string
}

var ncMetrics = []ncMetric{
	{[]string{"ocs", "data", "activeUsers", "last5minutes"}, "nextcloud_active_users", map[string]string{"period": "5m"}},
	{[]string{"ocs", "data", "activeUsers", "last1hour"}, "nextcloud_active_users", map[string]string{"period": "1h"}},
	{[]string{"ocs", "data", "activeUsers", "last24hours"}, "nextcloud_active_users", map[string]string{"period": "24h"}},
	{[]string{"ocs", "data", "nextcloud", "storage", "num_users"}, "nextcloud_users", nil},
	{[]string{"ocs", "data", "nextcloud", "storage", "num_files"}, "nextcloud_files", nil},
	{[]string{"ocs", "data", "nextcloud", "storage", "num_storages"}, "nextcloud_storages", map[string]string{"type": "total"}},
	{[]string{"ocs", "data", "nextcloud", "storage", "num_storages_local"}, "nextcloud_storages", map[string]string{"type": "local"}},
	{[]string{"ocs", "data", "nextcloud", "storage", "num_storages_home"}, "nextcloud_storages", map[string]string{"type": "home"}},
	{[]string{"ocs", "data", "nextcloud", "storage", "num_storages_other"}, "nextcloud_storages", map[string]string{"type": "other"}},
	{[]string{"ocs", "data", "nextcloud", "shares", "num_shares"}, "nextcloud_shares", map[string]string{"type": "total"}},
	{[]string{"ocs", "data", "nextcloud", "shares", "num_shares_user"}, "nextcloud_shares", map[string]string{"type": "user"}},
	{[]string{"ocs", "data", "nextcloud", "shares", "num_shares_groups"}, "nextcloud_shares", map[string]string{"type": "group"}},
	{[]string{"ocs", "data", "nextcloud", "shares", "num_shares_link"}, "nextcloud_shares", map[string]string{"type": "link"}},
	{[]string{"ocs", "data", "nextcloud", "shares", "num_shares_link_no_password"}, "nextcloud_shares", map[string]string{"type": "link_no_password"}},
	{[]string{"ocs", "data", "nextcloud", "shares", "num_fed_shares_sent"}, "nextcloud_shares", map[string]string{"type": "federated_sent"}},
	{[]string{"ocs", "data", "nextcloud", "shares", "num_fed_shares_received"}, "nextcloud_shares", map[string]string{"type": "federated_received"}},
	{[]string{"ocs", "data", "nextcloud", "system", "apps", "num_installed"}, "nextcloud_apps_installed", nil},
	{[]string{"ocs", "data", "nextcloud", "system", "apps", "num_updates_available"}, "nextcloud_apps_updates_available", nil},
	{[]string{"ocs", "data", "nextcloud", "system", "freespace"}, "nextcloud_data_free_bytes", nil},
	{[]string{"ocs", "data", "server", "database", "size"}, "nextcloud_database_size_bytes", nil},
	{[]string{"ocs", "data", "server", "php", "memory_limit"}, "nextcloud_php_memory_limit_bytes", nil},
	{[]string{"ocs", "data", "server", "php", "max_execution_time"}, "nextcloud_php_max_execution_time_seconds", nil},
	{[]string{"ocs", "data", "server", "php", "opcache", "opcache_statistics", "opcache_hit_rate"}, "nextcloud_php_opcache_hit_rate", nil},
	{[]string{"ocs", "data", "server", "php", "opcache", "memory_usage", "used_memory"}, "nextcloud_php_opcache_used_bytes", nil},
	{[]string{"ocs", "data", "server", "php", "opcache", "memory_usage", "current_wasted_percentage"}, "nextcloud_php_opcache_wasted_percent", nil},
}

// CollectNextcloud polls one instance. It always returns whatever it could gather (at minimum
// nextcloud_up), plus an error describing what failed, so a broken serverinfo login does not
// hide the availability signal.
func CollectNextcloud(ctx context.Context, t NextcloudTarget, now int64) ([]Point, error) {
	pts, err := collectCore(ctx, t, now)
	up := false
	for _, p := range pts {
		if p.Name == "nextcloud_up" && p.Value >= 1 {
			up = true
		}
	}
	extra, xerr := collectDeep(ctx, t, now, up)
	if err == nil {
		err = xerr
	}
	return append(pts, extra...), err
}

func collectCore(ctx context.Context, t NextcloudTarget, now int64) ([]Point, error) {
	base := strings.TrimRight(t.URL, "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid nextcloud url %q", t.URL)
	}
	svc := t.Service
	if svc == "" {
		svc = "nextcloud"
	}
	inst := map[string]string{"instance": u.Host}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{"instance": u.Host}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	pt := func(name string, v float64, a map[string]string) Point {
		return Point{Service: svc, Name: name, Type: "gauge", Value: v, Attrs: a, TimeNs: now}
	}

	body, code, took, err := ncGet(ctx, base+"/status.php", nil)
	if err != nil || code < 200 || code >= 300 {
		if err == nil {
			err = fmt.Errorf("%s/status.php returned HTTP %d: the URL must be the base address of Nextcloud (https://cloud.example.com), without /login or /index.php", base, code)
		}
		return []Point{pt("nextcloud_up", 0, inst)}, err
	}
	pts := []Point{pt("nextcloud_up", 1, inst), pt("nextcloud_status_response_seconds", took.Seconds(), inst)}
	var st map[string]any
	if json.Unmarshal(body, &st) != nil {
		return pts, fmt.Errorf("status.php did not return JSON (is this a Nextcloud URL?)")
	}
	for name, key := range map[string]string{"nextcloud_maintenance_mode": "maintenance", "nextcloud_needs_db_upgrade": "needsDbUpgrade", "nextcloud_installed": "installed"} {
		if v, ok := num(st, key); ok {
			pts = append(pts, pt(name, v, inst))
		}
	}
	version := str(st, "versionstring")
	info := with(map[string]string{"version": version})

	if t.Token == "" && t.Username == "" {
		return append(pts, pt("nextcloud_info", 1, info)), nil
	}
	body, code, _, err = ncGet(ctx, base+"/ocs/v2.php/apps/serverinfo/api/v1/info?format=json", func(r *http.Request) {
		r.Header.Set("OCS-APIRequest", "true")
		if t.Token != "" {
			r.Header.Set("NC-Token", t.Token)
		} else {
			r.SetBasicAuth(t.Username, t.Password)
		}
	})
	if err != nil {
		return append(pts, pt("nextcloud_info", 1, info)), err
	}
	if code != 200 {
		hint := ""
		if code == 401 || code == 403 {
			hint = " (check the serverinfo token, or the admin user/app password)"
		}
		return append(pts, pt("nextcloud_info", 1, info)), fmt.Errorf("serverinfo returned HTTP %d%s", code, hint)
	}
	var si map[string]any
	if json.Unmarshal(body, &si) != nil {
		return append(pts, pt("nextcloud_info", 1, info)), fmt.Errorf("serverinfo did not return JSON")
	}
	if v := str(si, "ocs", "data", "nextcloud", "system", "version"); v != "" {
		info["version"] = v
	}
	if v := str(si, "ocs", "data", "server", "php", "version"); v != "" {
		info["php_version"] = v
	}
	if v := str(si, "ocs", "data", "server", "database", "type"); v != "" {
		info["db_type"] = v
	}
	if v := str(si, "ocs", "data", "server", "database", "version"); v != "" {
		info["db_version"] = v
	}
	if avail, ok := boolAt(si, "ocs", "data", "nextcloud", "system", "update", "available"); ok {
		v := 0.0
		if avail {
			v = 1
			if nv := str(si, "ocs", "data", "nextcloud", "system", "update", "available_version"); nv != "" {
				info["update_version"] = nv
			}
		}
		pts = append(pts, pt("nextcloud_update_available", v, with(nil)))
		if at, ok := num(si, "ocs", "data", "nextcloud", "system", "update", "lastupdatedat"); ok && at > 0 {
			pts = append(pts, pt("nextcloud_update_checked_timestamp_seconds", at, with(nil)))
		}
	}
	pts = append(pts, pt("nextcloud_info", 1, info))
	for _, m := range ncMetrics {
		v, ok := num(si, m.path...)
		if !ok || (v < 0 && strings.HasSuffix(m.name, "_bytes")) { // -1 means unlimited
			continue
		}
		pts = append(pts, pt(m.name, v, with(m.attrs)))
	}
	return pts, nil
}
