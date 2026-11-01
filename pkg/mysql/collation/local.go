//go:build !collations_library

/*
Copyright 2021 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package collations

// MySQL8 is the collation Environment for MySQL 8.
func MySQL8() *Environment {
	return fetchCacheEnvironment(collverMySQL8)
}

// Local modification (shardweave): CollationForType was removed together with
// the vitess.io/vitess/go/sqltypes dependency. The proxy reads the collation
// ID directly from the result-set ColumnDefinition (result.Field.Charset), so
// deriving it from a sqltypes.Type is not needed. See NOTICE.
