package main

import (
	"fmt"
	"net/http"
	"net/url"
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
	Endpoint string // URL of the dmbcau authenticator API
}

func (d DjangoAuth) Auth(r *http.Request, c *Client) error {
	ip := r.Header.Get("X-Forwarded-For")
	sessionID, err := r.Cookie("sessionid")
	if err != nil {
		return &AuthError{http.StatusUnprocessableEntity, "no sessionid cookie"}
	}

	params := url.Values{}
	params.Add("sessionid", sessionID.Value)
	params.Add("clientip", ip)

	res, err := http.Get(d.Endpoint + "?" + params.Encode())
	if err != nil {
		return fmt.Errorf("django: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		return &AuthError{res.StatusCode, "django refused"}
	}
	return nil
}

// NoAuth does not enforce authentication.
type NoAuth struct{}

func (NoAuth) Auth(r *http.Request, c *Client) error {
	return nil
}
