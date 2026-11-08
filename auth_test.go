package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// fakeDjango is a stand-in for the dmbcau authenticator API.
type fakeDjango struct {
	*httptest.Server
	status int // answer to every request

	mu      sync.Mutex
	queries []url.Values
}

func newFakeDjango(t *testing.T, status int) *fakeDjango {
	t.Helper()
	d := &fakeDjango{status: status}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.queries = append(d.queries, r.URL.Query())
		d.mu.Unlock()
		w.WriteHeader(d.status)
	}))
	t.Cleanup(d.Close)
	return d
}

func (d *fakeDjango) lastQuery(t *testing.T) url.Values {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.queries) == 0 {
		t.Fatal("django was not called")
	}
	return d.queries[len(d.queries)-1]
}

func authRequest(cookie string) *http.Request {
	r := httptest.NewRequest("GET", "/?subscribe=x", nil)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: "sessionid", Value: cookie})
	}
	return r
}

func authStatus(err error) int {
	var authErr *AuthError
	if errors.As(err, &authErr) {
		return authErr.Status
	}
	return 0
}

func TestDjangoAuthAccepts(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	auth := DjangoAuth{Endpoint: d.URL + "/authenticator/"}

	if err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"})); err != nil {
		t.Fatalf("Auth: %s", err)
	}
	if got := d.lastQuery(t).Get("sessionid"); got != "s3cr3t" {
		t.Errorf("sessionid = %q", got)
	}
}

func TestDjangoAuthRefuses(t *testing.T) {
	d := newFakeDjango(t, http.StatusForbidden)
	auth := DjangoAuth{Endpoint: d.URL + "/authenticator/"}

	err := auth.Auth(authRequest("expired"), newClientFor("1.2.3.4", []string{"x"}))
	if got := authStatus(err); got != http.StatusForbidden {
		t.Errorf("err = %v, want status 403", err)
	}
}

func TestDjangoAuthNeedsCookie(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	auth := DjangoAuth{Endpoint: d.URL + "/authenticator/"}

	err := auth.Auth(authRequest(""), newClientFor("1.2.3.4", []string{"x"}))
	if got := authStatus(err); got != http.StatusUnprocessableEntity {
		t.Errorf("err = %v, want status 422", err)
	}
	if len(d.queries) != 0 {
		t.Error("django was called without a session")
	}
}

func TestDjangoAuthDown(t *testing.T) {
	d := newFakeDjango(t, http.StatusOK)
	d.Close()
	auth := DjangoAuth{Endpoint: d.URL + "/authenticator/"}

	err := auth.Auth(authRequest("s3cr3t"), newClientFor("1.2.3.4", []string{"x"}))
	if err == nil || authStatus(err) != 0 {
		t.Errorf("err = %v, want a plain error", err)
	}
}

// refuseAll refuses every request with status.
type refuseAll struct{ status int }

func (a refuseAll) Auth(*http.Request, *Client) error {
	return &AuthError{a.status, "test"}
}

type failAll struct{}

func (failAll) Auth(*http.Request, *Client) error { return errors.New("boom") }

func TestServerAnswersAuthFailures(t *testing.T) {
	for _, tc := range []struct {
		auth Authenticator
		want int
	}{
		{refuseAll{http.StatusForbidden}, http.StatusForbidden},
		{failAll{}, http.StatusInternalServerError},
	} {
		srv, h := newTestServerWithAuth(t, tc.auth)
		resp, err := http.Get(srv.URL + "/?subscribe=x")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%T: status %d, want %d", tc.auth, resp.StatusCode, tc.want)
		}
		if n := h.clientCount(); n != 0 {
			t.Errorf("%T: refused client was subscribed", tc.auth)
		}
	}
}
