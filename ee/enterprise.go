//go:build enterprise

// Package ee holds Lumen Enterprise features: ticket and on-call notifiers today (notifiers.go); SSO, audit export and
// long-term retention tiers are planned (see docs/EDITIONS.md).
// This directory is under a separate commercial license, see ee/LICENSE.
package ee

import "github.com/danielingemar/lumen/internal/edition"

func init() {
	edition.Name = "enterprise"
	// TODO: edition.NewAuthenticator = newOIDCAuthenticator
	// TODO: edition.Authz = newRBAC()
	// TODO: verify the license key at startup
}
