package forge

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sylirre/git-archive/internal/safe"
)

const (
	maxBody     = 32 << 20 // per API response
	maxPages    = 2000
	maxAttempts = 4
)

// api is a small JSON-over-HTTPS client with the safety rails every
// provider needs.
type api struct {
	base     *url.URL
	token    string
	auth     func(h http.Header, token string)
	accept   string
	extra    http.Header
	client   *http.Client
	ua       string
	protocol string
	sleep    func(context.Context, time.Duration) error
}

// apiBases are the default API locations per provider.
func defaultAPIBase(s Spec) string {
	scheme := "https"
	if s.Scheme == "http" && s.Insecure {
		scheme = "http"
	}
	switch s.Provider {
	case GitHub:
		if s.Host == "github.com" {
			return "https://api.github.com"
		}
		return scheme + "://" + s.Host + "/api/v3" // GitHub Enterprise Server
	case GitLab:
		return scheme + "://" + s.Host + "/api/v4"
	case Gitea:
		return scheme + "://" + s.Host + "/api/v1"
	}
	return ""
}

func newAPI(s Spec, o Options) (*api, error) {
	raw := s.API
	if raw == "" {
		raw = defaultAPIBase(s)
	}
	base, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || base.Host == "" {
		return nil, fmt.Errorf("invalid API URL %q", safe.Term(raw))
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && s.Insecure) {
		return nil, fmt.Errorf("API URL %s must use https (or set insecure)", safe.Term(raw))
	}
	client := o.HTTP
	if client == nil {
		client = defaultHTTPClient()
	}
	// Never follow a redirect away from the API origin: the token must
	// not leak, and responses from elsewhere are not trustworthy.
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != base.Scheme || req.URL.Host != base.Host {
			return fmt.Errorf("refusing redirect to %s", safe.Term(req.URL.Redacted()))
		}
		return nil
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "git-archive"
	}
	protocol := o.Protocol
	if protocol == "" {
		protocol = "https"
	}
	a := &api{base: base, token: Token(s, o.Getenv), client: &c, ua: ua, protocol: protocol, extra: http.Header{},
		sleep: sleepCtx}
	switch s.Provider {
	case GitHub:
		a.accept = "application/vnd.github+json"
		a.extra.Set("X-GitHub-Api-Version", "2022-11-28")
		a.auth = func(h http.Header, t string) { h.Set("Authorization", "Bearer "+t) }
	case GitLab:
		a.accept = "application/json"
		a.auth = func(h http.Header, t string) { h.Set("Authorization", "Bearer "+t) }
	case Gitea:
		a.accept = "application/json"
		a.auth = func(h http.Header, t string) { h.Set("Authorization", "token "+t) }
	}
	return a, nil
}

func defaultHTTPClient() *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: tr, Timeout: 2 * time.Minute}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// endpoint builds an API URL from path segments (escaped individually)
// and query parameters.
func (a *api) endpoint(query url.Values, segments ...string) *url.URL {
	u := *a.base
	var raw strings.Builder
	raw.WriteString(strings.TrimRight(a.base.EscapedPath(), "/"))
	for _, s := range segments {
		raw.WriteByte('/')
		raw.WriteString(url.PathEscape(s))
	}
	u.RawPath = raw.String()
	u.Path, _ = url.PathUnescape(u.RawPath)
	u.RawQuery = query.Encode()
	return &u
}

// APIError is a non-success HTTP response.
type APIError struct {
	Status  int
	Message string // sanitised server message
	URL     string
	Reset   time.Time // rate limit reset, if reported
	Hint    string
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s: HTTP %d", e.URL, e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	if !e.Reset.IsZero() {
		msg += fmt.Sprintf(" (rate limit resets at %s)", e.Reset.Local().Format("15:04:05"))
	}
	if e.Hint != "" {
		msg += "; " + e.Hint
	}
	return msg
}

func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

// get fetches u and decodes the JSON body into v. It returns the
// response headers for pagination. Transient failures (5xx, short
// secondary rate limits, network errors) are retried with backoff.
func (a *api) get(ctx context.Context, u *url.URL, v any) (http.Header, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			if err := a.sleep(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
				return nil, err
			}
		}
		hdr, retry, err := a.getOnce(ctx, u, v)
		if err == nil {
			return hdr, nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			break
		}
		var ae *APIError
		if errors.As(err, &ae) && !ae.Reset.IsZero() {
			wait := time.Until(ae.Reset) + time.Second
			if wait > 90*time.Second {
				break
			}
			if err := a.sleep(ctx, wait); err != nil {
				return nil, err
			}
		}
	}
	return nil, lastErr
}

