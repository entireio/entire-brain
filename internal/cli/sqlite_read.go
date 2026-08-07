package cli

import (
	"net/url"
	"path/filepath"
)

// sqliteReadOnlyDSN opens an immutable generated index without asking SQLite
// to create journal or shared-memory sidecars beside it. Agents commonly run
// with read access to the brain store but no write access to its data directory.
func sqliteReadOnlyDSN(path string) string {
	uriPath := filepath.ToSlash(path)
	// A Windows drive-letter path must be absolute in a file URI. Without the
	// leading slash, url.URL emits file:C:%5C..., which SQLite interprets as a
	// URI with the drive path in the authority component and rejects.
	if volume := filepath.VolumeName(path); len(volume) == 2 && volume[1] == ':' {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	query := u.Query()
	query.Set("immutable", "1")
	query.Set("mode", "ro")
	u.RawQuery = query.Encode()
	return u.String()
}
