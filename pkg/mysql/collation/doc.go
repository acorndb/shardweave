// Package collations is a first-party copy of
// vitess.io/vitess/go/mysql/collations (Apache-2.0).
//
// Source: github.com/vitessio/vitess commit
// 6c6c0036ab2a3eae20195a51f1890103fe3b0ddb (2026-10-30).
// The upstream checkout available at migration time carried no release tags, so
// the commit is the reproducible lock; switching to a release tag is tracked as
// an open item in docs/design/merge-engine-design.md §13.7.
//
// Local modifications (see NOTICE for the full list):
//   - import paths rewritten to this module;
//   - CollationForType removed, eliminating the sqltypes dependency;
//   - Hash(*vthash.Hasher, ...) removed from the Collation interface and every
//     implementation, eliminating the vthash dependency;
//   - generated CachedSize files removed, eliminating the hack dependency;
//   - charset.ConvertFromBytes uses a plain []byte conversion;
//   - test files that require vitess-only test utilities were dropped.
package collations
