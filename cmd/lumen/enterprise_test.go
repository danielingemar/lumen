package main

import (
	"testing"

	"github.com/inmagma/lumen/internal/alerts"
)

// Every build carries the Enterprise notifiers; only a licence in force lets them send. A build that left them out
// would need a different binary for each customer.
func TestEnterpriseNotifiersAreInEveryBuild(t *testing.T) {
	for _, typ := range []string{"pagerduty", "opsgenie", "jira", "servicenow"} {
		n, ok := alerts.Default.Get(typ)
		if !ok {
			t.Fatalf("%s is not registered in the default build", typ)
		}
		if alerts.EditionOf(n) != "enterprise" {
			t.Fatalf("%s must need an Enterprise licence, it says %q", typ, alerts.EditionOf(n))
		}
	}
	if n, ok := alerts.Default.Get("webhook"); !ok || alerts.EditionOf(n) != "community" {
		t.Fatal("the webhook must stay a Community channel")
	}
}
