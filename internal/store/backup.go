package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func dayWindow(day time.Time) (string, string) {
	d := day.UTC().Truncate(24 * time.Hour)
	f := "2006-01-02 15:04:05"
	return d.Format(f), d.Add(24 * time.Hour).Format(f)
}

func timeCol(table string) (string, error) {
	for _, t := range tables {
		if t.Name == table {
			return t.TimeCol, nil
		}
	}
	return "", fmt.Errorf("unknown table %q", table)
}

func buildTenantsQuery(table string, day time.Time) (string, map[string]string, error) {
	col, err := timeCol(table)
	if err != nil {
		return "", nil, err
	}
	f, t := dayWindow(day)
	return fmt.Sprintf("SELECT DISTINCT tenant FROM %s WHERE %s >= {from:DateTime64(9)} AND %s < {to:DateTime64(9)} ORDER BY tenant FORMAT JSONEachRow", table, col, col),
		map[string]string{"from": f, "to": t}, nil
}

func buildExportQuery(table, tenant string, day time.Time) (string, map[string]string, error) {
	col, err := timeCol(table)
	if err != nil {
		return "", nil, err
	}
	f, t := dayWindow(day)
	return fmt.Sprintf("SELECT * FROM %s WHERE tenant = {tenant:String} AND %s >= {from:DateTime64(9)} AND %s < {to:DateTime64(9)} ORDER BY %s FORMAT JSONEachRow", table, col, col, col),
		map[string]string{"tenant": tenant, "from": f, "to": t}, nil
}

func buildUnloadQuery(table, tenant string, day time.Time) (string, map[string]string, error) {
	col, err := timeCol(table)
	if err != nil {
		return "", nil, err
	}
	f, t := dayWindow(day)
	return fmt.Sprintf("ALTER TABLE %s_archive DELETE WHERE tenant = {tenant:String} AND %s >= {from:DateTime64(9)} AND %s < {to:DateTime64(9)}", table, col, col),
		map[string]string{"tenant": tenant, "from": f, "to": t}, nil
}

func buildLoadedQuery(tenant string) (string, map[string]string) {
	var parts []string
	for _, t := range tables {
		parts = append(parts, fmt.Sprintf("SELECT toString(toDate(%s)) AS d FROM %s_archive WHERE tenant = {tenant:String}", t.TimeCol, t.Name))
	}
	return "SELECT DISTINCT d FROM (" + strings.Join(parts, " UNION ALL ") + ") ORDER BY d LIMIT 1000 FORMAT JSONEachRow", map[string]string{"tenant": tenant}
}

// Tenants lists the tenants that have rows in a table on a given (UTC) day.
func (c *ClickHouse) Tenants(ctx context.Context, table string, day time.Time) ([]string, error) {
	q, p, err := buildTenantsQuery(table, day)
	if err != nil {
		return nil, err
	}
	rows, err := c.run(ctx, q, p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		var x struct{ Tenant string }
		if json.Unmarshal(r, &x) == nil && x.Tenant != "" {
			out = append(out, x.Tenant)
		}
	}
	return out, nil
}

// Export streams one tenant's rows of one day, as JSON lines, into w.
func (c *ClickHouse) Export(ctx context.Context, table, tenant string, day time.Time, w io.Writer) error {
	q, p, err := buildExportQuery(table, tenant, day)
	if err != nil {
		return err
	}
	resp, err := c.send(ctx, c.long, q, p, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// Import loads JSON lines into the archive table of the given table (never into the live table).
func (c *ClickHouse) Import(ctx context.Context, table string, r io.Reader) error {
	if _, err := timeCol(table); err != nil {
		return err
	}
	resp, err := c.send(ctx, c.long, "INSERT INTO "+table+"_archive FORMAT JSONEachRow", nil, nil, r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Unload removes one tenant's rows of one day from the archive table (waits until done).
func (c *ClickHouse) Unload(ctx context.Context, table, tenant string, day time.Time) error {
	q, p, err := buildUnloadQuery(table, tenant, day)
	if err != nil {
		return err
	}
	resp, err := c.send(ctx, c.long, q, p, map[string]string{"mutations_sync": "1"}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// LoadedDays lists the days (YYYY-MM-DD) of a tenant that currently sit in the archive tables.
func (c *ClickHouse) LoadedDays(ctx context.Context, tenant string) ([]string, error) {
	q, p := buildLoadedQuery(tenant)
	rows, err := c.run(ctx, q, p)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		var x struct{ D string }
		if json.Unmarshal(r, &x) == nil {
			out = append(out, x.D)
		}
	}
	return out, nil
}
