// Copyright 2020-2022 Dolthub, Inc.
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

package memory

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	errors "gopkg.in/src-d/go-errors.v1"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/expression"
	"github.com/dolthub/go-mysql-server/sql/fulltext"
	"github.com/dolthub/go-mysql-server/sql/transform"
	"github.com/dolthub/go-mysql-server/sql/types"
)

type MemTable interface {
	sql.Table
	IgnoreSessionData() bool
	UnderlyingTable() *Table
}

// Table represents an in-memory database table.
type Table struct {
	name string

	// Schema and related info
	data *TableData
	// ignoreSessionData is used to ignore session data for versioned tables (smoke tests only), unused otherwise
	ignoreSessionData bool

	// Projection info and settings
	pkIndexesEnabled bool
	projection       []string
	projectedSchema  sql.Schema
	columns          []int

	// Indexed lookups
	lookup  sql.DriverIndexLookup
	filters []sql.Expression

	db         *BaseDatabase
	tableStats *sql.TableStatistics
}

var _ sql.Table = (*Table)(nil)
var _ MemTable = (*Table)(nil)
var _ sql.InsertableTable = (*Table)(nil)
var _ sql.UpdatableTable = (*Table)(nil)
var _ sql.DeletableTable = (*Table)(nil)
var _ sql.ReplaceableTable = (*Table)(nil)
var _ sql.TruncateableTable = (*Table)(nil)
var _ sql.DriverIndexableTable = (*Table)(nil)
var _ sql.AlterableTable = (*Table)(nil)
var _ sql.IndexAlterableTable = (*Table)(nil)
var _ sql.CollationAlterableTable = (*Table)(nil)
var _ sql.ForeignKeyTable = (*Table)(nil)
var _ sql.CheckAlterableTable = (*Table)(nil)
var _ sql.RewritableTable = (*Table)(nil)
var _ sql.CheckTable = (*Table)(nil)
var _ sql.AutoIncrementTable = (*Table)(nil)
var _ sql.StatisticsTable = (*Table)(nil)
var _ sql.ProjectedTable = (*Table)(nil)
var _ sql.PrimaryKeyAlterableTable = (*Table)(nil)
var _ sql.PrimaryKeyTable = (*Table)(nil)
var _ fulltext.IndexAlterableTable = (*Table)(nil)

// NewTable creates a new Table with the given name and schema. Assigns the default collation, therefore if a different
// collation is desired, please use NewTableWithCollation.
func NewTable(db MemoryDatabase, name string, schema sql.PrimaryKeySchema, fkColl *ForeignKeyCollection) *Table {
	var baseDatabase *BaseDatabase
	// the dual table has no database
	if db != nil {
		baseDatabase = db.Database()
	}
	return NewPartitionedTableWithCollation(baseDatabase, name, schema, fkColl, 0, sql.Collation_Default)
}

// NewLocalTable returns a table suitable to use for transient non-memory applications
func NewLocalTable(db MemoryDatabase, name string, schema sql.PrimaryKeySchema, fkColl *ForeignKeyCollection) *Table {
	var baseDatabase *BaseDatabase
	// the dual table has no database
	if db != nil {
		baseDatabase = db.Database()
	}
	tbl := NewPartitionedTableWithCollation(baseDatabase, name, schema, fkColl, 0, sql.Collation_Default)
	tbl.ignoreSessionData = true
	return tbl
}

// NewTableWithCollation creates a new Table with the given name, schema, and collation.
func NewTableWithCollation(db *BaseDatabase, name string, schema sql.PrimaryKeySchema, fkColl *ForeignKeyCollection, collation sql.CollationID) *Table {
	return NewPartitionedTableWithCollation(db, name, schema, fkColl, 0, collation)
}

// NewPartitionedTable creates a new Table with the given name, schema and number of partitions. Assigns the default
// collation, therefore if a different collation is desired, please use NewPartitionedTableWithCollation.
func NewPartitionedTable(db *BaseDatabase, name string, schema sql.PrimaryKeySchema, fkColl *ForeignKeyCollection, numPartitions int) *Table {
	return NewPartitionedTableWithCollation(db, name, schema, fkColl, numPartitions, sql.Collation_Default)
}

// NewPartitionedTable creates a new Table with the given name, schema and number of partitions. Assigns the default
// collation, therefore if a different collation is desired, please use NewPartitionedTableWithCollation.
func NewPartitionedTableRevision(db *BaseDatabase, name string, schema sql.PrimaryKeySchema, fkColl *ForeignKeyCollection, numPartitions int) *TableRevision {
	tbl := NewPartitionedTableWithCollation(db, name, schema, fkColl, numPartitions, sql.Collation_Default)
	tbl.ignoreSessionData = true
	return &TableRevision{tbl}
}

// NewPartitionedTableWithCollation creates a new Table with the given name, schema, number of partitions, and collation.
func NewPartitionedTableWithCollation(db *BaseDatabase, name string, schema sql.PrimaryKeySchema, fkColl *ForeignKeyCollection, numPartitions int, collation sql.CollationID) *Table {
	var keys [][]byte
	var partitions = map[string][]sql.Row{}

	if numPartitions < 1 {
		numPartitions = 1
	}

	for i := 0; i < numPartitions; i++ {
		key := strconv.Itoa(i)
		keys = append(keys, []byte(key))
		partitions[key] = []sql.Row{}
	}

	var autoIncVal uint64
	autoIncIdx := -1
	for i, c := range schema.Schema {
		if c.AutoIncrement {
			autoIncVal = uint64(1)
			autoIncIdx = i
			break
		}
	}

	newSchema := make(sql.Schema, len(schema.Schema))
	for i, c := range schema.Schema {
		cCopy := c.Copy()
		if cCopy.Default != nil {
			newDef, _, _ := transform.Expr(cCopy.Default, func(e sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
				switch e := e.(type) {
				case *expression.GetField:
					// strip table names
					return expression.NewGetField(e.Index(), e.Type(), e.Name(), e.IsNullable()), transform.NewTree, nil
				default:
				}
				return e, transform.SameTree, nil
			})
			defStr := newDef.String()
			unrDef := sql.NewUnresolvedColumnDefaultValue(defStr)
			cCopy.Default = unrDef
		}
		newSchema[i] = cCopy
	}

	schema.Schema = newSchema

	// The dual table has a nil database
	dbName := ""
	if db != nil {
		dbName = db.Name()
	}
	return &Table{
		name: name,
		data: &TableData{
			dbName:        dbName,
			tableName:     name,
			schema:        schema,
			fkColl:        fkColl,
			collation:     collation,
			partitions:    partitions,
			partitionKeys: keys,
			autoIncVal:    autoIncVal,
			autoColIdx:    autoIncIdx,
		},
		db: db,
	}
}

// Name implements the sql.Table interface.
func (t Table) Name() string {
	return t.name
}

// Schema implements the sql.Table interface.
func (t *Table) Schema() sql.Schema {
	if t.projectedSchema != nil {
		return t.projectedSchema
	}
	return t.data.schema.Schema
}

// Collation implements the sql.Table interface.
func (t *Table) Collation() sql.CollationID {
	return t.data.collation
}

func (t Table) IgnoreSessionData() bool {
	return t.ignoreSessionData
}

func (t *Table) UnderlyingTable() *Table {
	return t
}

func (t *Table) GetPartition(key string) []sql.Row {
	rows, ok := t.data.partitions[key]
	if ok {
		return rows
	}

	return nil
}

// Partitions implements the sql.Table interface.
func (t *Table) Partitions(ctx *sql.Context) (sql.PartitionIter, error) {
	data := t.sessionTableData(ctx)

	var keys [][]byte
	for _, k := range data.partitionKeys {
		if rows, ok := data.partitions[string(k)]; ok && len(rows) > 0 {
			keys = append(keys, k)
		}
	}
	return &partitionIter{keys: keys}, nil
}

// rangePartitionIter returns a partition that has range and table data access
type rangePartitionIter struct {
	child  *partitionIter
	ranges sql.Expression
}

var _ sql.PartitionIter = (*rangePartitionIter)(nil)

func (i rangePartitionIter) Close(ctx *sql.Context) error {
	return i.child.Close(ctx)
}

func (i rangePartitionIter) Next(ctx *sql.Context) (sql.Partition, error) {
	part, err := i.child.Next(ctx)
	if err != nil {
		return nil, err
	}
	return &rangePartition{
		Partition: part.(*Partition),
		rang:      i.ranges,
	}, nil
}

type rangePartition struct {
	*Partition
	rang sql.Expression
}

// spatialRangePartitionIter returns a partition that has range and table data access
type spatialRangePartitionIter struct {
	child                  *partitionIter
	ord                    int
	minX, minY, maxX, maxY float64
}

var _ sql.PartitionIter = (*spatialRangePartitionIter)(nil)

