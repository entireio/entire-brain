package cli

import "net/url"

// sqliteReadOnlyDSN opens an immutable generated index without asking SQLite
// to create journal or shared-memory sidecars beside it. Agents commonly run
// with read access to the brain store but no write access to its data directory.
func sqliteReadOnlyDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Set("immutable", "1")
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	return u.String()
}
