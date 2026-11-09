package garmin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Sign-in to Garmin SSO tries three strategies in turn: the iOS app's
// sign-in, then the SSO embed widget, then the web portal. Each yields a CAS service ticket, which is
// exchanged for DI OAuth2 tokens. A rejected password or a completed MFA
// stops the chain; a block, rate limit, or unexpected page moves on to the
// next strategy.
//
// Cloudflare may still block every strategy, because Go's TLS stack can't
// present a browser's TLS fingerprint.

const (
	iosSSOClientID    = "GCM_IOS_DARK"
	portalSSOClientID = "GarminConnect"
	iosLoginUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_7 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148"
	desktopUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	diGrantType       = "https://connectapi.garmin.com/di-oauth2-service/oauth/grant/service_ticket"
	htmlAccept        = "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"
	jsonAccept        = "application/json, text/plain, */*"
	maxMFAAttempts    = 3
)

// diClientIDs are tried in this order for the ticket exchange.
var diClientIDs = []string{
	"GARMIN_CONNECT_MOBILE_ANDROID_DI_2025Q2",
	"GARMIN_CONNECT_MOBILE_ANDROID_DI_2024Q4",
	"GARMIN_CONNECT_MOBILE_ANDROID_DI",
	"GARMIN_CONNECT_MOBILE_IOS_DI",
}

// Pauses between loading a sign-in page and posting the password. Cloudflare
// flags a page load followed at once by a POST as a bot.
var (
	mobileDelay = [2]time.Duration{2 * time.Second, 5 * time.Second}
	widgetDelay = [2]time.Duration{3 * time.Second, 8 * time.Second}
	portalDelay = [2]time.Duration{10 * time.Second, 20 * time.Second}
)

var (
	csrfPattern    = regexp.MustCompile(`name="_csrf"\s+value="(.+?)"`)
	titlePattern   = regexp.MustCompile(`<title>(.+?)</title>`)
	ticketPattern  = regexp.MustCompile(`\?ticket=(ST-[^"&\s]+)`)
	mfaVarsPattern = regexp.MustCompile(`var\s+(customerGuid|mfaMethod|locale|clientId|codeSentTo)\s*=\s*"([^"]*)"\s*;`)
)

// errBadCredentials stops the chain: every strategy would get the same answer.
var errBadCredentials = &connectLoginError{"Garmin rejected the email or password."}

// LoginPrompts asks the person signing in for what Garmin wants next.
type LoginPrompts struct {
	// MFACode returns the one-time code Garmin sent; retry is true after a
	// wrong code.
	MFACode func(ctx context.Context, method string, retry bool) (string, error)
	// Progress reports each step, since the pauses make a sign-in take up to
	// half a minute. It may be nil.
	Progress func(message string)
}

// strategyError is a failure the next strategy might get past.
type strategyError struct {
	strategy    string
	reason      string
	rateLimited bool
}

func (e *strategyError) Error() string { return e.strategy + ": " + e.reason }

// pendingMFA is what completing an MFA challenge needs from the sign-in that
// raised it: the same cookies, parameters, and headers.
type pendingMFA struct {
	flow       string // "mobile", "portal", or "widget"
	session    *http.Client
	method     string
	params     url.Values
	headers    http.Header
	serviceURL string
	csrf       string
}

type loginResult struct {
	ticket     string
	serviceURL string
	mfa        *pendingMFA
}