func (i spatialRangePartitionIter) Close(ctx *sql.Context) error {
	return i.child.Close(ctx)
}

func (i spatialRangePartitionIter) Next(ctx *sql.Context) (sql.Partition, error) {
	part, err := i.child.Next(ctx)
	if err != nil {
		return nil, err
	}
	return &spatialRangePartition{
		Partition: part.(*Partition),
		ord:       i.ord,
		minX:      i.minX,
		minY:      i.minY,
		maxX:      i.maxX,
		maxY:      i.maxY,
	}, nil
}

type spatialRangePartition struct {
	*Partition
	ord                    int
	minX, minY, maxX, maxY float64
}

// PartitionCount implements the sql.PartitionCounter interface.
func (t *Table) PartitionCount(ctx *sql.Context) (int64, error) {
	data := t.sessionTableData(ctx)

	return int64(len(data.partitions)), nil
}

// PartitionRows implements the sql.PartitionRows interface.
func (t *Table) PartitionRows(ctx *sql.Context, partition sql.Partition) (sql.RowIter, error) {
	data := t.sessionTableData(ctx)

	filters := t.filters
	if r, ok := partition.(*rangePartition); ok && r.rang != nil {
		// index lookup is currently a single filter applied to a full table scan
		filters = append(t.filters, r.rang)
	}

	rows, ok := data.partitions[string(partition.Key())]
	if !ok {
		return nil, sql.ErrPartitionNotFound.New(partition.Key())
	}
	// The slice could be altered by other operations taking place during iteration (such as deletion or insertion), so
	// make a copy of the values as they exist when execution begins.
	rowsCopy := make([]sql.Row, len(rows))
	copy(rowsCopy, rows)

	if r, ok := partition.(*spatialRangePartition); ok {
		return &spatialTableIter{
			columns: t.columns,
			ord:     r.ord,
			minX:    r.minX,
			minY:    r.minY,
			maxX:    r.maxX,
			maxY:    r.maxY,
			rows:    rowsCopy,
		}, nil
	}

	return &tableIter{
		rows:    rowsCopy,
		columns: t.columns,
		filters: filters,
	}, nil
}

func (t *Table) DataLength(ctx *sql.Context) (uint64, error) {
	data := t.sessionTableData(ctx)

	var numBytesPerRow uint64
	for _, col := range data.schema.Schema {
		switch n := col.Type.(type) {
		case sql.NumberType:
			numBytesPerRow += 8
		case sql.StringType:
			numBytesPerRow += uint64(n.MaxByteLength())
		case types.BitType:
			numBytesPerRow += 1
		case sql.DatetimeType:
			numBytesPerRow += 8
		case sql.DecimalType:
			numBytesPerRow += uint64(n.MaximumScale())
		case sql.EnumType:
			numBytesPerRow += 2
		case types.JsonType:
			numBytesPerRow += 20
		case sql.NullType:
			numBytesPerRow += 1
		case types.TimeType:
			numBytesPerRow += 16
		case sql.YearType:
			numBytesPerRow += 8
		default:
			numBytesPerRow += 0
		}
	}

	numRows, err := data.numRows(ctx)
	if err != nil {
		return 0, err
	}

	return numBytesPerRow * numRows, nil
}

// AnalyzeTable implements the sql.StatisticsTable interface.
func (t *Table) AnalyzeTable(ctx *sql.Context) error {
	// initialize histogram map
	t.tableStats = &sql.TableStatistics{
		CreatedAt: time.Now(),
	}

	histMap, err := NewHistogramMapFromTable(ctx, t)
	if err != nil {
		return err
	}

	t.tableStats.Histograms = histMap
	for _, v := range histMap {
		t.tableStats.RowCount = v.Count + v.NullCount
		break
	}

	return nil
}

func (t *Table) RowCount(ctx *sql.Context) (uint64, error) {
	data := t.sessionTableData(ctx)
	return data.numRows(ctx)
}

func NewPartition(key []byte) *Partition {
	return &Partition{key: key}
}

type Partition struct {
	key []byte
}

func (p *Partition) Key() []byte { return p.key }

type partitionIter struct {
	keys [][]byte
	pos  int
}

func (p *partitionIter) Next(*sql.Context) (sql.Partition, error) {
	if p.pos >= len(p.keys) {
		return nil, io.EOF
	}

	key := p.keys[p.pos]
	p.pos++
	return &Partition{key}, nil
}

func (p *partitionIter) Close(*sql.Context) error { return nil }

type tableIter struct {
	columns []int
	filters []sql.Expression

	rows        []sql.Row
	indexValues sql.IndexValueIter
	pos         int
}

var _ sql.RowIter = (*tableIter)(nil)

func (i *tableIter) Next(ctx *sql.Context) (sql.Row, error) {
	row, err := i.getRow(ctx)
	if err != nil {
		return nil, err
	}

	for _, f := range i.filters {
		result, err := f.Eval(ctx, row)
		if err != nil {
			return nil, err
		}
		result, _ = types.ConvertToBool(result)
		if result != true {
			return i.Next(ctx)
		}
	}

	if i.columns != nil {
		resultRow := make(sql.Row, len(i.columns))
		for i, j := range i.columns {
			resultRow[i] = row[j]
		}
		return resultRow, nil
	}

	return row, nil
}

func (i *tableIter) colIsProjected(idx int) bool {
	for _, colIdx := range i.columns {
		if idx == colIdx {
			return true
		}
	}
	return false
}

func (i *tableIter) Close(ctx *sql.Context) error {
	if i.indexValues == nil {
		return nil
	}

	return i.indexValues.Close(ctx)
}

func (i *tableIter) getRow(ctx *sql.Context) (sql.Row, error) {
	if i.indexValues != nil {
		return i.getFromIndex(ctx)
	}

	if i.pos >= len(i.rows) {
		return nil, io.EOF
	}

	row := i.rows[i.pos]
	i.pos++
	return row, nil
}

func projectOnRow(columns []int, row sql.Row) sql.Row {
	if len(columns) < 1 {
		return row
	}

	projected := make([]interface{}, len(columns))
	for i, selected := range columns {
		projected[i] = row[selected]
	}

	return projected
}

func (i *tableIter) getFromIndex(ctx *sql.Context) (sql.Row, error) {
	data, err := i.indexValues.Next(ctx)
	if err != nil {
		return nil, err
	}

	value, err := DecodeIndexValue(data)
	if err != nil {
		return nil, err
	}

	return i.rows[value.Pos], nil
}

type spatialTableIter struct {
	columns                []int
	rows                   []sql.Row
	pos                    int
	ord                    int
	minX, minY, maxX, maxY float64
}

var _ sql.RowIter = (*spatialTableIter)(nil)

func (i *spatialTableIter) Next(ctx *sql.Context) (sql.Row, error) {
	row, err := i.getRow(ctx)
	if err != nil {
		return nil, err
	}

	if len(i.columns) == 0 {
		return row, nil
	}

	// check if bounding boxes of geometry and range intersect
	// if the range [i.minX, i.maxX] and [gMinX, gMaxX] overlap and
	// if the range [i.minY, i.maxY] and [gMinY, gMaxY] overlap
	// then, the bounding boxes intersect
	g, ok := row[i.ord].(types.GeometryValue)
	if !ok {
		return nil, fmt.Errorf("spatial index over non-geometry column")
	}
	gMinX, gMinY, gMaxX, gMaxY := g.BBox()
	xInt := (gMinX <= i.minX && i.minX <= gMaxX) ||
		(gMinX <= i.maxX && i.maxX <= gMaxX) ||
		(i.minX <= gMinX && gMinX <= i.maxX) ||
		(i.minX <= gMaxX && gMaxX <= i.maxX)
	yInt := (gMinY <= i.minY && i.minY <= gMaxY) ||
		(gMinY <= i.maxY && i.maxY <= gMaxY) ||
		(i.minY <= gMinY && gMinY <= i.maxY) ||
		(i.minY <= gMaxY && gMaxY <= i.maxY)
	if !(xInt && yInt) {
		return i.Next(ctx)
	}

	resultRow := make(sql.Row, len(i.columns))
	for i, j := range i.columns {
		resultRow[i] = row[j]
	}
	return resultRow, nil
}

func (i *spatialTableIter) Close(ctx *sql.Context) error {
	return nil
}

func (i *spatialTableIter) getRow(ctx *sql.Context) (sql.Row, error) {
	if i.pos >= len(i.rows) {
		return nil, io.EOF
	}

	row := i.rows[i.pos]
	i.pos++
	return row, nil
}

type IndexValue struct {
	Key string
	Pos int
}

func DecodeIndexValue(data []byte) (*IndexValue, error) {
	dec := gob.NewDecoder(bytes.NewReader(data))
	var value IndexValue
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}

	return &value, nil
}

