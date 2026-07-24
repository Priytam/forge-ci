package api

import (
	"net/http"

	"github.com/priytamjeepandey/forge-ci/internal/oidc"
)

// OIDC discovery + JWKS for keyless cloud auth. These two endpoints let AWS/GCP
// validate the per-job ID tokens Forge mints (see internal/oidc): the cloud
// fetches the discovery document at the issuer's well-known path, reads jwks_uri
// from it, then fetches the public keys. They serve at the ROOT well-known paths
// (not under /api/v1) because that is where an OIDC relying party looks by
// convention, and are PUBLIC — auth-exempt (see authExempt) so a cloud provider
// with no Forge session can reach them even when SSO is enforced.
func (s *Server) registerOIDCRoutes() {
	m := s.mux
	m.HandleFunc("GET /.well-known/openid-configuration", s.oidcDiscovery)
	m.HandleFunc("GET /.well-known/jwks.json", s.oidcJWKS)
}

func (s *Server) oidcDiscovery(w http.ResponseWriter, _ *http.Request) {
	// Issuer is EXTERNAL_URL (the server's public origin), identical to the `iss`
	// claim stamped into minted tokens.
	writeJSON(w, http.StatusOK, oidc.Discovery(externalURL()))
}

func (s *Server) oidcJWKS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.store.OIDCSigner().JWKS())
}