// Login signs in with the account's email and password and saves the
// resulting tokens. The password is used for the sign-in requests only and
// never stored.
func (c *ConnectClient) Login(ctx context.Context, email, password string, prompts LoginPrompts) (string, error) {
	progress := prompts.Progress
	if progress == nil {
		progress = func(string) {}
	}
	strategies := []struct {
		name string
		run  func(context.Context, string, string, func(string)) (loginResult, error)
	}{
		{"iOS app sign-in", c.mobileLogin},
		{"SSO widget sign-in", c.widgetLogin},
		{"web portal sign-in", c.portalLogin},
	}
	var failures []string
	rateLimited := 0
	for _, strategy := range strategies {
		progress("Trying the " + strategy.name + "...")
		result, err := strategy.run(ctx, email, password, progress)
		if err == nil && result.mfa != nil {
			result, err = c.completeMFA(ctx, result.mfa, prompts.MFACode, progress)
			if err != nil {
				return "", err
			}
		}
		if err == nil {
			path, err := c.finishLogin(ctx, result, progress)
			if err == nil {
				return path, nil
			}
			var sErr *strategyError
			if !errors.As(err, &sErr) {
				return "", err
			}
			failures = append(failures, strategy.name+": "+sErr.reason)
			continue
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		var sErr *strategyError
		if !errors.As(err, &sErr) {
			return "", err
		}
		if sErr.rateLimited {
			rateLimited++
		}
		failures = append(failures, strategy.name+": "+sErr.reason)
		progress("  " + sErr.reason)
	}
	path, _ := ConnectTokenFile()
	fallback := fmt.Sprintf("If this keeps happening, sign in with python-garminconnect and copy its "+
		"~/.garminconnect/garmin_tokens.json to %s.", path)
	if rateLimited == len(strategies) {
		return "", &connectLoginError{"Garmin rate-limited or blocked every sign-in attempt (HTTP 429/427). " +
			"Garmin often does this to clients that aren't a browser, not only after too many tries. " +
			"Try again later or from another network. " + fallback}
	}
	return "", &connectLoginError{"Couldn't sign in to Garmin Connect (" + strings.Join(failures, "; ") + "). " + fallback}
}

func (c *ConnectClient) newSession() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Transport: c.httpClient.Transport, Timeout: c.httpClient.Timeout, Jar: jar}
}

func (c *ConnectClient) pause(ctx context.Context, bounds [2]time.Duration, progress func(string)) error {
	delay := c.jitter(bounds[0], bounds[1])
	progress(fmt.Sprintf("  Waiting %ds before sending the password, as a browser would...", int(delay.Round(time.Second)/time.Second)))
	return c.sleep(ctx, delay)
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func randomBetween(low, high time.Duration) time.Duration {
	if high <= low {
		return low
	}
	return low + rand.N(high-low)
}

type page struct {
	status int
	body   string
	url    string
}

func fetch(ctx context.Context, session *http.Client, method, target string, header http.Header, body io.Reader) (page, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return page{}, err
	}
	req.Header = header
	resp, err := session.Do(req)
	if err != nil {
		// The URL can carry the service ticket, so only the cause is kept.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return page{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return page{}, err
	}
	return page{status: resp.StatusCode, body: string(data), url: resp.Request.URL.String()}, nil
}

func blocked(status int) bool { return status == 427 || status == http.StatusTooManyRequests }

func browserHeader(userAgent, accept, referer string) http.Header {
	h := http.Header{}
	h.Set("User-Agent", userAgent)
	h.Set("Accept", accept)
	h.Set("Accept-Language", "en-US,en;q=0.9")
	if referer != "" {
		h.Set("Referer", referer)
	}
	return h
}

func withQuery(base string, q url.Values) string { return base + "?" + q.Encode() }

func (c *ConnectClient) mobileLogin(ctx context.Context, email, password string, progress func(string)) (loginResult, error) {
	const name = "iOS app sign-in"
	session := c.newSession()
	signIn := withQuery(c.hosts.SSO+"/mobile/sso/en_US/sign-in", url.Values{
		"clientId": {iosSSOClientID}, "service": {c.hosts.iosServiceURL()},
	})
	// Without the cookies and Referer this page sets, the POST gets a 429.
	p, err := fetch(ctx, session, http.MethodGet, signIn, browserHeader(iosLoginUserAgent, htmlAccept, ""), nil)
	if err != nil {
		return loginResult{}, &strategyError{strategy: name, reason: err.Error()}
	}
	if err := pageBlocked(name, p.status); err != nil {
		return loginResult{}, err
	}
	if err := c.pause(ctx, mobileDelay, progress); err != nil {
		return loginResult{}, err
	}
	header := browserHeader(iosLoginUserAgent, jsonAccept, signIn)
	header.Set("Origin", c.hosts.SSO)
	return c.postCredentials(ctx, session, name, "mobile", c.hosts.SSO+"/mobile/api/login", url.Values{
		"clientId": {iosSSOClientID}, "locale": {"en-US"}, "service": {c.hosts.iosServiceURL()},
	}, header, c.hosts.iosServiceURL(), email, password)
}

