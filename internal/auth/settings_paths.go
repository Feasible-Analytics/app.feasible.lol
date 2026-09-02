//
// settings_paths.go
// The settings routes this package answers, for the packages that mount beside it.
//
// Created: 2026-09-02
// Copyright (c) 2026 Cloudmanic Labs, LLC. All rights reserved.
//

package auth

import "strings"

// SettingsPaths returns every route pattern this package registers at
// /settings, read from the same table the mux is built from. Other packages
// mount their own settings screens on the same host and use this list to prove
// none of theirs shadows, or is shadowed by, one of ours.
func SettingsPaths() []string {
	var out []string

	for _, entry := range (&Handler{}).routeTable() {
		_, path, ok := strings.Cut(entry.pattern, " ")
		if !ok {
			path = entry.pattern
		}

		if path == "/settings" || strings.HasPrefix(path, "/settings/") {
			out = append(out, entry.pattern)
		}
	}

	return out
}
