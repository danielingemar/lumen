package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/danielingemar/lumen/internal/model"
)

// buildFacetsQuery lists, for one signal, the services, hosts and (for traces) root operations that have data in a
// window. One query returns rows {k, v, n} where k is service, host or op.
func buildFacetsQuery(tenant, source, service string, from, to time.Time) (string, map[string]string, error) {
	f, t := window(from, to)
	p := map[string]string{"tenant": tenant, "from": f, "to": t}
	var table, col string
	switch source {
	case "traces":
		table, col = "otel_spans", "start_time"
	case "logs":
		table, col = "otel_logs", "ts"
	default:
		return "", nil, fmt.Errorf("source must be traces or logs")
	}
	base := fmt.Sprintf("FROM %s WHERE tenant = {tenant:String} AND %s >= {from:DateTime64(9)} AND %s <= {to:DateTime64(9)}", table, col, col)
	q := "SELECT 'service' AS k, service AS v, count() AS n " + base + " GROUP BY service\n" +
		"UNION ALL SELECT 'host' AS k, resource_attrs['host.name'] AS v, count() AS n " + base + " AND resource_attrs['host.name'] != '' GROUP BY v"
	if source == "traces" {
		op := base + " AND parent_span_id = ''"
		if service != "" {
			op += " AND service = {service:String}"
			p["service"] = service
		}
		q += "\nUNION ALL SELECT 'op' AS k, name AS v, count() AS n " + op + " GROUP BY name"
	}
	return "SELECT k, v, n FROM (" + q + ") WHERE v != '' ORDER BY k, n DESC, v LIMIT 3000 FORMAT JSONEachRow", p, nil
}

// Facets returns what a tenant has received: for dropdowns on the Traces and Logs pages.
func (c *ClickHouse) Facets(ctx context.Context, tenant, source, service string, from, to time.Time) (model.Facets, error) {
	q, p, err := buildFacetsQuery(tenant, source, service, from, to)
	if err != nil {
		return model.Facets{}, err
	}
	rows, err := c.run(ctx, q, p)
	out := model.Facets{Services: []model.Facet{}, Hosts: []model.Facet{}, Operations: []model.Facet{}}
	if err != nil {
		return out, err
	}
	for _, r := range rows {
		var x struct {
			K, V string
			N    json.Number
		}
		if json.Unmarshal(r, &x) != nil {
			continue
		}
		n, _ := x.N.Int64()
		f := model.Facet{Name: x.V, Count: n}
		switch x.K {
		case "service":
			out.Services = append(out.Services, f)
		case "host":
			out.Hosts = append(out.Hosts, f)
		case "op":
			out.Operations = append(out.Operations, f)
		}
	}
	return out, nil
}