func (c *ConnectClient) portalLogin(ctx context.Context, email, password string, progress func(string)) (loginResult, error) {
	const name = "web portal sign-in"
	session := c.newSession()
	signIn := withQuery(c.hosts.SSO+"/portal/sso/en-US/sign-in", url.Values{
		"clientId": {portalSSOClientID}, "service": {c.hosts.portalServiceURL()},
	})
	p, err := fetch(ctx, session, http.MethodGet, signIn, browserHeader(desktopUserAgent, htmlAccept, ""), nil)
	if err != nil {
		return loginResult{}, &strategyError{strategy: name, reason: err.Error()}
	}
	if err := pageBlocked(name, p.status); err != nil {
		return loginResult{}, err
	}
	if err := c.pause(ctx, portalDelay, progress); err != nil {
		return loginResult{}, err
	}
	header := browserHeader(desktopUserAgent, jsonAccept, signIn)
	header.Set("Origin", c.hosts.SSO)
	return c.postCredentials(ctx, session, name, "portal", c.hosts.SSO+"/portal/api/login", url.Values{
		"clientId": {portalSSOClientID}, "locale": {"en-US"}, "service": {c.hosts.portalServiceURL()},
	}, header, c.hosts.portalServiceURL(), email, password)
}

func pageBlocked(name string, status int) error {
	switch {
	case blocked(status):
		return &strategyError{strategy: name, reason: fmt.Sprintf("sign-in page returned HTTP %d (rate limited or blocked)", status), rateLimited: true}
	case status == http.StatusForbidden:
		return &strategyError{strategy: name, reason: "sign-in page returned HTTP 403 (bot challenge)"}
	}
	return nil
}

// jsonLoginResponse is the part of Garmin's answer this client reads. The
// rest can hold account details, so it is never echoed.
type jsonLoginResponse struct {
	ResponseStatus struct {
		Type string `json:"type"`
	} `json:"responseStatus"`
	ServiceTicketID string `json:"serviceTicketId"`
	CustomerMFAInfo struct {
		MFALastMethodUsed string `json:"mfaLastMethodUsed"`
	} `json:"customerMfaInfo"`
	Error struct {
		StatusCode string `json:"status-code"`
	} `json:"error"`
}

func (c *ConnectClient) postCredentials(ctx context.Context, session *http.Client, name, flow, endpoint string,
	params url.Values, header http.Header, serviceURL, email, password string) (loginResult, error) {
	payload, err := json.Marshal(map[string]any{"username": email, "password": password, "rememberMe": true, "captchaToken": ""})
	if err != nil {
		return loginResult{}, err
	}
	post := header.Clone()
	post.Set("Content-Type", "application/json")
	p, err := fetch(ctx, session, http.MethodPost, withQuery(endpoint, params), post, bytes.NewReader(payload))
	if err != nil {
		return loginResult{}, &strategyError{strategy: name, reason: err.Error()}
	}
	if blocked(p.status) {
		return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("HTTP %d (rate limited or blocked)", p.status), rateLimited: true}
	}
	if p.status == http.StatusForbidden {
		return loginResult{}, &strategyError{strategy: name, reason: "HTTP 403 (bot challenge)"}
	}
	var res jsonLoginResponse
	if err := json.Unmarshal([]byte(p.body), &res); err != nil {
		return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("HTTP %d with a page that isn't JSON", p.status)}
	}
	switch res.ResponseStatus.Type {
	case "SUCCESSFUL":
		if res.ServiceTicketID == "" {
			return loginResult{}, &strategyError{strategy: name, reason: "no service ticket in the response"}
		}
		return loginResult{ticket: res.ServiceTicketID, serviceURL: serviceURL}, nil
	case "MFA_REQUIRED":
		method := res.CustomerMFAInfo.MFALastMethodUsed
		if method == "" {
			method = "email"
		}
		return loginResult{mfa: &pendingMFA{
			flow: flow, session: session, method: method, params: params, headers: post, serviceURL: serviceURL,
		}}, nil
	case "INVALID_USERNAME_PASSWORD":
		return loginResult{}, errBadCredentials
	case "CAPTCHA_REQUIRED":
		return loginResult{}, &strategyError{strategy: name, reason: "CAPTCHA required (bot challenge)"}
	}
	if res.Error.StatusCode == "429" {
		return loginResult{}, &strategyError{strategy: name, reason: "rate limited", rateLimited: true}
	}
	status := res.ResponseStatus.Type
	if status == "" {
		status = "unknown"
	}
	return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("HTTP %d, responseStatus %s", p.status, status)}
}

