// Copyright 2017 PingCAP, Inc.
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

package ranger

import (
	"github.com/pingcap/errors"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/model"
	"github.com/pingcap/tidb/expression"
	"github.com/pingcap/tidb/sessionctx"
	"github.com/pingcap/tidb/types"
	"github.com/pingcap/tidb/util/chunk"
	"github.com/pingcap/tidb/util/collate"
)

// detachColumnCNFConditions detaches the condition for calculating range from the other conditions.
// Please make sure that the top level is CNF form.
func detachColumnCNFConditions(sctx sessionctx.Context, conditions []expression.Expression, checker *conditionChecker) ([]expression.Expression, []expression.Expression) {
	var accessConditions, filterConditions []expression.Expression
	for _, cond := range conditions {
		if sf, ok := cond.(*expression.ScalarFunction); ok && sf.FuncName.L == ast.LogicOr {
			dnfItems := expression.FlattenDNFConditions(sf)
			columnDNFItems, hasResidual := detachColumnDNFConditions(sctx, dnfItems, checker)
			// If this CNF has expression that cannot be resolved as access condition, then the total DNF expression
			// should be also appended into filter condition.
			if hasResidual {
				filterConditions = append(filterConditions, cond)
			}
			if len(columnDNFItems) == 0 {
				continue
			}
			rebuildDNF := expression.ComposeDNFCondition(sctx, columnDNFItems...)
			accessConditions = append(accessConditions, rebuildDNF)
			continue
		}
		if !checker.check(cond) {
			filterConditions = append(filterConditions, cond)
			continue
		}
		accessConditions = append(accessConditions, cond)
		if checker.shouldReserve {
			filterConditions = append(filterConditions, cond)
			checker.shouldReserve = checker.length != types.UnspecifiedLength
		}
	}
	return accessConditions, filterConditions
}

// detachColumnDNFConditions detaches the condition for calculating range from the other conditions.
// Please make sure that the top level is DNF form.
func detachColumnDNFConditions(sctx sessionctx.Context, conditions []expression.Expression, checker *conditionChecker) ([]expression.Expression, bool) {
	var (
		hasResidualConditions bool
		accessConditions      []expression.Expression
	)
	for _, cond := range conditions {
		if sf, ok := cond.(*expression.ScalarFunction); ok && sf.FuncName.L == ast.LogicAnd {
			cnfItems := expression.FlattenCNFConditions(sf)
			columnCNFItems, others := detachColumnCNFConditions(sctx, cnfItems, checker)
			if len(others) > 0 {
				hasResidualConditions = true
			}
			// If one part of DNF has no access condition. Then this DNF cannot get range.
			if len(columnCNFItems) == 0 {
				return nil, true
			}
			rebuildCNF := expression.ComposeCNFCondition(sctx, columnCNFItems...)
			accessConditions = append(accessConditions, rebuildCNF)
		} else if checker.check(cond) {
			accessConditions = append(accessConditions, cond)
			if checker.shouldReserve {
				hasResidualConditions = true
				checker.shouldReserve = checker.length != types.UnspecifiedLength
			}
		} else {
			return nil, true
		}
	}
	return accessConditions, hasResidualConditions
}