func (a *api) getOnce(ctx context.Context, u *url.URL, v any) (hdr http.Header, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", a.accept)
	req.Header.Set("User-Agent", a.ua)
	for k, vs := range a.extra {
		req.Header[k] = vs
	}
	if a.token != "" {
		a.auth(req.Header, a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, ctx.Err() == nil, fmt.Errorf("%s: %w", u.Redacted(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, true, fmt.Errorf("%s: reading response: %w", u.Redacted(), err)
	}
	if len(body) > maxBody {
		return nil, false, fmt.Errorf("%s: response larger than %d bytes", u.Redacted(), maxBody)
	}
	if resp.StatusCode != http.StatusOK {
		ae := &APIError{Status: resp.StatusCode, URL: u.Redacted(), Message: serverMessage(body)}
		remaining := resp.Header.Get("X-RateLimit-Remaining")
		limited := resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode == http.StatusForbidden && (remaining == "0" || strings.Contains(strings.ToLower(ae.Message), "rate limit")))
		if limited {
			if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				ae.Reset = time.Now().Add(time.Duration(secs) * time.Second)
			} else if ts, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				ae.Reset = time.Unix(ts, 0)
			}
			if a.token == "" {
				ae.Hint = "set a token (e.g. GITHUB_TOKEN) to raise the limit"
			}
			return nil, true, ae
		}
		if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && a.token != "" {
			ae.Hint = "check that the token is valid and has read access"
		}
		return nil, resp.StatusCode >= 500, ae
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		return nil, false, fmt.Errorf("%s: unexpected content type %q", u.Redacted(), safe.Term(ct))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return nil, false, fmt.Errorf("%s: invalid JSON: %v", u.Redacted(), err)
	}
	return resp.Header, false, nil
}

func serverMessage(body []byte) string {
	var m struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	var s string
	switch msg := m.Message.(type) {
	case string:
		s = msg
	case nil:
		s = m.Error
	default:
		b, _ := json.Marshal(msg)
		s = string(b)
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return safe.TermLine(s)
}

// paginate fetches u and follows rel="next" links, decoding each page
// into a fresh slice and passing it to fn. Links must stay on the API
// origin. fallbackNext computes the next page when a server sends no Link
// header (returns nil to stop).
func paginate[T any](ctx context.Context, a *api, u *url.URL, fn func([]T) error, fallbackNext func(cur *url.URL, n int) *url.URL) error {
	seen := map[string]bool{}
	for page := 0; u != nil; page++ {
		if page >= maxPages {
			return fmt.Errorf("%s: more than %d pages, giving up", a.base.Host, maxPages)
		}
		key := u.String()
		if seen[key] {
			return fmt.Errorf("%s: pagination loops", u.Redacted())
		}
		seen[key] = true
		var items []T
		hdr, err := a.get(ctx, u, &items)
		if err != nil {
			return err
		}
		if err := fn(items); err != nil {
			return err
		}
		next, err := a.nextLink(hdr.Get("Link"))
		if err != nil {
			return err
		}
		if next == nil && fallbackNext != nil && len(items) > 0 {
			next = fallbackNext(u, len(items))
		}
		u = next
	}
	return nil
}

// nextLink extracts and validates the rel="next" target of a Link header.
func (a *api) nextLink(header string) (*url.URL, error) {
	for _, part := range strings.Split(header, ",") {
		segs := strings.Split(part, ";")
		if len(segs) < 2 {
			continue
		}
		isNext := false
		for _, p := range segs[1:] {
			p = strings.TrimSpace(p)
			if p == `rel="next"` || p == "rel=next" {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(segs[0]), "<>")
		next, err := a.base.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid pagination link %q", safe.Term(raw))
		}
		if next.Scheme != a.base.Scheme || next.Host != a.base.Host {
			return nil, fmt.Errorf("refusing pagination link to %s", safe.Term(next.Redacted()))
		}
		return next, nil
	}
	return nil, nil
}