func (c *ConnectClient) widgetQuery() url.Values {
	embed := c.hosts.widgetEmbedURL()
	return url.Values{
		"id": {"gauth-widget"}, "embedWidget": {"true"}, "gauthHost": {embed}, "service": {embed},
		"source": {embed}, "redirectAfterAccountLoginUrl": {embed}, "redirectAfterAccountCreationUrl": {embed},
	}
}

func (c *ConnectClient) widgetLogin(ctx context.Context, email, password string, progress func(string)) (loginResult, error) {
	const name = "SSO widget sign-in"
	session := c.newSession()
	embed := c.hosts.widgetEmbedURL()
	embedPage := withQuery(embed, url.Values{"id": {"gauth-widget"}, "embedWidget": {"true"}, "gauthHost": {c.hosts.SSO + "/sso"}})
	p, err := fetch(ctx, session, http.MethodGet, embedPage, browserHeader(desktopUserAgent, htmlAccept, ""), nil)
	if err != nil {
		return loginResult{}, &strategyError{strategy: name, reason: err.Error()}
	}
	if err := pageBlocked(name, p.status); err != nil {
		return loginResult{}, err
	}
	if p.status < 200 || p.status >= 300 {
		return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("embed page returned HTTP %d", p.status)}
	}
	params := c.widgetQuery()
	signIn := withQuery(c.hosts.SSO+"/sso/signin", params)
	p, err = fetch(ctx, session, http.MethodGet, signIn, browserHeader(desktopUserAgent, htmlAccept, embed), nil)
	if err != nil {
		return loginResult{}, &strategyError{strategy: name, reason: err.Error()}
	}
	if err := pageBlocked(name, p.status); err != nil {
		return loginResult{}, err
	}
	csrf := csrfPattern.FindStringSubmatch(p.body)
	if csrf == nil {
		return loginResult{}, &strategyError{strategy: name, reason: "the sign-in page had no CSRF token"}
	}
	if err := c.pause(ctx, widgetDelay, progress); err != nil {
		return loginResult{}, err
	}
	form := url.Values{"username": {email}, "password": {password}, "embed": {"true"}, "_csrf": {csrf[1]}}
	post := browserHeader(desktopUserAgent, htmlAccept, p.url)
	post.Set("Content-Type", "application/x-www-form-urlencoded")
	p, err = fetch(ctx, session, http.MethodPost, signIn, post, strings.NewReader(form.Encode()))
	if err != nil {
		return loginResult{}, &strategyError{strategy: name, reason: err.Error()}
	}
	if blocked(p.status) {
		return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("HTTP %d (rate limited or blocked)", p.status), rateLimited: true}
	}
	title := pageTitle(p.body)
	lower := strings.ToLower(title)
	switch {
	case containsAny(lower, "bad gateway", "service unavailable", "cloudflare", "502", "503"):
		return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("server error page %q", title)}
	case containsAny(lower, "locked", "invalid", "incorrect", "account error"):
		return loginResult{}, errBadCredentials
	case containsAny(lower, "unable to sign in", "unable to login"):
		return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("account restricted from web sign-in (%q); child accounts aren't supported", title)}
	}
	vars := widgetMFAVars(p.body)
	if strings.Contains(lower, "mfa") || (strings.Contains(lower, "authentication application") && vars["mfaMethod"] != "") {
		c.requestWidgetMFACode(ctx, session, vars, p.url)
		pending := &pendingMFA{flow: "widget", session: session, method: vars["mfaMethod"], params: params,
			headers: browserHeader(desktopUserAgent, htmlAccept, p.url), serviceURL: embed}
		if m := csrfPattern.FindStringSubmatch(p.body); m != nil {
			pending.csrf = m[1]
		}
		return loginResult{mfa: pending}, nil
	}
	if title != "Success" {
		return loginResult{}, &strategyError{strategy: name, reason: fmt.Sprintf("unexpected page %q", title)}
	}
	ticket := ticketPattern.FindStringSubmatch(p.body)
	if ticket == nil {
		return loginResult{}, &strategyError{strategy: name, reason: "no service ticket on the success page"}
	}
	return loginResult{ticket: ticket[1], serviceURL: embed}, nil
}

