package main

import (
	"fmt"
	"io"
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

// DjangoAuth authenticates against active Django sessions.
type DjangoAuth struct {
	Endpoint string       // URL of the dmbcau authenticator API
	Client   *http.Client // see NewDjangoAuth
}

// NewDjangoAuth returns a DjangoAuth calling endpoint, giving up after
// timeout.
func NewDjangoAuth(endpoint string, timeout time.Duration) DjangoAuth {
	return DjangoAuth{
		Endpoint: endpoint,
		Client: &http.Client{
			Timeout: timeout,
			// The API answers itself; following a redirect (e.g. to a
			// login page answering 200) could accept anybody.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (d DjangoAuth) Auth(r *http.Request, c *Client) error {
	sessionID, err := r.Cookie("sessionid")
	if err != nil {
		return &AuthError{http.StatusUnprocessableEntity, "no sessionid cookie"}
	}

	params := url.Values{}
	params.Add("sessionid", sessionID.Value)
	params.Add("clientip", c.ip)

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
