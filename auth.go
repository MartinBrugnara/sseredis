package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// Authenticator authorizes a subscribe request.
type Authenticator interface {
	// Auth returns nil if the request r may open the stream for client c,
	// an error otherwise. An *AuthError sets the HTTP status of the answer
	// (500 for other errors).
	Auth(r *http.Request, c *Client) error
}

// AuthError is a refused authentication.
type AuthError struct {
	Status int    // HTTP status to answer
	Reason string // logged, not sent to the client
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, http.StatusText(e.Status), e.Reason)
}

// DjangoAuth authenticates against active Django sessions, and lets Django
// decide whether the session may read the requested channels.
type DjangoAuth struct {
	Endpoint string       // URL of the dmbcau authenticator API
	Client   *http.Client // see NewDjangoAuth
}

// NewDjangoAuth returns a DjangoAuth calling endpoint, giving up after
// timeout. If socket is not empty, every call connects to that unix
// socket whatever the host of endpoint, which still gives the scheme, the
// Host header and the path.
func NewDjangoAuth(endpoint, socket string, timeout time.Duration) DjangoAuth {
	var transport http.RoundTripper // nil: http.DefaultTransport
	if socket != "" {
		transport = unixTransport(socket)
	}
	return DjangoAuth{
		Endpoint: endpoint,
		Client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			// The API answers itself; following a redirect (e.g. to a
			// login page answering 200) could accept anybody.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// unixTransport returns a transport connecting to the unix socket at path
// for every request.
func unixTransport(path string) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// The socket is the server: an HTTP_PROXY from the environment must
	// not turn the requests into proxy requests.
	t.Proxy = nil
	var d net.Dialer
	t.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", path)
	}
	return t
}

func (d DjangoAuth) Auth(r *http.Request, c *Client) error {
	sessionID, err := r.Cookie("sessionid")
	if err != nil {
		return &AuthError{http.StatusUnprocessableEntity, "no sessionid cookie"}
	}

	params := url.Values{}
	params.Add("sessionid", sessionID.Value)
	params.Add("clientip", c.ip)
	for _, ch := range c.Channels() {
		params.Add("channel", ch)
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET", d.Endpoint+"?"+params.Encode(), nil)
	if err != nil {
		return fmt.Errorf("django: %w", err)
	}
	res, err := d.Client.Do(req)
	if err != nil {
		return fmt.Errorf("django: %w", err)
	}
	defer res.Body.Close()
	// Read what is left of the body, so the connection can be reused.
	io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))

	switch {
	case res.StatusCode == http.StatusOK:
		return nil
	case res.StatusCode >= 400 && res.StatusCode < 500:
		return &AuthError{res.StatusCode, "django refused"}
	default:
		return &AuthError{http.StatusBadGateway, fmt.Sprintf("django answered %d", res.StatusCode)}
	}
}

// NoAuth does not enforce authentication.
type NoAuth struct{}

func (NoAuth) Auth(r *http.Request, c *Client) error {
	return nil
}