func pageTitle(html string) string {
	if m := titlePattern.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

func containsAny(s string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

func widgetMFAVars(html string) map[string]string {
	vars := map[string]string{}
	for _, m := range mfaVarsPattern.FindAllStringSubmatch(html, -1) {
		vars[m[1]] = m[2]
	}
	return vars
}

// requestWidgetMFACode asks Garmin to send an email or SMS code, as the
// widget's own "request a new code" link does. Failing here doesn't fail the
// sign-in: Garmin may have sent a code already.
func (c *ConnectClient) requestWidgetMFACode(ctx context.Context, session *http.Client, vars map[string]string, referer string) {
	method := strings.ToLower(vars["mfaMethod"])
	if (method != "email" && method != "sms") || vars["codeSentTo"] != "" {
		return
	}
	payload, err := json.Marshal(map[string]string{
		"customerGuid": vars["customerGuid"], "mfaMethod": vars["mfaMethod"], "locale": vars["locale"],
	})
	if err != nil {
		return
	}
	header := browserHeader(desktopUserAgent, jsonAccept, referer)
	header.Set("Content-Type", "application/json")
	_, _ = fetch(ctx, session, http.MethodPost,
		withQuery(c.hosts.SSO+"/sso/verifyMFA/mfaCode", url.Values{"clientId": {vars["clientId"]}}), header, bytes.NewReader(payload))
}

var errWrongMFACode = errors.New("wrong MFA code")

func (c *ConnectClient) completeMFA(ctx context.Context, pending *pendingMFA,
	prompt func(context.Context, string, bool) (string, error), progress func(string)) (loginResult, error) {
	if prompt == nil {
		return loginResult{}, &connectLoginError{"Garmin asked for an MFA code, and there is no terminal to type it in."}
	}
	method := pending.method
	if method == "" {
		method = "the authenticator app"
	}
	for attempt := range maxMFAAttempts {
		code, err := prompt(ctx, method, attempt > 0)
		if err != nil {
			return loginResult{}, err
		}
		code = strings.TrimSpace(code)
		if code == "" {
			return loginResult{}, &connectLoginError{"No MFA code entered; nothing saved."}
		}
		var result loginResult
		if pending.flow == "widget" {
			result, err = c.verifyWidgetMFA(ctx, pending, code)
		} else {
			result, err = c.verifyJSONMFA(ctx, pending, code)
		}
		if !errors.Is(err, errWrongMFACode) {
			return result, err
		}
		progress("  Garmin didn't accept that code.")
	}
	return loginResult{}, &connectLoginError{"Garmin didn't accept the MFA code; nothing saved."}
}

// verifyJSONMFA tries the flow's own endpoint and then the other flow's, which
// share the SSO cookies but may sit behind different rate limits.
func (c *ConnectClient) verifyJSONMFA(ctx context.Context, pending *pendingMFA, code string) (loginResult, error) {
	payload, err := json.Marshal(map[string]any{
		"mfaMethod": pending.method, "mfaVerificationCode": code, "rememberMyBrowser": true,
		"reconsentList": []string{}, "mfaSetup": false,
	})
	if err != nil {
		return loginResult{}, err
	}
	type endpoint struct {
		url    string
		params url.Values
	}
	endpoints := []endpoint{{c.hosts.SSO + "/" + pending.flow + "/api/mfa/verifyCode", pending.params}}
	if pending.flow == "mobile" {
		endpoints = append(endpoints, endpoint{c.hosts.SSO + "/portal/api/mfa/verifyCode", url.Values{
			"clientId": {portalSSOClientID}, "locale": {"en-US"}, "service": {c.hosts.portalServiceURL()},
		}})
	} else {
		endpoints = append(endpoints, endpoint{c.hosts.SSO + "/mobile/api/mfa/verifyCode", url.Values{
			"clientId": {iosSSOClientID}, "locale": {"en-US"}, "service": {c.hosts.iosServiceURL()},
		}})
	}
	limited := 0
	refused := false
	for _, endpoint := range endpoints {
		p, err := fetch(ctx, pending.session, http.MethodPost, withQuery(endpoint.url, endpoint.params), pending.headers, bytes.NewReader(payload))
		if err != nil {
			if ctx.Err() != nil {
				return loginResult{}, ctx.Err()
			}
			continue
		}
		if blocked(p.status) {
			limited++
			continue
		}
		var res jsonLoginResponse
		if err := json.Unmarshal([]byte(p.body), &res); err != nil {
			continue
		}
		if res.Error.StatusCode == "429" {
			limited++
			continue
		}
		if res.ResponseStatus.Type == "SUCCESSFUL" && res.ServiceTicketID != "" {
			return loginResult{ticket: res.ServiceTicketID, serviceURL: pending.serviceURL}, nil
		}
		refused = true
	}
	if limited == len(endpoints) {
		return loginResult{}, &connectLoginError{"Garmin rate-limited the MFA check; try again later. Nothing saved."}
	}
	if refused {
		return loginResult{}, errWrongMFACode
	}
	return loginResult{}, &connectLoginError{"Couldn't reach Garmin to check the MFA code; nothing saved."}
}

func (c *ConnectClient) verifyWidgetMFA(ctx context.Context, pending *pendingMFA, code string) (loginResult, error) {
	if pending.csrf == "" {
		return loginResult{}, &connectLoginError{"Garmin's MFA page had no CSRF token; nothing saved."}
	}
	form := url.Values{"mfa-code": {code}, "embed": {"true"}, "_csrf": {pending.csrf}, "fromPage": {"setupEnterMfaCode"}}
	header := pending.headers.Clone()
	header.Set("Content-Type", "application/x-www-form-urlencoded")
	p, err := fetch(ctx, pending.session, http.MethodPost,
		withQuery(c.hosts.SSO+"/sso/verifyMFA/loginEnterMfaCode", pending.params), header, strings.NewReader(form.Encode()))
	if err != nil {
		if ctx.Err() != nil {
			return loginResult{}, ctx.Err()
		}
		return loginResult{}, &connectLoginError{"Couldn't reach Garmin to check the MFA code; nothing saved."}
	}
	if blocked(p.status) {
		return loginResult{}, &connectLoginError{"Garmin rate-limited the MFA check; try again later. Nothing saved."}
	}
	if pageTitle(p.body) != "Success" {
		// The page asking again carries a fresh token for the next try.
		if m := csrfPattern.FindStringSubmatch(p.body); m != nil {
			pending.csrf = m[1]
		}
		return loginResult{}, errWrongMFACode
	}
	ticket := ticketPattern.FindStringSubmatch(p.body)
	if ticket == nil {
		return loginResult{}, &connectLoginError{"Garmin accepted the MFA code but sent no service ticket; nothing saved."}
	}
	return loginResult{ticket: ticket[1], serviceURL: pending.serviceURL}, nil
}

// finishLogin exchanges the ticket, checks that connectapi accepts the
// tokens, and saves them. A token connectapi refuses (which depends on the
// account and region) sends the chain on to the next strategy.
func (c *ConnectClient) finishLogin(ctx context.Context, result loginResult, progress func(string)) (string, error) {
	progress("  Exchanging the sign-in for tokens...")
	tokens, err := c.exchangeTicket(ctx, result.ticket, result.serviceURL)
	if err != nil {
		return "", err
	}
	if _, err := c.send(ctx, http.MethodGet, socialProfilePath, nil, nil, tokens.AccessToken); err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && (httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden) {
			return "", &strategyError{strategy: "token check", reason: fmt.Sprintf("connectapi refused the new token (HTTP %d)", httpErr.StatusCode)}
		}
		// Anything else, such as a network error, says nothing about the token:
		// keep it.
	}
	path, err := saveConnectTokens(tokens)
	if err != nil {
		return "", fmt.Errorf("signed in, but couldn't save the tokens: %w", err)
	}
	c.auth.mu.Lock()
	c.auth.tokens = tokens
	c.auth.mu.Unlock()
	return path, nil
}

func (c *ConnectClient) exchangeTicket(ctx context.Context, ticket, serviceURL string) (connectTokens, error) {
	for _, clientID := range diClientIDs {
		resp, err := postDIToken(ctx, c.httpClient, c.hosts, clientID, "application/json,text/html;q=0.9,*/*;q=0.8", url.Values{
			"client_id": {clientID}, "service_ticket": {ticket}, "grant_type": {diGrantType}, "service_url": {serviceURL},
		})
		if err != nil {
			return connectTokens{}, err
		}
		if resp.status == http.StatusTooManyRequests {
			return connectTokens{}, &connectLoginError{"Garmin rate-limited the token exchange; try again later. Nothing saved."}
		}
		if resp.status < 200 || resp.status >= 300 {
			continue
		}
		if tokens, err := parseDIToken(resp.body, clientID); err == nil {
			return tokens, nil
		}
	}
	return connectTokens{}, &connectLoginError{"Signed in, but Garmin wouldn't exchange the sign-in for tokens; nothing saved."}
}