func EncodeIndexValue(value *IndexValue) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func (t *Table) Inserter(ctx *sql.Context) sql.RowInserter {
	return t.getTableEditor(ctx)
}

func (t *Table) Updater(ctx *sql.Context) sql.RowUpdater {
	return t.getTableEditor(ctx)
}

func (t *Table) Replacer(ctx *sql.Context) sql.RowReplacer {
	return t.getTableEditor(ctx)
}

func (t *Table) Deleter(ctx *sql.Context) sql.RowDeleter {
	return t.getTableEditor(ctx)
}

func (t *Table) AutoIncrementSetter(ctx *sql.Context) sql.AutoIncrementSetter {
	return t.getTableEditor(ctx).(sql.AutoIncrementSetter)
}

func (t *Table) getTableEditor(ctx *sql.Context) sql.TableEditor {
	editor, err := t.newTableEditor(ctx)
	if err != nil {
		panic(err)
	}

	tableSets, err := t.getFulltextTableSets(ctx)
	if err != nil {
		panic(err)
	}

	if len(tableSets) > 0 {
		editor = t.newFulltextTableEditor(ctx, editor, tableSets)
	}

	return editor
}

func (t *Table) getRewriteTableEditor(ctx *sql.Context, oldSchema, newSchema sql.PrimaryKeySchema) sql.TableEditor {
	editor, err := t.tableEditorForRewrite(ctx, oldSchema, newSchema)
	if err != nil {
		panic(err)
	}

	tableUnderEdit := editor.(*tableEditor).editedTable
	err = tableUnderEdit.modifyFulltextIndexesForRewrite(ctx, tableUnderEdit.data, oldSchema)
	if err != nil {
		panic(err)
	}

	tableSets, err := fulltextTableSets(ctx, tableUnderEdit.data, tableUnderEdit.db)
	if err != nil {
		panic(err)
	}

	if len(tableSets) > 0 {
		_, insertCols, err := fulltext.GetKeyColumns(ctx, tableUnderEdit)
		if err != nil {
			panic(err)
		}

		// table editors used for rewrite need to truncate the fulltext tables as well as the primary table (which happens
		// in the RewriteInserter method for all tables)
		newTableSets := make([]fulltext.TableSet, len(tableSets))
		for i := range tableSets {
			ts := *(&tableSets[i])

			positionSch, err := fulltext.NewSchema(fulltext.SchemaPosition, insertCols, ts.Position.Name(), tableUnderEdit.Collation())
			if err != nil {
				panic(err)
			}

			docCountSch, err := fulltext.NewSchema(fulltext.SchemaDocCount, insertCols, ts.DocCount.Name(), tableUnderEdit.Collation())
			if err != nil {
				panic(err)
			}

			globalCountSch, err := fulltext.NewSchema(fulltext.SchemaGlobalCount, nil, ts.GlobalCount.Name(), tableUnderEdit.Collation())
			if err != nil {
				panic(err)
			}

			rowCountSch, err := fulltext.NewSchema(fulltext.SchemaRowCount, nil, ts.RowCount.Name(), tableUnderEdit.Collation())
			if err != nil {
				panic(err)
			}

			ts.RowCount.(*Table).data = ts.RowCount.(*Table).data.copy().truncate(sql.NewPrimaryKeySchema(rowCountSch))
			ts.DocCount.(*Table).data = ts.DocCount.(*Table).data.copy().truncate(sql.NewPrimaryKeySchema(docCountSch))
			ts.GlobalCount.(*Table).data = ts.GlobalCount.(*Table).data.copy().truncate(sql.NewPrimaryKeySchema(globalCountSch))
			ts.Position.(*Table).data = ts.Position.(*Table).data.copy().truncate(sql.NewPrimaryKeySchema(positionSch))
			newTableSets[i] = ts

			// When we get a rowcount editor below, we are going to use the session data for each of these tables. Since we
			// are rewriting them anyway, update their session data with the new empty data and new schema
			sess := SessionFromContext(ctx)
			sess.putTable(ts.RowCount.(*Table).data)
			sess.putTable(ts.DocCount.(*Table).data)
			sess.putTable(ts.GlobalCount.(*Table).data)
			sess.putTable(ts.Position.(*Table).data)
		}

		editor = tableUnderEdit.newFulltextTableEditor(ctx, editor, newTableSets)
	}

	return editor
}

func (t *Table) newTableEditor(ctx *sql.Context) (sql.TableEditor, error) {
	var ea tableEditAccumulator
	var data *TableData
	if t.ignoreSessionData {
		ea = newTableEditAccumulator(t.data)
		data = t.data
	} else {
		sess := SessionFromContext(ctx)
		ea = sess.editAccumulator(t)
		data = sess.tableData(t)
	}

	tableUnderEdit := t.copy()
	tableUnderEdit.data = data

	uniqIdxCols, prefixLengths := t.data.indexColsForTableEditor()
	var editor sql.TableEditor = &tableEditor{
		editedTable:   tableUnderEdit,
		initialTable:  t.copy(),
		ea:            ea,
		uniqueIdxCols: uniqIdxCols,
		prefixLengths: prefixLengths,
	}
	return editor, nil
}

func (t *Table) tableEditorForRewrite(ctx *sql.Context, oldSchema, newSchema sql.PrimaryKeySchema) (sql.TableEditor, error) {
	// Make a copy of the table under edit with the new schema and no data
	// sess := SessionFromContext(ctx)
	tableUnderEdit := t.copy()
	// tableUnderEdit.data = sess.tableData(t).copy()
	tableData := tableUnderEdit.data.truncate(normalizeSchemaForRewrite(newSchema))
	tableUnderEdit.data = tableData

	uniqIdxCols, prefixLengths := tableData.indexColsForTableEditor()
	var editor sql.TableEditor = &tableEditor{
		editedTable:   tableUnderEdit,
		initialTable:  t.copy(),
		ea:            newTableEditAccumulator(tableData),
		uniqueIdxCols: uniqIdxCols,
		prefixLengths: prefixLengths,
	}
	return editor, nil
}

func (t *Table) newFulltextTableEditor(ctx *sql.Context, parentEditor sql.TableEditor, tableSets []fulltext.TableSet) sql.TableEditor {
	configTbl, ok, err := t.db.GetTableInsensitive(ctx, t.data.fullTextConfigTableName)
	if err != nil {
		panic(err)
	}
	if !ok { // This should never happen
		panic(fmt.Sprintf("table `%s` declares the table `%s` as a FULLTEXT config table, but it could not be found", t.name, configTbl))
	}
	ftEditor, err := fulltext.CreateEditor(ctx, t, configTbl.(fulltext.EditableTable), tableSets...)
	if err != nil {
		panic(err)
	}
	parentEditor, err = fulltext.CreateMultiTableEditor(ctx, parentEditor, ftEditor)
	if err != nil {
		panic(err)
	}
	return parentEditor
}

func (t *Table) getFulltextTableSets(ctx *sql.Context) ([]fulltext.TableSet, error) {
	data := t.sessionTableData(ctx)
	db := t.db

	return fulltextTableSets(ctx, data, db)
}

func fulltextTableSets(ctx *sql.Context, data *TableData, db *BaseDatabase) ([]fulltext.TableSet, error) {
	var tableSets []fulltext.TableSet
	for _, idx := range data.indexes {
		if !idx.IsFullText() {
			continue
		}
		if db == nil { // Rewrite your test if you run into this
			panic("database is nil, which can only happen when adding a table outside of the SQL path, such as during harness creation")
		}
		ftIdx, ok := idx.(fulltext.Index)
		if !ok { // This should never happen
			panic("index returns true for FULLTEXT, but does not implement interface")
		}
		ftTableNames, err := ftIdx.FullTextTableNames(ctx)
		if err != nil { // This should never happen
			panic(err.Error())
		}

		positionTbl, ok, err := db.GetTableInsensitive(ctx, ftTableNames.Position)
		if err != nil {
			panic(err)
		}
		if !ok { // This should never happen
			panic(fmt.Sprintf("index `%s` declares the table `%s` as a FULLTEXT position table, but it could not be found", idx.ID(), ftTableNames.Position))
		}
		docCountTbl, ok, err := db.GetTableInsensitive(ctx, ftTableNames.DocCount)
		if err != nil {
			panic(err)
		}
		if !ok { // This should never happen
			panic(fmt.Sprintf("index `%s` declares the table `%s` as a FULLTEXT doc count table, but it could not be found", idx.ID(), ftTableNames.DocCount))
		}
		globalCountTbl, ok, err := db.GetTableInsensitive(ctx, ftTableNames.GlobalCount)
		if err != nil {
			panic(err)
		}
		if !ok { // This should never happen
			panic(fmt.Sprintf("index `%s` declares the table `%s` as a FULLTEXT global count table, but it could not be found", idx.ID(), ftTableNames.GlobalCount))
		}
		rowCountTbl, ok, err := db.GetTableInsensitive(ctx, ftTableNames.RowCount)
		if err != nil {
			panic(err)
		}
		if !ok { // This should never happen
			panic(fmt.Sprintf("index `%s` declares the table `%s` as a FULLTEXT row count table, but it could not be found", idx.ID(), ftTableNames.RowCount))
		}

		tableSets = append(tableSets, fulltext.TableSet{
			Index:       ftIdx,
			Position:    positionTbl.(fulltext.EditableTable),
			DocCount:    docCountTbl.(fulltext.EditableTable),
			GlobalCount: globalCountTbl.(fulltext.EditableTable),
			RowCount:    rowCountTbl.(fulltext.EditableTable),
		})
	}

	return tableSets, nil
}

