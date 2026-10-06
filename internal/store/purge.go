package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// purgeTables are every table that can hold a tenant's rows: the live tables and the archive tables.
func purgeTables() []string {
	var out []string
	for _, t := range tables {
		out = append(out, t.Name, t.Name+"_archive")
	}
	return out
}

func buildPurgeQuery(table string) string {
	return "ALTER TABLE " + table + " DELETE WHERE tenant = {tenant:String}"
}

func buildTenantRowsQuery(tenant string) (string, map[string]string) {
	var parts []string
	for _, t := range purgeTables() {
		parts = append(parts, "SELECT count() AS n FROM "+t+" WHERE tenant = {tenant:String}")
	}
	return "SELECT sum(n) AS n FROM (" + strings.Join(parts, " UNION ALL ") + ") FORMAT JSONEachRow", map[string]string{"tenant": tenant}
}

// PurgeTenant deletes every row of one tenant from every telemetry table, live and archived, and waits until it is done.
// It is what removes a tenant for good; there is no way back except a backup.
func (c *ClickHouse) PurgeTenant(ctx context.Context, tenant string) error {
	if strings.TrimSpace(tenant) == "" {
		return fmt.Errorf("refusing to purge a tenant with no name")
	}
	for _, t := range purgeTables() {
		resp, err := c.send(ctx, c.long, buildPurgeQuery(t), map[string]string{"tenant": tenant}, map[string]string{"mutations_sync": "1"}, nil)
		if err != nil {
			return err
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("clickhouse: purging %s: %s: %s", t, resp.Status, strings.TrimSpace(string(b)))
		}
	}
	return nil
}

// TenantRows counts the rows a tenant still has in all telemetry tables, live and archived.
func (c *ClickHouse) TenantRows(ctx context.Context, tenant string) (int64, error) {
	q, p := buildTenantRowsQuery(tenant)
	rows, err := c.run(ctx, q, p)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, r := range rows {
		var x struct {
			N json.Number `json:"n"`
		}
		if json.Unmarshal(r, &x) == nil {
			v, _ := x.N.Int64()
			n += v
		}
	}
	return n, nil
}
