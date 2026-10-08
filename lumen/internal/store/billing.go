package store

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/danielingemar/lumen/internal/billing"
)

// instanceUsageSQL is the highest value of each day of what every Nextcloud instance reports about its size: the users (from
// serverinfo), the enabled accounts and the bytes the accounts' files take (both from the account list). The day is a UTC day.
const instanceUsageSQL = "SELECT attrs['instance'] AS instance, toString(toDate(ts, 'UTC')) AS day, name, max(value) AS v FROM otel_metrics " +
	"WHERE tenant = {tenant:String} AND ts >= {from:DateTime64(9)} AND ts < {to:DateTime64(9)} AND attrs['instance'] != '' " +
	"AND (name IN ('nextcloud_users', 'nextcloud_storage_used_bytes') OR (name = 'nextcloud_accounts' AND attrs['state'] = 'enabled')) " +
	"GROUP BY instance, day, name ORDER BY instance, day, name LIMIT 1000000 FORMAT JSONEachRow"

func buildInstanceUsageQuery(tenant string, from, to time.Time) (string, map[string]string) {
	f, t := window(from, to)
	return instanceUsageSQL, map[string]string{"tenant": tenant, "from": f, "to": t}
}

// InstanceUsage is what billing is made from. Every method of this package filters on the tenant; so does this one.
func (c *ClickHouse) InstanceUsage(ctx context.Context, tenant string, from, to time.Time) ([]billing.Sample, error) {
	q, p := buildInstanceUsageQuery(tenant, from, to)
	rows, err := c.run(ctx, q, p)
	if err != nil {
		return nil, err
	}
	type key struct{ inst, day string }
	idx := map[key]int{}
	var out []billing.Sample
	for _, r := range rows {
		var x struct {
			Instance string      `json:"instance"`
			Day      string      `json:"day"`
			Name     string      `json:"name"`
			V        json.Number `json:"v"`
		}
		if json.Unmarshal(r, &x) != nil {
			continue
		}
		v, err := strconv.ParseFloat(x.V.String(), 64)
		if err != nil || v != v || v < 0 { // NaN and negative numbers are not usage
			continue
		}
		k := key{x.Instance, x.Day}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, billing.Sample{Instance: x.Instance, Day: x.Day})
		}
		switch x.Name {
		case "nextcloud_users":
			out[i].Users = &v
		case "nextcloud_accounts":
			out[i].Accounts = &v
		case "nextcloud_storage_used_bytes":
			out[i].Bytes = &v
		}
	}
	return out, nil
}

var _ billing.Source = (*ClickHouse)(nil)