func (t *Table) Truncate(ctx *sql.Context) (int, error) {
	data := t.sessionTableData(ctx)

	count := 0
	for key := range data.partitions {
		count += len(data.partitions[key])
	}

	data.truncate(data.schema)
	return count, nil
}

// Insert is a convenience method to avoid having to create an inserter in test setup
func (t *Table) Insert(ctx *sql.Context, row sql.Row) error {
	inserter := t.Inserter(ctx)
	if err := inserter.Insert(ctx, row); err != nil {
		return err
	}
	return inserter.Close(ctx)
}

// PeekNextAutoIncrementValue peeks at the next AUTO_INCREMENT value
func (t *Table) PeekNextAutoIncrementValue(ctx *sql.Context) (uint64, error) {
	data := t.sessionTableData(ctx)

	return data.autoIncVal, nil
}

// GetNextAutoIncrementValue gets the next auto increment value for the memory table the increment.
func (t *Table) GetNextAutoIncrementValue(ctx *sql.Context, insertVal interface{}) (uint64, error) {
	data := t.sessionTableData(ctx)

	cmp, err := types.Uint64.Compare(insertVal, data.autoIncVal)
	if err != nil {
		return 0, err
	}

	if cmp > 0 && insertVal != nil {
		v, _, err := types.Uint64.Convert(insertVal)
		if err != nil {
			return 0, err
		}
		data.autoIncVal = v.(uint64)
	}

	return data.autoIncVal, nil
}

func (t *Table) AddColumn(ctx *sql.Context, column *sql.Column, order *sql.ColumnOrder) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	newColIdx, data := addColumnToSchema(ctx, data, column, order)

	err := insertValueInRows(ctx, data, newColIdx, column.Default)
	if err != nil {
		return err
	}

	sess.putTable(data)
	return nil
}

// addColumnToSchema adds the given column to the schema and returns the new index
func addColumnToSchema(ctx *sql.Context, data *TableData, newCol *sql.Column, order *sql.ColumnOrder) (int, *TableData) {
	// TODO: might have wrong case
	newCol.Source = data.tableName
	newSch := make(sql.Schema, len(data.schema.Schema)+1)

	// TODO: need to fix this in the engine itself
	if newCol.PrimaryKey {
		newCol.Nullable = false
	}

	newColIdx := 0
	var i int
	if order != nil && order.First {
		newSch[i] = newCol
		i++
	}

	for _, col := range data.schema.Schema {
		newSch[i] = col
		i++
		if (order != nil && order.AfterColumn == col.Name) || (order == nil && i == len(data.schema.Schema)) {
			newSch[i] = newCol
			newColIdx = i
			i++
		}
	}

	for _, newSchCol := range newSch {
		newDefault, _, _ := transform.Expr(newSchCol.Default, func(expr sql.Expression) (sql.Expression, transform.TreeIdentity, error) {
			if expr, ok := expr.(*expression.GetField); ok {
				return expr.WithIndex(newSch.IndexOf(expr.Name(), data.tableName)), transform.NewTree, nil
			}
			return expr, transform.SameTree, nil
		})
		newSchCol.Default = newDefault.(*sql.ColumnDefaultValue)
	}

	if newCol.AutoIncrement {
		data.autoColIdx = newColIdx
		data.autoIncVal = 0

		if newColIdx < len(data.schema.Schema) {
			for _, p := range data.partitions {
				for _, row := range p {
					if row[newColIdx] == nil {
						continue
					}

					cmp, err := newCol.Type.Compare(row[newColIdx], data.autoIncVal)
					if err != nil {
						panic(err)
					}

					if cmp > 0 {
						var val interface{}
						val, _, err = types.Uint64.Convert(row[newColIdx])
						if err != nil {
							panic(err)
						}
						data.autoIncVal = val.(uint64)
					}
				}
			}
		} else {
			data.autoIncVal = 0
		}

		data.autoIncVal++
	}

	newPkOrds := data.schema.PkOrdinals
	for i := 0; i < len(newPkOrds); i++ {
		// added column shifts the index of every column after
		// all ordinals above addIdx will be bumped
		if newColIdx <= newPkOrds[i] {
			newPkOrds[i]++
		}
	}

	data.schema = sql.NewPrimaryKeySchema(newSch, newPkOrds...)

	return newColIdx, data
}

func (t *Table) DropColumn(ctx *sql.Context, columnName string) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	droppedCol, data := dropColumnFromSchema(ctx, data, columnName)
	for k, p := range data.partitions {
		newP := make([]sql.Row, len(p))
		for i, row := range p {
			var newRow sql.Row
			newRow = append(newRow, row[:droppedCol]...)
			newRow = append(newRow, row[droppedCol+1:]...)
			newP[i] = newRow
		}
		data.partitions[k] = newP
	}

	sess.putTable(data)

	return nil
}

// dropColumnFromSchema drops the given column name from the schema and returns its old index.
func dropColumnFromSchema(ctx *sql.Context, data *TableData, columnName string) (int, *TableData) {
	newSch := make(sql.Schema, len(data.schema.Schema)-1)
	var i int
	droppedCol := -1
	for _, col := range data.schema.Schema {
		if col.Name != columnName {
			newSch[i] = col
			i++
		} else {
			droppedCol = i
		}
	}

	newPkOrds := data.schema.PkOrdinals
	for i := 0; i < len(newPkOrds); i++ {
		// deleting a column will shift subsequent column indices left
		// PK ordinals after dropIdx bumped down
		if droppedCol <= newPkOrds[i] {
			newPkOrds[i]--
		}
	}

	data.schema = sql.NewPrimaryKeySchema(newSch, newPkOrds...)
	return droppedCol, data
}

func (t *Table) ModifyColumn(ctx *sql.Context, columnName string, column *sql.Column, order *sql.ColumnOrder) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	oldIdx := -1
	newIdx := 0
	for i, col := range data.schema.Schema {
		if col.Name == columnName {
			oldIdx = i
			column.PrimaryKey = col.PrimaryKey
			if column.PrimaryKey {
				column.Nullable = false
			}
			// We've removed auto increment through this modification so we need to do some bookkeeping
			if col.AutoIncrement && !column.AutoIncrement {
				data.autoColIdx = -1
				data.autoIncVal = 0
			}
			break
		}
	}

	if order == nil {
		newIdx = oldIdx
		if newIdx == 0 {
			order = &sql.ColumnOrder{First: true}
		} else {
			order = &sql.ColumnOrder{AfterColumn: data.schema.Schema[newIdx-1].Name}
		}
	} else if !order.First {
		var oldSchemaWithoutCol sql.Schema
		oldSchemaWithoutCol = append(oldSchemaWithoutCol, data.schema.Schema[:oldIdx]...)
		oldSchemaWithoutCol = append(oldSchemaWithoutCol, data.schema.Schema[oldIdx+1:]...)
		for i, col := range oldSchemaWithoutCol {
			if col.Name == order.AfterColumn {
				newIdx = i + 1
				break
			}
		}
	}

	for k, p := range data.partitions {
		newP := make([]sql.Row, len(p))
		for i, row := range p {
			var oldRowWithoutVal sql.Row
			oldRowWithoutVal = append(oldRowWithoutVal, row[:oldIdx]...)
			oldRowWithoutVal = append(oldRowWithoutVal, row[oldIdx+1:]...)
			newVal, inRange, err := column.Type.Convert(row[oldIdx])
			if err != nil {
				if sql.ErrNotMatchingSRID.Is(err) {
					err = sql.ErrNotMatchingSRIDWithColName.New(columnName, err)
				}
				return err
			}
			if !inRange {
				return sql.ErrValueOutOfRange.New(row[oldIdx], column.Type)
			}
			var newRow sql.Row
			newRow = append(newRow, oldRowWithoutVal[:newIdx]...)
			newRow = append(newRow, newVal)
			newRow = append(newRow, oldRowWithoutVal[newIdx:]...)
			newP[i] = newRow
		}
		data.partitions[k] = newP
	}

	pkNameToOrdIdx := make(map[string]int)
	for i, ord := range data.schema.PkOrdinals {
		pkNameToOrdIdx[data.schema.Schema[ord].Name] = i
	}

	_, data = dropColumnFromSchema(ctx, data, columnName)
	_, data = addColumnToSchema(ctx, data, column, order)

	newPkOrds := make([]int, len(data.schema.PkOrdinals))
	for ord, col := range data.schema.Schema {
		if col.PrimaryKey {
			i := pkNameToOrdIdx[col.Name]
			newPkOrds[i] = ord
		}
	}

	data.schema.PkOrdinals = newPkOrds

	for _, index := range data.indexes {
		memIndex := index.(*Index)
		nameLowercase := strings.ToLower(columnName)
		for i, expr := range memIndex.Exprs {
			getField := expr.(*expression.GetField)
			if strings.ToLower(getField.Name()) == nameLowercase {
				memIndex.Exprs[i] = expression.NewGetFieldWithTable(newIdx, column.Type, getField.Table(), column.Name, column.Nullable)
			}
		}
	}

	sess.putTable(data)

	return nil
}

