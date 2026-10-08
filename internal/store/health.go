package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/danielingemar/lumen/internal/health"
)

// Health asks ClickHouse how much room its disks have and what takes the space. The system tables (query log, trace log and the
// like) are included on purpose: they grow without anyone looking, and they are the usual surprise.
// the two statements are named so that the test of the SQL on a real engine can run exactly them
const (
	disksSQL  = "SELECT name, free_space AS free, total_space AS total FROM system.disks FORMAT JSONEachRow"
	tablesSQL = "SELECT concat(database, '.', table) AS name, sum(bytes_on_disk) AS bytes FROM system.parts WHERE active GROUP BY database, table ORDER BY bytes DESC LIMIT 12 FORMAT JSONEachRow"
)

func hdQueries() ([]string, error) { return []string{disksSQL, tablesSQL}, nil }

func (c *ClickHouse) Health(ctx context.Context) (health.CH, error) {
	var h health.CH
	rows, err := c.run(ctx, disksSQL, nil)
	if err != nil {
		return h, err
	}
	for _, r := range rows {
		var x struct {
			Name  string      `json:"name"`
			Free  json.Number `json:"free"`
			Total json.Number `json:"total"`
		}
		if json.Unmarshal(r, &x) != nil {
			continue
		}
		d := health.Disk{Name: x.Name, Path: "ClickHouse disk " + x.Name}
		fmt.Sscan(x.Free.String(), &d.Free)
		fmt.Sscan(x.Total.String(), &d.Total)
		h.Disks = append(h.Disks, d)
	}
	rows, err = c.run(ctx, tablesSQL, nil)
	if err != nil {
		return h, nil // the disks are what matters; a failing size list is not worth failing for
	}
	for _, r := range rows {
		var x struct {
			Name  string      `json:"name"`
			Bytes json.Number `json:"bytes"`
		}
		if json.Unmarshal(r, &x) != nil {
			continue
		}
		t := health.Table{Name: x.Name}
		fmt.Sscan(x.Bytes.String(), &t.Bytes)
		h.Tables = append(h.Tables, t)
	}
	return h, nil
}
