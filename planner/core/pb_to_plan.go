// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"strings"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/expression/aggregation"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/planner/property"
	"github.com/pingcap/tidb/planner/util"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/rowcodec"
	"github.com/pingcap/tipb/go-tipb"
)

// PBPlanBuilder uses to build physical plan from dag protocol buffers.
type PBPlanBuilder struct {
	sctx   sessionctx.Context
	tps    []*types.FieldType
	is     infoschema.InfoSchema
	ranges []*coprocessor.KeyRange
}

// NewPBPlanBuilder creates a new pb plan builder.
func NewPBPlanBuilder(sctx sessionctx.Context, is infoschema.InfoSchema, ranges []*coprocessor.KeyRange) *PBPlanBuilder {
	return &PBPlanBuilder{sctx: sctx, is: is, ranges: ranges}
}

// Build builds physical plan from dag protocol buffers.
func (b *PBPlanBuilder) Build(executors []*tipb.Executor) (p PhysicalPlan, err error) {
	var src PhysicalPlan
	for i := 0; i < len(executors); i++ {
		curr, err := b.pbToPhysicalPlan(executors[i])
		if err != nil {
			return nil, errors.Trace(err)
		}
		if src != nil {
			curr.SetChildren(src)
		}
		src = curr
	}
	_, src = b.predicatePushDown(src, nil)
	return src, nil
}

func (b *PBPlanBuilder) pbToPhysicalPlan(e *tipb.Executor) (p PhysicalPlan, err error) {
	switch e.Tp {
	case tipb.ExecType_TypeTableScan:
		p, err = b.pbToTableScan(e)
	case tipb.ExecType_TypeSelection:
		p, err = b.pbToSelection(e)
	case tipb.ExecType_TypeTopN:
		p, err = b.pbToTopN(e)
	case tipb.ExecType_TypeLimit:
		p, err = b.pbToLimit(e)
	case tipb.ExecType_TypeAggregation:
		p, err = b.pbToAgg(e, false)
	case tipb.ExecType_TypeStreamAgg:
		p, err = b.pbToAgg(e, true)
	case tipb.ExecType_TypeKill:
		p, err = b.pbToKill(e)
	default:
		// TODO: Support other types.
		err = errors.Errorf("this exec type %v doesn't support yet.", e.GetTp())
	}
	return p, err
}

func (b *PBPlanBuilder) pbToTableScan(e *tipb.Executor) (PhysicalPlan, error) {
	tblScan := e.TblScan
	tbl, ok := b.is.TableByID(tblScan.TableId)
	if !ok {
		return nil, infoschema.ErrTableNotExists.GenWithStack("Table which ID = %d does not exist.", tblScan.TableId)
	}
	dbInfo, ok := b.is.SchemaByTable(tbl.Meta())
	if !ok {
		return nil, infoschema.ErrDatabaseNotExists.GenWithStack("Database of table ID = %d does not exist.", tblScan.TableId)
	}
	// Currently only support cluster table.
	if !tbl.Type().IsClusterTable() {
		return nil, errors.Errorf("table %s is not a cluster table", tbl.Meta().Name.L)
	}
	columns, err := b.convertColumnInfo(tbl.Meta(), tblScan.Columns)
	if err != nil {
		return nil, err
	}
	schema := b.buildTableScanSchema(tbl.Meta(), columns)
	p := PhysicalMemTable{
		DBName:  dbInfo.Name,
		Table:   tbl.Meta(),
		Columns: columns,
	}.Init(b.sctx, &property.StatsInfo{}, 0)
	p.SetSchema(schema)
	if strings.ToUpper(p.Table.Name.O) == infoschema.ClusterTableSlowLog {
		extractor := &SlowQueryExtractor{}
		extractor.Desc = tblScan.Desc
		if b.ranges != nil {
			trs, err := b.decodeTimeRanges(b.ranges)
			if err != nil {
				return nil, err
			}
			for _, tr := range trs {
				extractor.setTimeRange(tr[0], tr[1])
			}
		}
		p.Extractor = extractor
	}
	return p, nil
}

