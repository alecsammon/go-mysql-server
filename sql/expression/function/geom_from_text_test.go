// Copyright 2020-2021 Dolthub, Inc.
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

package function

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/expression"
)

func TestGeomFromText(t *testing.T) {
	t.Run("create valid point with well formatted string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("POINT(1 2)", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Point{X: 1, Y: 2}, v)
	})

	t.Run("create valid point with well formatted float", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("POINT(123.456 789.0)", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Point{X: 123.456, Y: 789}, v)
	})

	t.Run("create valid point with whitespace string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("   POINT   (   1    2   )   ", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Point{X: 1, Y: 2}, v)
	})

	t.Run("null string returns null", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral(nil, sql.Null))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(nil, v)
	})

	t.Run("create point with bad string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("badpoint(1 2)", sql.Blob))

		_, err := f.Eval(sql.NewEmptyContext(), nil)
		require.Error(err)
	})

	t.Run("create valid linestring with well formatted string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("LINESTRING(1 2, 3 4)", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Linestring{Points: []sql.Point{{1, 2}, {3, 4}}}, v)
	})

	t.Run("create valid linestring with float", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("LINESTRING(123.456 789.0, 987.654 321.0)", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Linestring{Points: []sql.Point{{123.456, 789}, {987.654, 321}}}, v)
	})

	t.Run("create valid linestring with whitespace string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("   LINESTRING   (   1    2   ,   3    4   )   ", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Linestring{Points: []sql.Point{{1, 2}, {3, 4}}}, v)
	})

	t.Run("null string returns null", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral(nil, sql.Null))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(nil, v)
	})

	t.Run("create linestring with bad string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("badlinestring(1 2)", sql.Blob))

		_, err := f.Eval(sql.NewEmptyContext(), nil)
		require.Error(err)
	})

	t.Run("create valid polygon with well formatted string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("POLYGON((0 0, 0 1, 1 0, 0 0))", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Polygon{Lines: []sql.Linestring{{Points: []sql.Point{{0, 0}, {0, 1}, {1, 0}, {0, 0}}}}}, v)
	})

	t.Run("create valid polygon with multiple lines", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("POLYGON((0 0, 0 1, 1 0, 0 0), (0 0, 1 0, 1 1, 0 1, 0 0))", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Polygon{Lines: []sql.Linestring{{Points: []sql.Point{{0, 0}, {0, 1}, {1, 0}, {0, 0}}}, {Points: []sql.Point{{0, 0}, {1, 0}, {1, 1}, {0, 1}, {0, 0}}}}}, v)
	})

	t.Run("create valid linestring with whitespace string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("   POLYGON    (   (   0    0    ,   0    1   ,   1    0   ,   0    0   )   )   ", sql.Blob))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(sql.Polygon{Lines: []sql.Linestring{{Points: []sql.Point{{0, 0}, {0, 1}, {1, 0}, {0, 0}}}}}, v)
	})

	t.Run("null string returns null", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral(nil, sql.Null))

		v, err := f.Eval(sql.NewEmptyContext(), nil)
		require.NoError(err)
		require.Equal(nil, v)
	})

	t.Run("create polygon with non linear ring", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("polygon((1 2, 3 4))", sql.Blob))

		_, err := f.Eval(sql.NewEmptyContext(), nil)
		require.Error(err)
	})

	t.Run("create polygon with bad string", func(t *testing.T) {
		require := require.New(t)
		f := NewGeomFromText(expression.NewLiteral("badlinestring(1 2)", sql.Blob))

		_, err := f.Eval(sql.NewEmptyContext(), nil)
		require.Error(err)
	})
}