// PrimaryKeySchema implements sql.PrimaryKeyAlterableTable
func (t *Table) PrimaryKeySchema() sql.PrimaryKeySchema {
	return t.data.schema
}

// String implements the sql.Table interface.
func (t *Table) String() string {
	return t.name
}

var debugDataPrint = false

func (t *Table) DebugString() string {
	if debugDataPrint {
		p := t.data.partitions["0"]
		s := ""
		for i, row := range p {
			if i > 0 {
				s += ", "
			}
			s += fmt.Sprintf("%v", row)
		}
		return s
	}

	p := sql.NewTreePrinter()

	children := []string{fmt.Sprintf("name: %s", t.name)}
	if t.lookup != nil {
		children = append(children, fmt.Sprintf("index: %s", t.lookup))
	}

	if len(t.columns) > 0 {
		var projections []string
		for _, column := range t.columns {
			projections = append(projections, fmt.Sprintf("%d", column))
		}
		children = append(children, fmt.Sprintf("projections: %s", projections))

	}

	if len(t.filters) > 0 {
		var filters []string
		for _, filter := range t.filters {
			filters = append(filters, fmt.Sprintf("%s", sql.DebugString(filter)))
		}
		children = append(children, fmt.Sprintf("filters: %s", filters))
	}
	_ = p.WriteNode("Table")
	p.WriteChildren(children...)
	return p.String()
}

// HandledFilters implements the sql.FilteredTable interface.
func (t *Table) HandledFilters(filters []sql.Expression) []sql.Expression {
	var handled []sql.Expression
	for _, f := range filters {
		var hasOtherFields bool
		sql.Inspect(f, func(e sql.Expression) bool {
			if e, ok := e.(*expression.GetField); ok {
				if e.Table() != t.name || !t.data.schema.Contains(e.Name(), t.name) {
					hasOtherFields = true
					return false
				}
			}
			return true
		})

		if !hasOtherFields {
			handled = append(handled, f)
		}
	}

	return handled
}

// FilteredTable functionality in the Table type was disabled for a long period of time, and has developed major
// issues with the current analyzer logic. It's only used in the pushdown unit tests, and sql.FilteredTable should be
// considered unstable until this situation is fixed.
type FilteredTable struct {
	*Table
}

var _ sql.FilteredTable = (*FilteredTable)(nil)

func NewFilteredTable(db MemoryDatabase, name string, schema sql.PrimaryKeySchema, fkColl *ForeignKeyCollection) *FilteredTable {
	return &FilteredTable{
		Table: NewTable(db, name, schema, fkColl),
	}
}

// WithFilters implements the sql.FilteredTable interface.
func (t *FilteredTable) WithFilters(ctx *sql.Context, filters []sql.Expression) sql.Table {
	if len(filters) == 0 {
		return t
	}

	nt := *t
	nt.filters = filters
	return &nt
}

// WithProjections implements sql.ProjectedTable
func (t *FilteredTable) WithProjections(schema []string) sql.Table {
	table := t.Table.WithProjections(schema)

	nt := *t
	nt.Table = table.(*Table)
	return &nt
}

// Projections implements sql.ProjectedTable
func (t *FilteredTable) Projections() []string {
	return t.projection
}

// IndexedTable is a table that expects to return one or more partitions
// for range lookups.
type IndexedTable struct {
	*Table
	Lookup sql.IndexLookup
}

func (t *IndexedTable) LookupPartitions(ctx *sql.Context, lookup sql.IndexLookup) (sql.PartitionIter, error) {
	filter, err := lookup.Index.(*Index).rangeFilterExpr(ctx, lookup.Ranges...)
	if err != nil {
		return nil, err
	}
	child, err := t.Table.Partitions(ctx)
	if err != nil {
		return nil, err
	}

	if lookup.Index.IsSpatial() {
		lower := sql.GetRangeCutKey(lookup.Ranges[0][0].LowerBound)
		upper := sql.GetRangeCutKey(lookup.Ranges[0][0].UpperBound)
		minPoint, ok := lower.(types.Point)
		if !ok {
			return nil, sql.ErrInvalidGISData.New()
		}
		maxPoint, ok := upper.(types.Point)
		if !ok {
			return nil, sql.ErrInvalidGISData.New()
		}

		ord := lookup.Index.(*Index).Exprs[0].(*expression.GetField).Index()
		return spatialRangePartitionIter{
			child: child.(*partitionIter),
			ord:   ord,
			minX:  minPoint.X,
			minY:  minPoint.Y,
			maxX:  maxPoint.X,
			maxY:  maxPoint.Y,
		}, nil
	}

	return rangePartitionIter{child: child.(*partitionIter), ranges: filter}, nil
}

// PartitionRows implements the sql.PartitionRows interface.
func (t *IndexedTable) PartitionRows(ctx *sql.Context, partition sql.Partition) (sql.RowIter, error) {
	iter, err := t.Table.PartitionRows(ctx, partition)
	if err != nil {
		return nil, err
	}
	if t.Lookup.Index != nil {
		idx := t.Lookup.Index.(*Index)
		sf := make(sql.SortFields, len(idx.Exprs))
		for i, e := range idx.Exprs {
			sf[i] = sql.SortField{Column: e}
			if t.Lookup.IsReverse {
				sf[i].Order = sql.Descending
				// TODO: null ordering?
			}
		}
		var sorter *expression.Sorter
		if i, ok := iter.(*tableIter); ok {
			sorter = &expression.Sorter{
				SortFields: sf,
				Rows:       i.rows,
				LastError:  nil,
				Ctx:        ctx,
			}
		} else if i, ok := iter.(*spatialTableIter); ok {
			sorter = &expression.Sorter{
				SortFields: sf,
				Rows:       i.rows,
				LastError:  nil,
				Ctx:        ctx,
			}
		}

		sort.Stable(sorter)
	}

	return iter, nil
}

func (t *Table) IndexedAccess(lookup sql.IndexLookup) sql.IndexedTable {
	return &IndexedTable{Table: t, Lookup: lookup}
}

// WithProjections implements sql.ProjectedTable
func (t *Table) WithProjections(cols []string) sql.Table {
	nt := *t
	if cols == nil {
		nt.projectedSchema = nil
		nt.projection = nil
		nt.columns = nil
		return &nt
	}
	columns, err := nt.data.columnIndexes(cols)
	if err != nil {
		panic(err)
	}

	nt.columns = columns

	projectedSchema := make(sql.Schema, len(columns))
	for i, j := range columns {
		projectedSchema[i] = nt.data.schema.Schema[j]
	}
	nt.projectedSchema = projectedSchema
	nt.projection = cols

	return &nt
}

// Projections implements sql.ProjectedTable
func (t *Table) Projections() []string {
	return t.projection
}

// EnablePrimaryKeyIndexes enables the use of primary key indexes on this table.
func (t *Table) EnablePrimaryKeyIndexes() {
	t.pkIndexesEnabled = true
	t.data.primaryKeyIndexes = true
}

// GetIndexes implements sql.IndexedTable
func (t *Table) GetIndexes(ctx *sql.Context) ([]sql.Index, error) {
	data := t.sessionTableData(ctx)

	indexes := make([]sql.Index, 0)

	if data.primaryKeyIndexes {
		if len(data.schema.PkOrdinals) > 0 {
			exprs := make([]sql.Expression, len(data.schema.PkOrdinals))
			for i, ord := range data.schema.PkOrdinals {
				column := data.schema.Schema[ord]
				idx, field := data.getColumnOrdinal(column.Name)
				exprs[i] = expression.NewGetFieldWithTable(idx, field.Type, t.name, field.Name, field.Nullable)
			}
			indexes = append(indexes, &Index{
				DB:         "",
				DriverName: "",
				Tbl:        t,
				TableName:  t.name,
				Exprs:      exprs,
				Name:       "PRIMARY",
				Unique:     true,
			})
		}
	}

	nonPrimaryIndexes := make([]sql.Index, len(data.indexes))
	var i int
	for _, index := range data.indexes {
		nonPrimaryIndexes[i] = index
		i++
	}
	sort.Slice(nonPrimaryIndexes, func(i, j int) bool {
		return nonPrimaryIndexes[i].ID() < nonPrimaryIndexes[j].ID()
	})

	return append(indexes, nonPrimaryIndexes...), nil
}