func (b *PBPlanBuilder) decodeTimeRanges(keyRanges []*coprocessor.KeyRange) ([][]int64, error) {
	var krs [][]int64
	for _, kr := range keyRanges {
		if len(kr.Start) >= tablecodec.RecordRowKeyLen && len(kr.Start) >= tablecodec.RecordRowKeyLen {
			start, err := tablecodec.DecodeRowKey(kr.Start)
			var startTime int64
			if err != nil {
				startTime = 0
			} else {
				startTime, err = b.decodeToTime(start)
				if err != nil {
					return nil, err
				}
			}
			end, err := tablecodec.DecodeRowKey(kr.End)
			var endTime int64
			if err != nil {
				endTime = 0
			} else {
				endTime, err = b.decodeToTime(end)
				if err != nil {
					return nil, err
				}
			}
			kr := []int64{startTime, endTime}
			krs = append(krs, kr)
		}
	}
	return krs, nil
}

func (b *PBPlanBuilder) decodeToTime(handle kv.Handle) (int64, error) {
	tp := types.NewFieldType(mysql.TypeDatetime)
	col := rowcodec.ColInfo{ID: 0, Ft: tp}
	chk := chunk.NewChunkWithCapacity([]*types.FieldType{tp}, 1)
	coder := codec.NewDecoder(chk, nil)
	_, err := coder.DecodeOne(handle.EncodedCol(0), 0, col.Ft)
	if err != nil {
		return 0, err
	}
	datum := chk.GetRow(0).GetDatum(0, tp)
	mysqlTime := (&datum).GetMysqlTime()
	timestampInNano := time.Date(mysqlTime.Year(),
		time.Month(mysqlTime.Month()),
		mysqlTime.Day(),
		mysqlTime.Hour(),
		mysqlTime.Minute(),
		mysqlTime.Second(),
		mysqlTime.Microsecond()*1000,
		time.UTC,
	).UnixNano()
	return timestampInNano, err
}

func (b *PBPlanBuilder) buildTableScanSchema(tblInfo *model.TableInfo, columns []*model.ColumnInfo) *expression.Schema {
	schema := expression.NewSchema(make([]*expression.Column, 0, len(columns))...)
	for _, col := range tblInfo.Columns {
		for _, colInfo := range columns {
			if col.ID != colInfo.ID {
				continue
			}
			newCol := &expression.Column{
				UniqueID: b.sctx.GetSessionVars().AllocPlanColumnID(),
				ID:       col.ID,
				RetType:  &col.FieldType,
			}
			schema.Append(newCol)
		}
	}
	return schema
}

func (b *PBPlanBuilder) pbToSelection(e *tipb.Executor) (PhysicalPlan, error) {
	conds, err := expression.PBToExprs(e.Selection.Conditions, b.tps, b.sctx.GetSessionVars().StmtCtx)
	if err != nil {
		return nil, err
	}
	p := PhysicalSelection{
		Conditions: conds,
	}.Init(b.sctx, &property.StatsInfo{}, 0, &property.PhysicalProperty{})
	return p, nil
}

func (b *PBPlanBuilder) pbToTopN(e *tipb.Executor) (PhysicalPlan, error) {
	topN := e.TopN
	sc := b.sctx.GetSessionVars().StmtCtx
	byItems := make([]*util.ByItems, 0, len(topN.OrderBy))
	for _, item := range topN.OrderBy {
		expr, err := expression.PBToExpr(item.Expr, b.tps, sc)
		if err != nil {
			return nil, errors.Trace(err)
		}
		byItems = append(byItems, &util.ByItems{Expr: expr, Desc: item.Desc})
	}
	p := PhysicalTopN{
		ByItems: byItems,
		Count:   topN.Limit,
	}.Init(b.sctx, &property.StatsInfo{}, 0, &property.PhysicalProperty{})
	return p, nil
}

func (b *PBPlanBuilder) pbToLimit(e *tipb.Executor) (PhysicalPlan, error) {
	p := PhysicalLimit{
		Count: e.Limit.Limit,
	}.Init(b.sctx, &property.StatsInfo{}, 0, &property.PhysicalProperty{})
	return p, nil
}

func (b *PBPlanBuilder) pbToAgg(e *tipb.Executor, isStreamAgg bool) (PhysicalPlan, error) {
	aggFuncs, groupBys, err := b.getAggInfo(e)
	if err != nil {
		return nil, errors.Trace(err)
	}
	schema := b.buildAggSchema(aggFuncs, groupBys)
	baseAgg := basePhysicalAgg{
		AggFuncs:     aggFuncs,
		GroupByItems: groupBys,
	}
	baseAgg.schema = schema
	var partialAgg PhysicalPlan
	if isStreamAgg {
		partialAgg = baseAgg.initForStream(b.sctx, &property.StatsInfo{}, 0, &property.PhysicalProperty{})
	} else {
		partialAgg = baseAgg.initForHash(b.sctx, &property.StatsInfo{}, 0, &property.PhysicalProperty{})
	}
	return partialAgg, nil
}

