package sseredis

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
)

// Authenticator authorize a subscribe request.
type Authenticator interface {
	// Auth returns error on failure, nil otherwise.
	Auth(w http.ResponseWriter, r *http.Request) error
}

var djangoServer = flag.String("dmbcau", "http://dmbcau/authenticator/", "dmbcau auth API endpoint")

// DjangoAuth authenticate against active Django sessions.
func DjangoAuth(w http.ResponseWriter, r *http.Request) error {
	ip := r.Header.Get("X-Forwarded-For")
	sessionsID, err := r.Cookie("sessionid")
	if err != nil {
		Warn("Not valid cookie for %s", ip)
		w.WriteHeader(http.StatusUnprocessableEntity)
		return errors.New("Not valid cookie")
	}

	query := fmt.Sprintf("%s?sessionid=%s", *djangoServer, sessionsID.Value)
	if res, err := http.Get(query); err != nil {
		Error("Django error [%s]: %s", err, ip)
		w.WriteHeader(http.StatusInternalServerError)
		return errors.New("Django error")
	} else if res.StatusCode != http.StatusOK {
		Warn("Django not OK [%d]: %s", res.StatusCode, ip)
		w.WriteHeader(res.StatusCode)
		return errors.New("Django not OK")
	} // else authenticate
	Info("Authenticated %s", ip)
	return nil
}

// NoAuth does not enforce authentication.
func NoAuth(w http.ResponseWriter, r *http.Request) error {
	return nil
}