// getEqOrInColOffset checks if the expression is a eq function that one side is constant and another is column or an
// in function which is `column in (constant list)`.
// If so, it will return the offset of this column in the slice, otherwise return -1 for not found.
func getEqOrInColOffset(expr expression.Expression, cols []*expression.Column) int {
	f, ok := expr.(*expression.ScalarFunction)
	if !ok {
		return -1
	}
	_, collation := expr.CharsetAndCollation(f.GetCtx())
	switch f.FuncName.L {
	case ast.LogicOr:
		dnfItems := expression.FlattenDNFConditions(f)
		offset := int(-1)
		for _, dnfItem := range dnfItems {
			curOffset := getEqOrInColOffset(dnfItem, cols)
			if curOffset == -1 {
				return -1
			}
			if offset != -1 && curOffset != offset {
				return -1
			}
			offset = curOffset
		}
		return offset
	case ast.EQ, ast.NullEQ:
		if c, ok := f.GetArgs()[0].(*expression.Column); ok {
			if c.RetType.EvalType() == types.ETString && !collate.CompatibleCollate(c.RetType.Collate, collation) {
				return -1
			}
			if constVal, ok := f.GetArgs()[1].(*expression.Constant); ok {
				val, err := constVal.Eval(chunk.Row{})
				if err != nil || val.IsNull() {
					// treat col<=>null as range scan instead of point get to avoid incorrect results
					// when nullable unique index has multiple matches for filter x is null
					return -1
				}
				for i, col := range cols {
					if col.Equal(nil, c) {
						return i
					}
				}
			}
		}
		if c, ok := f.GetArgs()[1].(*expression.Column); ok {
			if c.RetType.EvalType() == types.ETString && !collate.CompatibleCollate(c.RetType.Collate, collation) {
				return -1
			}
			if constVal, ok := f.GetArgs()[0].(*expression.Constant); ok {
				val, err := constVal.Eval(chunk.Row{})
				if err != nil || val.IsNull() {
					return -1
				}
				for i, col := range cols {
					if col.Equal(nil, c) {
						return i
					}
				}
			}
		}
	case ast.In:
		c, ok := f.GetArgs()[0].(*expression.Column)
		if !ok {
			return -1
		}
		if c.RetType.EvalType() == types.ETString && !collate.CompatibleCollate(c.RetType.Collate, collation) {
			return -1
		}
		for _, arg := range f.GetArgs()[1:] {
			if _, ok := arg.(*expression.Constant); !ok {
				return -1
			}
		}
		for i, col := range cols {
			if col.Equal(nil, c) {
				return i
			}
		}
	}
	return -1
}

// extractIndexPointRangesForCNF extracts a CNF item from the input CNF expressions, such that the CNF item
// is totally composed of point range filters.
// e.g, for input CNF expressions ((a,b) in ((1,1),(2,2))) and a > 1 and ((a,b,c) in (1,1,1),(2,2,2))
// ((a,b,c) in (1,1,1),(2,2,2)) would be extracted.
func extractIndexPointRangesForCNF(sctx sessionctx.Context, conds []expression.Expression, cols []*expression.Column, lengths []int) (*DetachRangeResult, int, error) {
	if len(conds) < 2 {
		return nil, -1, nil
	}
	var r *DetachRangeResult
	maxNumCols := int(0)
	offset := int(-1)
	for i, cond := range conds {
		tmpConds := []expression.Expression{cond}
		colSets := expression.ExtractColumnSet(tmpConds)
		origColNum := colSets.Len()
		if origColNum == 0 {
			continue
		}
		if l := len(cols); origColNum > l {
			origColNum = l
		}
		currCols := cols[:origColNum]
		currLengths := lengths[:origColNum]
		res, err := DetachCondAndBuildRangeForIndex(sctx, tmpConds, currCols, currLengths)
		if err != nil {
			return nil, -1, err
		}
		if len(res.Ranges) == 0 {
			return &DetachRangeResult{}, -1, nil
		}
		if len(res.AccessConds) == 0 || len(res.RemainedConds) > 0 {
			continue
		}
		sameLens, allPoints := true, true
		numCols := int(0)
		for i, ran := range res.Ranges {
			if !ran.IsPoint(sctx.GetSessionVars().StmtCtx) {
				allPoints = false
				break
			}
			if i == 0 {
				numCols = len(ran.LowVal)
			} else if numCols != len(ran.LowVal) {
				sameLens = false
				break
			}
		}
		if !allPoints || !sameLens {
			continue
		}
		if numCols > maxNumCols {
			r = res
			offset = i
			maxNumCols = numCols
		}
	}
	if r != nil {
		r.IsDNFCond = false
	}
	return r, offset, nil
}