// GetDeclaredForeignKeys implements the interface sql.ForeignKeyTable.
func (t *Table) GetDeclaredForeignKeys(ctx *sql.Context) ([]sql.ForeignKeyConstraint, error) {
	data := t.sessionTableData(ctx)

	//TODO: may not be the best location, need to handle db as well
	var fks []sql.ForeignKeyConstraint
	lowerName := strings.ToLower(t.name)
	for _, fk := range data.fkColl.Keys() {
		if strings.ToLower(fk.Table) == lowerName {
			fks = append(fks, fk)
		}
	}
	return fks, nil
}

// GetReferencedForeignKeys implements the interface sql.ForeignKeyTable.
func (t *Table) GetReferencedForeignKeys(ctx *sql.Context) ([]sql.ForeignKeyConstraint, error) {
	data := t.sessionTableData(ctx)

	// TODO: may not be the best location, need to handle db as well
	var fks []sql.ForeignKeyConstraint
	lowerName := strings.ToLower(t.name)
	for _, fk := range data.fkColl.Keys() {
		if strings.ToLower(fk.ParentTable) == lowerName {
			fks = append(fks, fk)
		}
	}
	return fks, nil
}

// AddForeignKey implements sql.ForeignKeyTable. Foreign partitionKeys are not enforced on update / delete.
func (t *Table) AddForeignKey(ctx *sql.Context, fk sql.ForeignKeyConstraint) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	lowerName := strings.ToLower(fk.Name)
	for _, key := range data.fkColl.Keys() {
		if strings.ToLower(key.Name) == lowerName {
			return fmt.Errorf("Constraint %s already exists", fk.Name)
		}
	}
	data.fkColl.AddFK(fk)

	return nil
}

// DropForeignKey implements sql.ForeignKeyTable.
func (t *Table) DropForeignKey(ctx *sql.Context, fkName string) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	if data.fkColl.DropFK(fkName) {
		return nil
	}

	return sql.ErrForeignKeyNotFound.New(fkName, t.name)
}

// UpdateForeignKey implements sql.ForeignKeyTable.
func (t *Table) UpdateForeignKey(ctx *sql.Context, fkName string, fk sql.ForeignKeyConstraint) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	data.fkColl.DropFK(fkName)
	lowerName := strings.ToLower(fk.Name)
	for _, key := range data.fkColl.Keys() {
		if strings.ToLower(key.Name) == lowerName {
			return fmt.Errorf("Constraint %s already exists", fk.Name)
		}
	}
	data.fkColl.AddFK(fk)

	return nil
}

// CreateIndexForForeignKey implements sql.ForeignKeyTable.
func (t *Table) CreateIndexForForeignKey(ctx *sql.Context, idx sql.IndexDef) error {
	return t.CreateIndex(ctx, idx)
}

// SetForeignKeyResolved implements sql.ForeignKeyTable.
func (t *Table) SetForeignKeyResolved(ctx *sql.Context, fkName string) error {
	data := t.sessionTableData(ctx)

	if !data.fkColl.SetResolved(fkName) {
		return sql.ErrForeignKeyNotFound.New(fkName, t.name)
	}
	return nil
}

// GetForeignKeyEditor implements sql.ForeignKeyTable.
func (t *Table) GetForeignKeyEditor(ctx *sql.Context) sql.ForeignKeyEditor {
	return t.getTableEditor(ctx).(sql.ForeignKeyEditor)
}

// GetChecks implements sql.CheckTable
func (t *Table) GetChecks(ctx *sql.Context) ([]sql.CheckDefinition, error) {
	data := t.sessionTableData(ctx)
	return data.checks, nil
}

func (t *Table) sessionTableData(ctx *sql.Context) *TableData {
	if t.ignoreSessionData {
		return t.data
	}
	sess := SessionFromContext(ctx)
	return sess.tableData(t)
}

// CreateCheck implements sql.CheckAlterableTable
func (t *Table) CreateCheck(ctx *sql.Context, check *sql.CheckDefinition) error {
	data := t.sessionTableData(ctx)

	toInsert := *check
	if toInsert.Name == "" {
		toInsert.Name = data.generateCheckName()
	}

	for _, key := range data.checks {
		if key.Name == toInsert.Name {
			return fmt.Errorf("constraint %s already exists", toInsert.Name)
		}
	}

	data.checks = append(data.checks, toInsert)
	return nil
}

// DropCheck implements sql.CheckAlterableTable.
func (t *Table) DropCheck(ctx *sql.Context, chName string) error {
	data := t.sessionTableData(ctx)

	lowerName := strings.ToLower(chName)
	for i, key := range data.checks {
		if strings.ToLower(key.Name) == lowerName {
			data.checks = append(data.checks[:i], data.checks[i+1:]...)
			return nil
		}
	}
	//TODO: add SQL error
	return fmt.Errorf("check '%s' was not found on the table", chName)
}

func (t *Table) createIndex(data *TableData, name string, columns []sql.IndexColumn, constraint sql.IndexConstraint, comment string) (sql.Index, error) {
	if name == "" {
		for _, column := range columns {
			name += column.Name + "_"
		}
	}
	if data.indexes[name] != nil {
		// TODO: extract a standard error type for this
		return nil, fmt.Errorf("Error: index already exists")
	}

	exprs := make([]sql.Expression, len(columns))
	colNames := make([]string, len(columns))
	for i, column := range columns {
		idx, field := data.getColumnOrdinal(column.Name)
		exprs[i] = expression.NewGetFieldWithTable(idx, field.Type, t.name, field.Name, field.Nullable)
		colNames[i] = column.Name
	}

	var hasNonZeroLengthColumn bool
	for _, column := range columns {
		if column.Length > 0 {
			hasNonZeroLengthColumn = true
			break
		}
	}
	var prefixLengths []uint16
	if hasNonZeroLengthColumn {
		prefixLengths = make([]uint16, len(columns))
		for i, column := range columns {
			prefixLengths[i] = uint16(column.Length)
		}
	}

	if constraint == sql.IndexConstraint_Unique {
		err := data.errIfDuplicateEntryExist(colNames, name)
		if err != nil {
			return nil, err
		}
	}

	return &Index{
		DB:         "",
		DriverName: "",
		Tbl:        t,
		TableName:  t.name,
		Exprs:      exprs,
		Name:       name,
		Unique:     constraint == sql.IndexConstraint_Unique,
		Spatial:    constraint == sql.IndexConstraint_Spatial,
		Fulltext:   constraint == sql.IndexConstraint_Fulltext,
		CommentStr: comment,
		PrefixLens: prefixLengths,
	}, nil
}

// CreateIndex implements sql.IndexAlterableTable
func (t *Table) CreateIndex(ctx *sql.Context, idx sql.IndexDef) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	if data.indexes == nil {
		data.indexes = make(map[string]sql.Index)
	}

	index, err := t.createIndex(data, idx.Name, idx.Columns, idx.Constraint, idx.Comment)
	if err != nil {
		return err
	}

	// Store the computed index name in the case of an empty index name being passed in
	data.indexes[index.ID()] = index
	sess.putTable(data)

	return nil
}

// DropIndex implements sql.IndexAlterableTable
func (t *Table) DropIndex(ctx *sql.Context, indexName string) error {
	data := t.sessionTableData(ctx)

	for name := range data.indexes {
		if strings.ToLower(name) == strings.ToLower(indexName) {
			delete(data.indexes, name)
			return nil
		}
	}

	return sql.ErrIndexNotFound.New(indexName)
}

// RenameIndex implements sql.IndexAlterableTable
func (t *Table) RenameIndex(ctx *sql.Context, fromIndexName string, toIndexName string) error {
	data := t.sessionTableData(ctx)

	if fromIndexName == toIndexName {
		return nil
	}
	if idx, ok := data.indexes[fromIndexName]; ok {
		delete(data.indexes, fromIndexName)
		data.indexes[toIndexName] = idx
		idx.(*Index).Name = toIndexName
	}
	return nil
}

