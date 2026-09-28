// Package logical applies a recipe override that says WHAT it changes rather
// than WHERE the lines are.
//
// # Why not a unified diff
//
// go-pkgx/packages carries 242 overrides as `git diff` output against
// upstream's package.yml. A diff is anchored on the text AROUND the change, so
// it stops applying the moment upstream edits a neighbouring line — and
// nothing downstream says so. Measured on 2026-09-24: 3 of 231 had silently
// stopped, among them the -Werror switch mozilla.org/nss needs, whose absence
// cost two rebuilds and a duplicate fix for a defect that was already fixed.
// On 2026-09-27 a patch that `git apply --check` accepted was refused by the
// builder's own applier, because git searches for context with an offset and
// go-gitdiff does not; it merged broken.
//
// A diff also cannot tell two very different situations apart. "Does not
// apply" means either *the thing I edit is gone* or *upstream already did what
// I was doing*. The first is a defect, the second is an override that should be
// deleted. This package reports them separately — see Outcome.
//
// # What the overrides actually do
//
// The verbs here are not invented. Every one of the 242 overrides was applied
// to a pristine pantry and the DOCUMENT compared before and after, with list
// elements matched by value:
//
//	change                           153   a scalar became another scalar
//	set                               44   a key appeared
//	change-substring                  23   a string gained or lost a run
//	list-append                       15
//	list-replace-element-substring     9
//	project-created                    8
//	remove                             7
//	list-rewrite                       7
//	list-replace-element               4
//	list-remove                        1
//
// 171 of the 213 changed projects need only set/remove/change and whole-list
// operations; the other 42 are almost all one string edited inside one command.
// So: a merge, a removal, an insertion, and a substitution.
//
// # Semantics, and where each rule comes from
//
// Maps DEEP MERGE, as in JSON Merge Patch (RFC 7386): a key present in the
// override replaces that key, a key absent is inherited.
//
// Lists are REPLACED, never merged. Kustomize merges some lists by a "merge
// key" and replaces others, and needs OpenAPI metadata to know which; there is
// no merge key for the lines of a shell script. Append and Prepend exist so
// that adding one element does not mean restating the list — the same reason
// Yocto has `:append` and `:prepend` rather than only assignment.
//
// Removal is a VERB, not a sentinel. RFC 7386 deletes by writing null, and
// therefore cannot ever set a value to null; more to the point here, omission
// has to keep meaning "inherit".
//
// Substitution matches a SUBSTRING of a value and never an index. Kustomize's
// own documentation warns that an index-based operation deletes the wrong
// element as soon as the base list is reordered — which is exactly as brittle
// as the line numbers this package exists to leave behind.
//
// # One HCL wrinkle
//
// A block header takes an identifier, so a map whose keys are project names
// has to be written as an object expression:
//
//	merge {
//	  build {
//	    dependencies = { "gnu.org/patch" = "*" }   // = { }, not a block
//	  }
//	}
//
// This is not ours to change: it is how the recipes themselves are written,
// and the same reader parses both.
//
// # Nothing is written
//
// Apply works on a parsed document, in memory. Rewriting package.yml would
// drop every comment in it, and those comments carry the reasons: gnu.org/grep
// spends thirteen lines explaining why it names itself as a build dependency.
// A textual diff preserved them by only touching what it changed; a document
// rewrite would not.
package logical
