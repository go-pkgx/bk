// Package schema embeds the authoritative JSON Schema for a pkgx pantry
// package.yml. The schema is derived from libpkgx's parser and validated
// against the full pkgxdev/pantry corpus (0 rejections over 1890 recipes).
package schema

import _ "embed"

// PackageJSON is the raw JSON Schema (draft 2020-12) for a package.yml recipe.
//
//go:embed package.schema.json
var PackageJSON []byte