// detachCNFCondAndBuildRangeForIndex will detach the index filters from table filters. These conditions are connected with `and`
// It will first find the point query column and then extract the range query column.
// considerDNF is true means it will try to extract access conditions from the DNF expressions.
func (d *rangeDetacher) detachCNFCondAndBuildRangeForIndex(conditions []expression.Expression, tpSlice []*types.FieldType, considerDNF bool) (*DetachRangeResult, error) {
	var (
		eqCount int
		ranges  []*Range
		err     error
	)
	res := &DetachRangeResult{}

	accessConds, filterConds, newConditions, emptyRange := ExtractEqAndInCondition(d.sctx, conditions, d.cols, d.lengths)
	if emptyRange {
		return res, nil
	}
	for ; eqCount < len(accessConds); eqCount++ {
		if accessConds[eqCount].(*expression.ScalarFunction).FuncName.L != ast.EQ {
			break
		}
	}
	eqOrInCount := len(accessConds)
	res.EqCondCount = eqCount
	res.EqOrInCount = eqOrInCount
	ranges, err = d.buildCNFIndexRange(tpSlice, eqOrInCount, accessConds)
	if err != nil {
		return res, err
	}
	res.Ranges = ranges
	res.AccessConds = accessConds
	res.RemainedConds = filterConds
	if eqOrInCount == len(d.cols) || len(newConditions) == 0 {
		res.RemainedConds = append(res.RemainedConds, newConditions...)
		return res, nil
	}
	checker := &conditionChecker{
		colUniqueID:   d.cols[eqOrInCount].UniqueID,
		length:        d.lengths[eqOrInCount],
		shouldReserve: d.lengths[eqOrInCount] != types.UnspecifiedLength,
	}
	if considerDNF {
		pointRes, offset, err := extractIndexPointRangesForCNF(d.sctx, conditions, d.cols, d.lengths)
		if err != nil {
			return nil, err
		}
		if pointRes != nil {
			if len(pointRes.Ranges) == 0 {
				return &DetachRangeResult{}, nil
			}
			if len(pointRes.Ranges[0].LowVal) > eqOrInCount {
				res = pointRes
				eqOrInCount = len(res.Ranges[0].LowVal)
				newConditions = newConditions[:0]
				newConditions = append(newConditions, conditions[:offset]...)
				newConditions = append(newConditions, conditions[offset+1:]...)
				if eqOrInCount == len(d.cols) || len(newConditions) == 0 {
					res.RemainedConds = append(res.RemainedConds, newConditions...)
					return res, nil
				}
			}
		}
		if eqOrInCount > 0 {
			newCols := d.cols[eqOrInCount:]
			newLengths := d.lengths[eqOrInCount:]
			saveConditions := newConditions
			// For cases like `a = 1 and ((b = 1 and c = 1) or (b = 3)) and d = 3` on index (a,b,c),
			// or condition cases need to be handled separately to take advantage of more index columns.
			for i, cond := range saveConditions {
				var condList []expression.Expression
				condList = append(condList, cond)
				if sf, ok := cond.(*expression.ScalarFunction); ok && sf.FuncName.L == ast.LogicOr {
					tailRes, err := DetachCondAndBuildRangeForIndex(d.sctx, condList, newCols, newLengths)
					if err != nil {
						return nil, err
					}
					if len(tailRes.Ranges) == 0 {
						return &DetachRangeResult{}, nil
					}
					if len(tailRes.AccessConds) > 0 {
						res.Ranges = appendRanges2PointRanges(res.Ranges, tailRes.Ranges)
						res.AccessConds = append(res.AccessConds, tailRes.AccessConds...)
						newConditions = append(newConditions[:i], newConditions[i+1:]...)
					}
					for _, resRange := range res.Ranges {
						if eqOrInCount < len(resRange.LowVal) {
							eqOrInCount = len(resRange.LowVal)
							if eqOrInCount < len(d.cols) {
								newCols = d.cols[eqOrInCount:]
								newLengths = d.lengths[eqOrInCount:]
							} else if eqOrInCount == len(d.cols) {
								res.RemainedConds = append(res.RemainedConds, newConditions...)
							}
						}
					}
					if res.EqOrInCount > 0 {
						if res.EqOrInCount == res.EqCondCount {
							res.EqCondCount = res.EqCondCount + tailRes.EqCondCount
						}
						res.EqOrInCount = res.EqOrInCount + tailRes.EqOrInCount
					}
				}
			}
			tailRes, err := DetachCondAndBuildRangeForIndex(d.sctx, newConditions, newCols, newLengths)
			if err != nil {
				return nil, err
			}
			if len(tailRes.Ranges) == 0 {
				return &DetachRangeResult{}, nil
			}
			if len(tailRes.AccessConds) > 0 {
				for i, resRange := range res.Ranges {
					var saveRange []*Range
					saveRange = append(saveRange, resRange)
					if len(resRange.LowVal) == eqOrInCount {
						saveRange = appendRanges2PointRanges(saveRange, tailRes.Ranges)
						res.Ranges[i] = saveRange[0]
					} else if len(resRange.LowVal) < eqOrInCount {
						res.RemainedConds = append(res.RemainedConds, tailRes.AccessConds...)
					}
				}
				res.AccessConds = append(res.AccessConds, tailRes.AccessConds...)
			}
			res.RemainedConds = append(res.RemainedConds, tailRes.RemainedConds...)
			// For cases like `((a = 1 and b = 1) or (a = 2 and b = 2)) and c = 1` on index (a,b,c), eqOrInCount is 2,
			// res.EqOrInCount is 0, and tailRes.EqOrInCount is 1. We should not set res.EqOrInCount to 1, otherwise,
			// `b = CorrelatedColumn` would be extracted as access conditions as well, which is not as expected at least for now.
			if res.EqOrInCount > 0 {
				if res.EqOrInCount == res.EqCondCount {
					res.EqCondCount = res.EqCondCount + tailRes.EqCondCount
				}
				res.EqOrInCount = res.EqOrInCount + tailRes.EqOrInCount
			}
			return res, nil
		}
		// `eqOrInCount` must be 0 when coming here.
		res.AccessConds, res.RemainedConds = detachColumnCNFConditions(d.sctx, newConditions, checker)
		ranges, err = d.buildCNFIndexRange(tpSlice, 0, res.AccessConds)
		if err != nil {
			return nil, err
		}
		res.Ranges = ranges
		return res, nil
	}
	for _, cond := range newConditions {
		if !checker.check(cond) {
			filterConds = append(filterConds, cond)
			continue
		}
		accessConds = append(accessConds, cond)
	}
	ranges, err = d.buildCNFIndexRange(tpSlice, eqOrInCount, accessConds)
	if err != nil {
		return nil, err
	}
	res.Ranges = ranges
	res.AccessConds = accessConds
	res.RemainedConds = filterConds
	return res, nil
}

