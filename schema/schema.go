// Package schema embeds the authoritative JSON Schema for a pantry
// package.yml, derived from libpkgx's parser.
//
// ⛔⛔ THIS COMMENT USED TO SAY "validated against the full pkgxdev/pantry
// corpus (0 rejections over 1890 recipes)". That measured nothing. The schema
// carried `"additionalProperties": true` at the root and in every nested
// object, so any key at all validated — a corpus passing it was evidence the
// schema declined to ask, not evidence it described the corpus. The number
// was true and empty, and it read as a guarantee.
//
// The root and the build, test, distributable and versions objects are now
// CLOSED, and the claim is the measurement that justified closing them, taken
// 2026-11-07 with schema/corpuskeys_test.go:
//
//	go-zpkg/pantry-overlay   213 recipes    0 unknown keys
//	go-zpkg/packages          14 recipes    0 unknown keys
//	pkgxdev/pantry          1932 recipes   22 unknown keys in 42 recipes
//
// The 42 are upstream recipes relying on keys nothing here implements; they
// are refused rather than silently misread.
package schema

import _ "embed"

// PackageJSON is the raw JSON Schema (draft 2020-12) for a package.yml recipe.
//
//go:embed package.schema.json
var PackageJSON []byte
