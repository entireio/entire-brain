package apiurl

import "net/http"

// WithoutRedirects copies the client's settings while keeping credential-bearing
// API requests at their validated target. Redirects can downgrade HTTPS or forward
// POST bodies to another origin. Return the original response for status handling.
// The caller's client and transport are never mutated.
func WithoutRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
