// Package ee holds Lumen Enterprise features: ticket and on-call notifiers today (notifiers.go); SSO, audit export and
// long-term retention tiers are planned (see docs/EDITIONS.md).
// This directory is under a separate commercial license, see ee/LICENSE.
//
// It is part of every build. What a licence unlocks is decided at run time: each Enterprise feature asks the licence
// manager (internal/license) whether it is allowed now, so adding a licence under Settings is all that is needed.
package ee