// CreateFulltextIndex implements fulltext.IndexAlterableTable
func (t *Table) CreateFulltextIndex(ctx *sql.Context, indexDef sql.IndexDef, keyCols fulltext.KeyColumns, tableNames fulltext.IndexTableNames) error {
	sess := SessionFromContext(ctx)
	data := sess.tableData(t)

	if len(data.fullTextConfigTableName) > 0 {
		if data.fullTextConfigTableName != tableNames.Config {
			return fmt.Errorf("Full-Text config table name has been changed from `%s` to `%s`", data.fullTextConfigTableName, tableNames.Config)
		}
	} else {
		data.fullTextConfigTableName = tableNames.Config
	}

	if data.indexes == nil {
		data.indexes = make(map[string]sql.Index)
	}

	index, err := t.createIndex(data, indexDef.Name, indexDef.Columns, indexDef.Constraint, indexDef.Comment)
	if err != nil {
		return err
	}
	index.(*Index).fulltextInfo = fulltextInfo{
		PositionTableName:    tableNames.Position,
		DocCountTableName:    tableNames.DocCount,
		GlobalCountTableName: tableNames.GlobalCount,
		RowCountTableName:    tableNames.RowCount,
		KeyColumns:           keyCols,
	}

	data.indexes[index.ID()] = index // We should store the computed index name in the case of an empty index name being passed in
	sess.putTable(data)

	return nil
}

// ModifyStoredCollation implements sql.CollationAlterableTable
func (t *Table) ModifyStoredCollation(ctx *sql.Context, collation sql.CollationID) error {
	return fmt.Errorf("converting the collations of columns is not yet supported")
}

// ModifyDefaultCollation implements sql.CollationAlterableTable
func (t *Table) ModifyDefaultCollation(ctx *sql.Context, collation sql.CollationID) error {
	data := t.sessionTableData(ctx)

	data.collation = collation
	return nil
}

// WithDriverIndexLookup implements the sql.IndexAddressableTable interface.
func (t *Table) WithDriverIndexLookup(lookup sql.DriverIndexLookup) sql.Table {
	if t.lookup != nil {
		return t
	}

	nt := *t
	nt.lookup = lookup

	return &nt
}

// IndexKeyValues implements the sql.IndexableTable interface.
func (t *Table) IndexKeyValues(
	ctx *sql.Context,
	colNames []string,
) (sql.PartitionIndexKeyValueIter, error) {
	iter, err := t.Partitions(ctx)
	if err != nil {
		return nil, err
	}

	columns, err := t.data.columnIndexes(colNames)
	if err != nil {
		return nil, err
	}

	return &partitionIndexKeyValueIter{
		table:   t,
		iter:    iter,
		columns: columns,
	}, nil
}

// Filters implements the sql.FilteredTable interface.
func (t *Table) Filters() []sql.Expression {
	return t.filters
}

// CreatePrimaryKey implements the PrimaryKeyAlterableTable
func (t *Table) CreatePrimaryKey(ctx *sql.Context, columns []sql.IndexColumn) error {
	data := t.sessionTableData(ctx)

	// TODO: create alternate table implementation that doesn't implement rewriter to test this
	// First check that a primary key already exists
	for _, col := range data.schema.Schema {
		if col.PrimaryKey {
			return sql.ErrMultiplePrimaryKeysDefined.New()
		}
	}

	potentialSchema := data.schema.Schema.Copy()

	pkOrdinals := make([]int, len(columns))
	for i, newCol := range columns {
		found := false
		for j, currCol := range potentialSchema {
			if strings.ToLower(currCol.Name) == strings.ToLower(newCol.Name) {
				if types.IsText(currCol.Type) && newCol.Length > 0 {
					return sql.ErrUnsupportedIndexPrefix.New(currCol.Name)
				}
				currCol.PrimaryKey = true
				currCol.Nullable = false
				found = true
				pkOrdinals[i] = j
				break
			}
		}

		if !found {
			return sql.ErrKeyColumnDoesNotExist.New(newCol.Name)
		}
	}

	// TODO: fix
	// pkSchema := sql.NewPrimaryKeySchema(potentialSchema, pkOrdinals...)
	// newTable, err := newTable(ctx, t, pkSchema)
	// if err != nil {
	// 	return err
	// }
	//
	// t.data.schema = pkSchema
	// t.data.partitions = newTable.data.partitions
	// t.partitionKeys = newTable.partitionKeys

	return nil
}

type partidx struct {
	key string
	i   int
}

type partitionssort struct {
	ps   map[string][]sql.Row
	idx  []partidx
	less func(l, r sql.Row) bool
}

func (ps partitionssort) Len() int {
	return len(ps.idx)
}

func (ps partitionssort) Less(i, j int) bool {
	lidx := ps.idx[i]
	ridx := ps.idx[j]
	lr := ps.ps[lidx.key][lidx.i]
	rr := ps.ps[ridx.key][ridx.i]
	return ps.less(lr, rr)
}

func (ps partitionssort) Swap(i, j int) {
	lidx := ps.idx[i]
	ridx := ps.idx[j]
	ps.ps[lidx.key][lidx.i], ps.ps[ridx.key][ridx.i] = ps.ps[ridx.key][ridx.i], ps.ps[lidx.key][lidx.i]
}

func (t Table) copy() *Table {
	t.data = t.data.copy()

	if t.projection != nil {
		projection := make([]string, len(t.projection))
		copy(projection, t.projection)
		t.projection = projection
	}

	if t.columns != nil {
		columns := make([]int, len(t.columns))
		copy(columns, t.columns)
		t.columns = columns
	}

	return &t
}

// replaceData replaces the data in this table with the one in the source
func (t *Table) replaceData(src *TableData) {
	t.data = src.copy()
}

// normalizeSchemaForRewrite returns a copy of the schema provided suitable for rewriting. This is necessary because
// the engine doesn't currently enforce that primary key columns are not nullable, rather taking the definition
// directly from the user.
func normalizeSchemaForRewrite(newSch sql.PrimaryKeySchema) sql.PrimaryKeySchema {
	schema := newSch.Schema.Copy()
	for _, col := range schema {
		if col.PrimaryKey {
			col.Nullable = false
		}
	}

	return sql.NewPrimaryKeySchema(schema, newSch.PkOrdinals...)
}

// DropPrimaryKey implements the PrimaryKeyAlterableTable
// TODO: get rid of this / make it error?
func (t *Table) DropPrimaryKey(ctx *sql.Context) error {
	data := t.sessionTableData(ctx)

	// Must drop auto increment property before dropping primary key
	if data.schema.HasAutoIncrement() {
		return sql.ErrWrongAutoKey.New()
	}

	pks := make([]*sql.Column, 0)
	for _, col := range data.schema.Schema {
		if col.PrimaryKey {
			pks = append(pks, col)
		}
	}

	if len(pks) == 0 {
		return sql.ErrCantDropFieldOrKey.New("PRIMARY")
	}

	// Check for foreign key relationships
	for _, pk := range pks {
		if fkName, ok := columnInFkRelationship(pk.Name, data.fkColl.Keys()); ok {
			return sql.ErrCantDropIndex.New("PRIMARY", fkName)
		}
	}

	for _, c := range pks {
		c.PrimaryKey = false
	}

	delete(data.indexes, "PRIMARY")
	data.schema.PkOrdinals = []int{}

	return nil
}

func columnInFkRelationship(col string, fkc []sql.ForeignKeyConstraint) (string, bool) {
	colsInFks := make(map[string]string)
	for _, fk := range fkc {
		allCols := append(fk.Columns, fk.ParentColumns...)
		for _, ac := range allCols {
			colsInFks[ac] = fk.Name
		}
	}

	fkName, ok := colsInFks[col]
	return fkName, ok
}

type partitionIndexKeyValueIter struct {
	table   *Table
	iter    sql.PartitionIter
	columns []int
}

func (i *partitionIndexKeyValueIter) Next(ctx *sql.Context) (sql.Partition, sql.IndexKeyValueIter, error) {
	p, err := i.iter.Next(ctx)
	if err != nil {
		return nil, nil, err
	}

	iter, err := i.table.PartitionRows(ctx, p)
	if err != nil {
		return nil, nil, err
	}

	return p, &indexKeyValueIter{
		key:     string(p.Key()),
		iter:    iter,
		columns: i.columns,
	}, nil
}

func (i *partitionIndexKeyValueIter) Close(ctx *sql.Context) error {
	return i.iter.Close(ctx)
}

var errColumnNotFound = errors.NewKind("could not find column %s")

type indexKeyValueIter struct {
	key     string
	iter    sql.RowIter
	columns []int
	pos     int
}

func (i *indexKeyValueIter) Next(ctx *sql.Context) ([]interface{}, []byte, error) {
	row, err := i.iter.Next(ctx)
	if err != nil {
		return nil, nil, err
	}

	value := &IndexValue{Key: i.key, Pos: i.pos}
	data, err := EncodeIndexValue(value)
	if err != nil {
		return nil, nil, err
	}

	i.pos++
	return projectOnRow(i.columns, row), data, nil
}

