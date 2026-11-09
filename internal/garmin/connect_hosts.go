package garmin

import (
	"fmt"
	"os"
	"strings"
)

// The unofficial Garmin Connect API is what Garmin's own apps call. It needs an
// account sign-in instead of a Personal API key, so it lives beside the Personal
// API client rather than inside it: a ConnectClient never reads the Personal
// API's key, base URL, or token cache, and the Personal API client never sees a
// Connect token.

// ConnectDomainEnvVar selects the Garmin region. Only the domains in
// connectDomains are accepted, because the sign-in sends the account password
// to sso.<domain>.
const ConnectDomainEnvVar = "GARMIN_CONNECT_DOMAIN"

var connectDomains = []string{"garmin.com", "garmin.cn"}

// ConnectHosts are the base URLs of one Garmin region, without a trailing
// slash.
type ConnectHosts struct {
	SSO               string // sign-in
	API               string // connectapi, the data API
	DIAuth            string // OAuth2 token endpoint
	Connect           string // web front end, the portal sign-in's service URL
	MobileIntegration string // the iOS sign-in's service URL
}

func connectHostsFor(domain string) ConnectHosts {
	return ConnectHosts{
		SSO:               "https://sso." + domain,
		API:               "https://connectapi." + domain,
		DIAuth:            "https://diauth." + domain,
		Connect:           "https://connect." + domain,
		MobileIntegration: "https://mobile.integration." + domain,
	}
}

// connectHostsFromEnv fails rather than falling back to garmin.com, so a typo
// in the region never sends a China account's password to the global region.
func connectHostsFromEnv() (ConnectHosts, error) {
	value := strings.TrimSpace(os.Getenv(ConnectDomainEnvVar))
	if value == "" {
		return connectHostsFor(connectDomains[0]), nil
	}
	for _, domain := range connectDomains {
		if value == domain {
			return connectHostsFor(domain), nil
		}
	}
	return ConnectHosts{}, fmt.Errorf("%s must be one of %s, got %q", ConnectDomainEnvVar, strings.Join(connectDomains, ", "), value)
}

func (h ConnectHosts) iosServiceURL() string    { return h.MobileIntegration + "/gcm/ios" }
func (h ConnectHosts) portalServiceURL() string { return h.Connect + "/app" }
func (h ConnectHosts) widgetEmbedURL() string   { return h.SSO + "/sso/embed" }
func (h ConnectHosts) diTokenURL() string       { return h.DIAuth + "/di-oauth2-service/oauth/token" }