// ExtractEqAndInCondition will split the given condition into three parts by the information of index columns and their lengths.
// accesses: The condition will be used to build range.
// filters: filters is the part that some access conditions need to be evaluate again since it's only the prefix part of char column.
// newConditions: We'll simplify the given conditions if there're multiple in conditions or eq conditions on the same column.
//   e.g. if there're a in (1, 2, 3) and a in (2, 3, 4). This two will be combined to a in (2, 3) and pushed to newConditions.
// bool: indicate whether there's nil range when merging eq and in conditions.
func ExtractEqAndInCondition(sctx sessionctx.Context, conditions []expression.Expression,
	cols []*expression.Column, lengths []int) ([]expression.Expression, []expression.Expression, []expression.Expression, bool) {
	var filters []expression.Expression
	rb := builder{sc: sctx.GetSessionVars().StmtCtx}
	accesses := make([]expression.Expression, len(cols))
	points := make([][]point, len(cols))
	mergedAccesses := make([]expression.Expression, len(cols))
	newConditions := make([]expression.Expression, 0, len(conditions))
	for _, cond := range conditions {
		offset := getEqOrInColOffset(cond, cols)
		if offset == -1 {
			newConditions = append(newConditions, cond)
			continue
		}
		if accesses[offset] == nil {
			accesses[offset] = cond
			continue
		}
		// Multiple Eq/In conditions for one column in CNF, apply intersection on them
		// Lazily compute the points for the previously visited Eq/In
		if mergedAccesses[offset] == nil {
			mergedAccesses[offset] = accesses[offset]
			points[offset] = rb.build(accesses[offset])
		}
		points[offset] = rb.intersection(points[offset], rb.build(cond))
		// Early termination if false expression found
		if len(points[offset]) == 0 {
			return nil, nil, nil, true
		}
	}
	for i, ma := range mergedAccesses {
		if ma == nil {
			if accesses[i] != nil {
				newConditions = append(newConditions, accesses[i])
			}
			continue
		}
		accesses[i] = points2EqOrInCond(sctx, points[i], cols[i])
		newConditions = append(newConditions, accesses[i])
	}
	for i, cond := range accesses {
		if cond == nil {
			accesses = accesses[:i]
			break
		}
		if lengths[i] != types.UnspecifiedLength {
			filters = append(filters, cond)
		}
	}
	// We should remove all accessConds, so that they will not be added to filter conditions.
	newConditions = removeAccessConditions(newConditions, accesses)
	return accesses, filters, newConditions, false
}

