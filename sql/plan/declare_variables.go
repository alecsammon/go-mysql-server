// Copyright 2022 Dolthub, Inc.
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
	"io"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/expression"
)

// DeclareVariables represents the DECLARE statement for local variables.
type DeclareVariables struct {
	Names []string
	Type  sql.Type
	ids   []int
	pRef  *expression.ProcedureReference
}

var _ sql.Node = (*DeclareVariables)(nil)

// NewDeclareVariables returns a new *DeclareVariables node.
func NewDeclareVariables(names []string, typ sql.Type) *DeclareVariables {
	return &DeclareVariables{
		Names: names,
		Type:  typ,
	}
}

// Resolved implements the interface sql.Node.
func (d *DeclareVariables) Resolved() bool {
	return true
}

// String implements the interface sql.Node.
func (d *DeclareVariables) String() string {
	return fmt.Sprintf("DECLARE %s %s", strings.Join(d.Names, ", "), d.Type.String())
}

// Schema implements the interface sql.Node.
func (d *DeclareVariables) Schema() sql.Schema {
	return nil
}

// Children implements the interface sql.Node.
func (d *DeclareVariables) Children() []sql.Node {
	return nil
}

// WithChildren implements the interface sql.Node.
func (d *DeclareVariables) WithChildren(children ...sql.Node) (sql.Node, error) {
	return NillaryWithChildren(d, children...)
}

// CheckPrivileges implements the interface sql.Node.
func (d *DeclareVariables) CheckPrivileges(ctx *sql.Context, opChecker sql.PrivilegedOperationChecker) bool {
	return true
}

// RowIter implements the interface sql.Node.
func (d *DeclareVariables) RowIter(ctx *sql.Context, row sql.Row) (sql.RowIter, error) {
	return &declareVariablesIter{d}, nil
}

// WithParamReference returns a new *DeclareVariables containing the given *expression.ProcedureReference.
func (d *DeclareVariables) WithParamReference(pRef *expression.ProcedureReference) *DeclareVariables {
	nd := *d
	nd.pRef = pRef
	return &nd
}

// WithIds returns a new *DeclareVariables containing the given ids.
func (d *DeclareVariables) WithIds(ctx *sql.Context, ids []int) (sql.Node, error) {
	if len(ids) != len(d.Names) {
		return nil, fmt.Errorf("expected %d declaration ids but received %d", len(d.Names), len(ids))
	}
	nd := *d
	nd.ids = ids
	return &nd, nil
}

// declareVariablesIter is the sql.RowIter of *DeclareVariables.
type declareVariablesIter struct {
	*DeclareVariables
}

var _ sql.RowIter = (*declareVariablesIter)(nil)

// Next implements the interface sql.RowIter.
func (d *declareVariablesIter) Next(ctx *sql.Context) (sql.Row, error) {
	for i := range d.ids {
		if err := d.pRef.InitializeVariable(d.ids[i], d.Names[i], d.Type, nil); err != nil {
			return nil, err
		}
	}
	return nil, io.EOF
}

// Close implements the interface sql.RowIter.
func (d *declareVariablesIter) Close(ctx *sql.Context) error {
	return nil
}