func (i *indexKeyValueIter) Close(ctx *sql.Context) error {
	return i.iter.Close(ctx)
}

// NewHistogramMapFromTable will construct a HistogramMap given a Table
// TODO: this is copied from the information_schema package, and should be moved to a more general location
func NewHistogramMapFromTable(ctx *sql.Context, t sql.Table) (sql.HistogramMap, error) {
	// initialize histogram map
	histMap := make(sql.HistogramMap)
	cols := t.Schema()
	for _, col := range cols {
		hist := new(sql.Histogram)
		hist.Min = math.MaxFloat64
		hist.Max = -math.MaxFloat64
		histMap[col.Name] = hist
	}

	// freqMap can be adapted to a histogram with any number of buckets
	freqMap := make(map[string]map[float64]uint64)
	for _, col := range cols {
		freqMap[col.Name] = make(map[float64]uint64)
	}

	partIter, err := t.Partitions(ctx)
	if err != nil {
		return nil, err
	}

	for {
		part, err := partIter.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		iter, err := t.PartitionRows(ctx, part)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		for {
			row, err := iter.Next(ctx)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}

			for i, col := range cols {
				hist, ok := histMap[col.Name]
				if !ok {
					panic("histogram was not initialized for this column; shouldn't be possible")
				}

				if row[i] == nil {
					hist.NullCount++
					continue
				}

				val, _, err := types.Float64.Convert(row[i])
				if err != nil {
					continue // silently skip unsupported column types for now
				}
				v := val.(float64)

				if freq, ok := freqMap[col.Name][v]; ok {
					freq++
				} else {
					freqMap[col.Name][v] = 1
					hist.DistinctCount++
				}

				hist.Mean += v
				hist.Min = math.Min(hist.Min, v)
				hist.Max = math.Max(hist.Max, v)
				hist.Count++
			}
		}
	}

	// add buckets to histogram in sorted order
	for colName, freqs := range freqMap {
		keys := make([]float64, 0)
		for k := range freqs {
			keys = append(keys, k)
		}
		sort.Float64s(keys)

		hist := histMap[colName]
		if hist.Count == 0 {
			hist.Min = 0
			hist.Max = 0
			continue
		}

		hist.Mean /= float64(hist.Count)
		for _, k := range keys {
			bucket := &sql.HistogramBucket{
				LowerBound: k,
				UpperBound: k,
				Frequency:  float64(freqs[k]) / float64(hist.Count),
			}
			hist.Buckets = append(hist.Buckets, bucket)
		}
	}

	return histMap, nil
}

func (t Table) ShouldRewriteTable(ctx *sql.Context, oldSchema, newSchema sql.PrimaryKeySchema, oldColumn, newColumn *sql.Column) bool {
	return orderChanged(oldSchema, newSchema, oldColumn, newColumn) ||
		isColumnDrop(oldSchema, newSchema) ||
		isPrimaryKeyChange(oldSchema, newSchema)
}

func orderChanged(oldSchema, newSchema sql.PrimaryKeySchema, oldColumn, newColumn *sql.Column) bool {
	if oldColumn == nil || newColumn == nil {
		return false
	}

	return oldSchema.Schema.IndexOfColName(oldColumn.Name) != newSchema.Schema.IndexOfColName(newColumn.Name)
}

func isPrimaryKeyChange(oldSchema sql.PrimaryKeySchema,
	newSchema sql.PrimaryKeySchema) bool {
	return len(newSchema.PkOrdinals) != len(oldSchema.PkOrdinals)
}

func isColumnDrop(oldSchema sql.PrimaryKeySchema, newSchema sql.PrimaryKeySchema) bool {
	return len(oldSchema.Schema) > len(newSchema.Schema)
}

func (t Table) RewriteInserter(ctx *sql.Context, oldSchema, newSchema sql.PrimaryKeySchema, oldColumn, newColumn *sql.Column, idxCols []sql.IndexColumn) (sql.RowInserter, error) {
	// TODO: this is insufficient: we need prevent dropping any index that is used by a primary key (or the engine does)
	if isPrimaryKeyDrop(oldSchema, newSchema) && primaryKeyIsAutoincrement(oldSchema) {
		return nil, sql.ErrWrongAutoKey.New()
	}

	if isPrimaryKeyChange(oldSchema, newSchema) {
		err := validatePrimaryKeyChange(ctx, oldSchema, newSchema, idxCols)
		if err != nil {
			return nil, err
		}
	}

	return t.getRewriteTableEditor(ctx, oldSchema, newSchema), nil
}

func validatePrimaryKeyChange(ctx *sql.Context, oldSchema sql.PrimaryKeySchema, newSchema sql.PrimaryKeySchema, idxCols []sql.IndexColumn) error {
	for _, idxCol := range idxCols {
		idx := newSchema.Schema.IndexOfColName(idxCol.Name)
		if idx < 0 {
			return sql.ErrColumnNotFound.New(idxCol.Name)
		}
		col := newSchema.Schema[idx]
		if col.PrimaryKey && idxCol.Length > 0 && types.IsText(col.Type) {
			return sql.ErrUnsupportedIndexPrefix.New(col.Name)
		}
	}

	return nil
}

func primaryKeyIsAutoincrement(schema sql.PrimaryKeySchema) bool {
	for _, ordinal := range schema.PkOrdinals {
		if schema.Schema[ordinal].AutoIncrement {
			return true
		}
	}
	return false
}

func isPrimaryKeyDrop(oldSchema sql.PrimaryKeySchema, newSchema sql.PrimaryKeySchema) bool {
	return len(oldSchema.PkOrdinals) > 0 && len(newSchema.PkOrdinals) == 0
}

// modifyFulltextIndexesForRewrite will modify the fulltext indexes of a table to correspond to a new schema before a rewrite.
func (t *Table) modifyFulltextIndexesForRewrite(
	ctx *sql.Context,
	data *TableData,
	oldSchema sql.PrimaryKeySchema,
) error {
	keyCols, _, err := fulltext.GetKeyColumns(ctx, data.Table(nil))
	if err != nil {
		return err
	}

	newIndexes := make(map[string]sql.Index)
	for name, idx := range data.indexes {
		if !idx.IsFullText() {
			newIndexes[name] = idx
			continue
		}

		if t.db == nil { // Rewrite your test if you run into this
			return fmt.Errorf("database is nil, which can only happen when adding a table outside of the SQL path, such as during harness creation")
		}

		memIdx, ok := idx.(*Index)
		if !ok { // This should never happen
			return fmt.Errorf("index returns true for FULLTEXT, but does not implement interface")
		}

		newExprs := removeDroppedColumns(data.schema, memIdx)
		if len(newExprs) == 0 {
			// omit this index, no columns in it left in new schema
			continue
		}

		newIdx := memIdx.copy()
		newIdx.fulltextInfo.KeyColumns = keyCols
		newIdx.Exprs = newExprs

		newIndexes[name] = newIdx
	}

	data.indexes = newIndexes

	return nil
}

func removeDroppedColumns(schema sql.PrimaryKeySchema, idx *Index) []sql.Expression {
	var newExprs []sql.Expression
	for _, expr := range idx.Exprs {
		if gf, ok := expr.(*expression.GetField); ok {
			idx := schema.Schema.IndexOfColName(gf.Name())
			if idx < 0 {
				continue
			}
		}
		newExprs = append(newExprs, expr)
	}
	return newExprs
}

func hasNullForAnyCols(row sql.Row, cols []int) bool {
	for _, idx := range cols {
		if row[idx] == nil {
			return true
		}
	}
	return false
}

// TableRevision is a container for memory tables to run basic smoke tests for versioned queries. It overrides only
// enough of the Table interface required to pass those tests. Memory tables have a flag to force them to ignore
// session data and use embedded data, which is required for the versioned table tests to pass.
type TableRevision struct {
	*Table
}

var _ MemTable = (*TableRevision)(nil)

func (t *TableRevision) Inserter(ctx *sql.Context) sql.RowInserter {
	ea := newTableEditAccumulator(t.Table.data)

	uniqIdxCols, prefixLengths := t.data.indexColsForTableEditor()
	return &tableEditor{
		editedTable:   t.Table,
		initialTable:  t.copy(),
		ea:            ea,
		uniqueIdxCols: uniqIdxCols,
		prefixLengths: prefixLengths,
	}
}

func (t *TableRevision) AddColumn(ctx *sql.Context, column *sql.Column, order *sql.ColumnOrder) error {
	newColIdx, data := addColumnToSchema(ctx, t.data, column, order)

	err := insertValueInRows(ctx, data, newColIdx, column.Default)
	if err != nil {
		return err
	}

	t.data = data
	return nil
}

func (t *TableRevision) IgnoreSessionData() bool {
	return true
}