// detachDNFCondAndBuildRangeForIndex will detach the index filters from table filters when it's a DNF.
// We will detach the conditions of every DNF items, then compose them to a DNF.
func (d *rangeDetacher) detachDNFCondAndBuildRangeForIndex(condition *expression.ScalarFunction, newTpSlice []*types.FieldType) ([]*Range, []expression.Expression, bool, error) {
	sc := d.sctx.GetSessionVars().StmtCtx
	firstColumnChecker := &conditionChecker{
		colUniqueID:   d.cols[0].UniqueID,
		shouldReserve: d.lengths[0] != types.UnspecifiedLength,
		length:        d.lengths[0],
	}
	rb := builder{sc: sc}
	dnfItems := expression.FlattenDNFConditions(condition)
	newAccessItems := make([]expression.Expression, 0, len(dnfItems))
	var totalRanges []*Range
	hasResidual := false
	for _, item := range dnfItems {
		if sf, ok := item.(*expression.ScalarFunction); ok && sf.FuncName.L == ast.LogicAnd {
			cnfItems := expression.FlattenCNFConditions(sf)
			var accesses, filters []expression.Expression
			res, err := d.detachCNFCondAndBuildRangeForIndex(cnfItems, newTpSlice, true)
			if err != nil {
				return nil, nil, false, nil
			}
			ranges := res.Ranges
			accesses = res.AccessConds
			filters = res.RemainedConds
			if len(accesses) == 0 {
				return FullRange(), nil, true, nil
			}
			if len(filters) > 0 {
				hasResidual = true
			}
			totalRanges = append(totalRanges, ranges...)
			newAccessItems = append(newAccessItems, expression.ComposeCNFCondition(d.sctx, accesses...))
		} else if firstColumnChecker.check(item) {
			if firstColumnChecker.shouldReserve {
				hasResidual = true
				firstColumnChecker.shouldReserve = d.lengths[0] != types.UnspecifiedLength
			}
			points := rb.build(item)
			ranges, err := points2Ranges(sc, points, newTpSlice[0])
			if err != nil {
				return nil, nil, false, errors.Trace(err)
			}
			totalRanges = append(totalRanges, ranges...)
			newAccessItems = append(newAccessItems, item)
		} else {
			return FullRange(), nil, true, nil
		}
	}

	// Take prefix index into consideration.
	if hasPrefix(d.lengths) {
		fixPrefixColRange(totalRanges, d.lengths, newTpSlice)
	}
	totalRanges, err := UnionRanges(sc, totalRanges, d.mergeConsecutive)
	if err != nil {
		return nil, nil, false, errors.Trace(err)
	}

	return totalRanges, []expression.Expression{expression.ComposeDNFCondition(d.sctx, newAccessItems...)}, hasResidual, nil
}

// DetachRangeResult wraps up results when detaching conditions and builing ranges.
type DetachRangeResult struct {
	// Ranges is the ranges extracted and built from conditions.
	Ranges []*Range
	// AccessConds is the extracted conditions for access.
	AccessConds []expression.Expression
	// RemainedConds is the filter conditions which should be kept after access.
	RemainedConds []expression.Expression
	// EqCondCount is the number of equal conditions extracted.
	EqCondCount int
	// EqOrInCount is the number of equal/in conditions extracted.
	EqOrInCount int
	// IsDNFCond indicates if the top layer of conditions are in DNF.
	IsDNFCond bool
}

// DetachCondAndBuildRangeForIndex will detach the index filters from table filters.
// The returned values are encapsulated into a struct DetachRangeResult, see its comments for explanation.
func DetachCondAndBuildRangeForIndex(sctx sessionctx.Context, conditions []expression.Expression, cols []*expression.Column,
	lengths []int) (*DetachRangeResult, error) {
	d := &rangeDetacher{
		sctx:             sctx,
		allConds:         conditions,
		cols:             cols,
		lengths:          lengths,
		mergeConsecutive: true,
	}
	return d.detachCondAndBuildRangeForCols()
}

type rangeDetacher struct {
	sctx             sessionctx.Context
	allConds         []expression.Expression
	cols             []*expression.Column
	lengths          []int
	mergeConsecutive bool
}

func (d *rangeDetacher) detachCondAndBuildRangeForCols() (*DetachRangeResult, error) {
	res := &DetachRangeResult{}
	newTpSlice := make([]*types.FieldType, 0, len(d.cols))
	for _, col := range d.cols {
		newTpSlice = append(newTpSlice, newFieldType(col.RetType))
	}
	if len(d.allConds) == 1 {
		if sf, ok := d.allConds[0].(*expression.ScalarFunction); ok && sf.FuncName.L == ast.LogicOr {
			ranges, accesses, hasResidual, err := d.detachDNFCondAndBuildRangeForIndex(sf, newTpSlice)
			if err != nil {
				return res, errors.Trace(err)
			}
			res.Ranges = ranges
			res.AccessConds = accesses
			res.IsDNFCond = true
			// If this DNF have something cannot be to calculate range, then all this DNF should be pushed as filter condition.
			if hasResidual {
				res.RemainedConds = d.allConds
				return res, nil
			}
			return res, nil
		}
	}
	return d.detachCNFCondAndBuildRangeForIndex(d.allConds, newTpSlice, true)
}

