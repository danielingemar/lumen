package server

import (
	"net/http"

	"github.com/inmagma/lumen/internal/auth"
	"github.com/inmagma/lumen/internal/edition"
	"github.com/inmagma/lumen/internal/license"
)

// Features that need an Enterprise licence. They are the things an organisation buys because it is bigger: groups with
// their own permissions, single sign-on and billing. Everything else, including any number of users, is Community.
const (
	msgGroups  = "groups with their own permissions need an Enterprise licence (the built-in Administrator and User groups are always available)"
	msgSSO     = "single sign-on needs an Enterprise licence"
	msgBilling = "billing needs an Enterprise licence"
)

// licensed tells whether the Enterprise features may be used now. Without a licence manager there is no licence.
func (s *Server) licensed() bool { return s.lic != nil && s.lic.Allows(license.Enterprise) }

// featureFlags is what the interface needs to know to show a feature or a lock.
func (s *Server) featureFlags() map[string]bool {
	on := s.licensed()
	return map[string]bool{"groups": on, "sso": on, "billing": on}
}

// requireEnterprise answers 403 with the reason and returns false when the licence does not cover the feature.
func (s *Server) requireEnterprise(w http.ResponseWriter, msg string) bool {
	if s.licensed() {
		return true
	}
	writeErr(w, http.StatusForbidden, msg)
	return false
}

// builtinGroup tells whether a group is one of the always-available ones. Anything else is a custom group.
func builtinGroup(id string) bool { return id == "" || auth.IsBuiltinGroup(id) }

// billingOnly wraps a billing handler: the data stays, but nobody reads or changes it without the licence.
func (s *Server) billingOnly(h func(http.ResponseWriter, *http.Request, edition.Identity)) func(http.ResponseWriter, *http.Request, edition.Identity) {
	return func(w http.ResponseWriter, r *http.Request, id edition.Identity) {
		if !s.requireEnterprise(w, msgBilling) {
			return
		}
		h(w, r, id)
	}
}
