// Copyright 2023 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plan

import (
	"fmt"
	"github.com/dolthub/go-mysql-server/sql"
)

// RangeHeap is a Node that wraps a table with min and max range columns. When used as a secondary provider in Join
// operations, it can efficiently compute the rows whose ranges bound the value from the other table. When the ranges
// don't overlap, the amortized complexity is O(1) for each result row.
type RangeHeap struct {
	UnaryNode
	ValueColumnIndex   int
	MinColumnIndex     int
	MaxColumnIndex     int
	ComparisonType     sql.Type
	RangeIsClosedBelow bool
	RangeIsClosedAbove bool
}

var _ sql.Node = (*RangeHeap)(nil)

func NewRangeHeap(child sql.Node, lhsSchema sql.Schema, rhsSchema sql.Schema, value, min, max string, rangeIsClosedBelow, rangeIsClosedAbove bool) (*RangeHeap, error) {
	// TODO: IndexOfColName is Only safe for schemas corresponding to a single table, where the source of the column is irrelevant.
	maxColumnIndex := rhsSchema.IndexOfColName(max)
	newSr := &RangeHeap{
		ValueColumnIndex:   lhsSchema.IndexOfColName(value),
		MinColumnIndex:     rhsSchema.IndexOfColName(min),
		MaxColumnIndex:     maxColumnIndex,
		RangeIsClosedBelow: rangeIsClosedBelow,
		RangeIsClosedAbove: rangeIsClosedAbove,
	}
	newSr.Child = child
	return newSr, nil
}

func (s *RangeHeap) String() string {
	return s.Child.String()
}

func (s *RangeHeap) WithChildren(children ...sql.Node) (sql.Node, error) {
	if len(children) != 1 {
		return nil, fmt.Errorf("ds")
	}

	s2 := *s
	s2.UnaryNode = UnaryNode{Child: children[0]}
	return &s2, nil
}

func (s *RangeHeap) CheckPrivileges(ctx *sql.Context, opChecker sql.PrivilegedOperationChecker) bool {
	return s.Child.CheckPrivileges(ctx, opChecker)
}

var _ sql.Node = (*RangeHeap)(nil)