// DetachSimpleCondAndBuildRangeForIndex will detach the index filters from table filters.
// It will find the point query column firstly and then extract the range query column.
func DetachSimpleCondAndBuildRangeForIndex(sctx sessionctx.Context, conditions []expression.Expression,
	cols []*expression.Column, lengths []int) ([]*Range, []expression.Expression, error) {
	newTpSlice := make([]*types.FieldType, 0, len(cols))
	for _, col := range cols {
		newTpSlice = append(newTpSlice, newFieldType(col.RetType))
	}
	d := &rangeDetacher{
		sctx:             sctx,
		allConds:         conditions,
		cols:             cols,
		lengths:          lengths,
		mergeConsecutive: true,
	}
	res, err := d.detachCNFCondAndBuildRangeForIndex(conditions, newTpSlice, false)
	return res.Ranges, res.AccessConds, err
}

func removeAccessConditions(conditions, accessConds []expression.Expression) []expression.Expression {
	filterConds := make([]expression.Expression, 0, len(conditions))
	for _, cond := range conditions {
		if !expression.Contains(accessConds, cond) {
			filterConds = append(filterConds, cond)
		}
	}
	return filterConds
}

// ExtractAccessConditionsForColumn extracts the access conditions used for range calculation. Since
// we don't need to return the remained filter conditions, it is much simpler than DetachCondsForColumn.
func ExtractAccessConditionsForColumn(conds []expression.Expression, uniqueID int64) []expression.Expression {
	checker := conditionChecker{
		colUniqueID: uniqueID,
		length:      types.UnspecifiedLength,
	}
	accessConds := make([]expression.Expression, 0, 8)
	return expression.Filter(accessConds, conds, checker.check)
}

// DetachCondsForColumn detaches access conditions for specified column from other filter conditions.
func DetachCondsForColumn(sctx sessionctx.Context, conds []expression.Expression, col *expression.Column) (accessConditions, otherConditions []expression.Expression) {
	checker := &conditionChecker{
		colUniqueID: col.UniqueID,
		length:      types.UnspecifiedLength,
	}
	return detachColumnCNFConditions(sctx, conds, checker)
}

// MergeDNFItems4Col receives a slice of DNF conditions, merges some of them which can be built into ranges on a single column, then returns.
// For example, [a > 5, b > 6, c > 7, a = 1, b > 3] will become [a > 5 or a = 1, b > 6 or b > 3, c > 7].
func MergeDNFItems4Col(ctx sessionctx.Context, dnfItems []expression.Expression) []expression.Expression {
	mergedDNFItems := make([]expression.Expression, 0, len(dnfItems))
	col2DNFItems := make(map[int64][]expression.Expression)
	for _, dnfItem := range dnfItems {
		cols := expression.ExtractColumns(dnfItem)
		// If this condition contains multiple columns, we can't merge it.
		// If this column is _tidb_rowid, we also can't merge it since Selectivity() doesn't handle it, or infinite recursion will happen.
		if len(cols) != 1 || cols[0].ID == model.ExtraHandleID {
			mergedDNFItems = append(mergedDNFItems, dnfItem)
			continue
		}

		uniqueID := cols[0].UniqueID
		checker := &conditionChecker{
			colUniqueID: uniqueID,
			length:      types.UnspecifiedLength,
		}
		// If we can't use this condition to build range, we can't merge it.
		// Currently, we assume if every condition in a DNF expression can pass this check, then `Selectivity` must be able to
		// cover this entire DNF directly without recursively call `Selectivity`. If this doesn't hold in the future, this logic
		// may cause infinite recursion in `Selectivity`.
		if !checker.check(dnfItem) {
			mergedDNFItems = append(mergedDNFItems, dnfItem)
			continue
		}

		col2DNFItems[uniqueID] = append(col2DNFItems[uniqueID], dnfItem)
	}
	for _, items := range col2DNFItems {
		mergedDNFItems = append(mergedDNFItems, expression.ComposeDNFCondition(ctx, items...))
	}
	return mergedDNFItems
}