func (b *PBPlanBuilder) buildAggSchema(aggFuncs []*aggregation.AggFuncDesc, groupBys []expression.Expression) *expression.Schema {
	schema := expression.NewSchema(make([]*expression.Column, 0, len(aggFuncs)+len(groupBys))...)
	for _, agg := range aggFuncs {
		newCol := &expression.Column{
			UniqueID: b.sctx.GetSessionVars().AllocPlanColumnID(),
			RetType:  agg.RetTp,
		}
		schema.Append(newCol)
	}
	return schema
}

func (b *PBPlanBuilder) getAggInfo(executor *tipb.Executor) ([]*aggregation.AggFuncDesc, []expression.Expression, error) {
	var err error
	aggFuncs := make([]*aggregation.AggFuncDesc, 0, len(executor.Aggregation.AggFunc))
	for _, expr := range executor.Aggregation.AggFunc {
		aggFunc, err := aggregation.PBExprToAggFuncDesc(b.sctx, expr, b.tps)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		aggFuncs = append(aggFuncs, aggFunc)
	}
	groupBys, err := expression.PBToExprs(executor.Aggregation.GetGroupBy(), b.tps, b.sctx.GetSessionVars().StmtCtx)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	return aggFuncs, groupBys, nil
}

func (b *PBPlanBuilder) convertColumnInfo(tblInfo *model.TableInfo, pbColumns []*tipb.ColumnInfo) ([]*model.ColumnInfo, error) {
	columns := make([]*model.ColumnInfo, 0, len(pbColumns))
	tps := make([]*types.FieldType, 0, len(pbColumns))
	for _, col := range pbColumns {
		found := false
		for _, colInfo := range tblInfo.Columns {
			if col.ColumnId == colInfo.ID {
				columns = append(columns, colInfo)
				tps = append(tps, colInfo.FieldType.Clone())
				found = true
				break
			}
		}
		if !found {
			return nil, errors.Errorf("Column ID %v of table %v not found", col.ColumnId, tblInfo.Name.L)
		}
	}
	b.tps = tps
	return columns, nil
}

func (b *PBPlanBuilder) pbToKill(e *tipb.Executor) (PhysicalPlan, error) {
	node := &ast.KillStmt{
		ConnectionID: e.Kill.ConnID,
		Query:        e.Kill.Query,
	}
	simple := Simple{Statement: node, IsFromRemote: true}
	return &PhysicalSimpleWrapper{Inner: simple}, nil
}

func (b *PBPlanBuilder) predicatePushDown(p PhysicalPlan, predicates []expression.Expression) ([]expression.Expression, PhysicalPlan) {
	if p == nil {
		return predicates, p
	}
	switch p.(type) {
	case *PhysicalMemTable:
		memTable := p.(*PhysicalMemTable)
		if memTable.Extractor == nil {
			return predicates, p
		}
		names := make([]*types.FieldName, 0, len(memTable.Columns))
		for _, col := range memTable.Columns {
			names = append(names, &types.FieldName{
				TblName:     memTable.Table.Name,
				ColName:     col.Name,
				OrigTblName: memTable.Table.Name,
				OrigColName: col.Name,
			})
		}
		// Set the expression column unique ID.
		// Since the expression is build from PB, It has not set the expression column ID yet.
		schemaCols := memTable.schema.Columns
		cols := expression.ExtractColumnsFromExpressions([]*expression.Column{}, predicates, nil)
		for i := range cols {
			cols[i].UniqueID = schemaCols[cols[i].Index].UniqueID
		}
		predicates = memTable.Extractor.Extract(b.sctx, memTable.schema, names, predicates)
		return predicates, memTable
	case *PhysicalSelection:
		selection := p.(*PhysicalSelection)
		conditions, child := b.predicatePushDown(p.Children()[0], selection.Conditions)
		if len(conditions) > 0 {
			selection.Conditions = conditions
			selection.SetChildren(child)
			return predicates, selection
		}
		return predicates, child
	default:
		if children := p.Children(); len(children) > 0 {
			_, child := b.predicatePushDown(children[0], nil)
			p.SetChildren(child)
		}
		return predicates, p
	}
}
