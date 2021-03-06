// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Nathan VanBenschoten (nvanbenschoten@gmail.com)

package sql

import (
	"bytes"
	"fmt"
	"sort"
	"unsafe"

	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/sql/sqlbase"
	"github.com/cockroachdb/cockroach/util/encoding"
	"github.com/pkg/errors"
)

// window constructs a windowNode according to window function applications. This may
// adjust the render targets in the selectNode as necessary. The use of window functions
// will run with a space complexity of O(NW) (N = number of rows, W = number of windows)
// and a time complexity of O(NW) (no ordering), O(W*NlogN) (with ordering), and
// O(W*N^2) (with constant or variable sized window-frames, which are not yet supported).
//
// This code uses the following terminology throughout:
// - window:
//     the optionally-ordered subset of data over which calculations are made, defined by
//     the window definition for a given window function application.
// - built-in window functions:
//     a set of built-in functions that can only be used in the context of a window
//     through a window function application, using window function syntax.
//     Ex. row_number(), rank(), dense_rank()
// - window function application:
//     the act of applying a built-in window function or built-in aggregation function
//     over a specific window. The application performs a calculation across a set of
//     table rows that are somehow related to the current row. Unlike regular aggregate
//     functions, window function application does not cause rows to become grouped into
//     a single output row — the rows retain their separate identities.
// - window definition:
//     the defined window to apply a window function over, which is stated in a window
//     function application's OVER clause.
//     Ex. SELECT avg(x) OVER (w PARTITION BY z) FROM y
//                            ^^^^^^^^^^^^^^^^^^
// - named window specification:
//     a named window provided at the end of a SELECT clause in the WINDOW clause that
//     can be referenced by the window definition of of one or more window function
//     applications. This window can be used directly as a window definition, or can be
//     overridden in a window definition.
//     Ex. used directly: SELECT avg(x) OVER w FROM y WINDOW w AS (ORDER BY z)
//                                                           ^^^^^^^^^^^^^^^^^
//     Ex. overridden: SELECT avg(x) OVER (w PARTITION BY z) FROM y WINDOW w AS (ORDER BY z)
//                                                                         ^^^^^^^^^^^^^^^^^
func (p *planner) window(n *parser.SelectClause, s *selectNode) (*windowNode, error) {
	// Determine if a window function is being applied. We use the selectNode's
	// renders to determine this because window functions may be added to the
	// selectNode by an ORDER BY clause.
	// For instance: SELECT x FROM y ORDER BY avg(x) OVER ().
	if containsWindowFn := p.parser.WindowFuncInExprs(s.render); !containsWindowFn {
		return nil, nil
	}

	window := &windowNode{
		planner:      p,
		values:       valuesNode{columns: s.columns},
		windowRender: make([]parser.TypedExpr, len(s.render)),
	}

	visitor := extractWindowFuncsVisitor{
		n:              window,
		aggregatesSeen: make(map[*parser.FuncExpr]struct{}),
	}

	// Loop over the render expressions and extract any window functions. While looping
	// over the renders, each window function will be replaced by a separate render for
	// each of its (possibly 0) arguments in the selectNode.
	oldRenders := s.render
	oldColumns := s.columns
	s.render = make([]parser.TypedExpr, 0, len(oldRenders))
	s.columns = make([]ResultColumn, 0, len(oldColumns))
	for i := range oldRenders {
		// Add all window function applications found in oldRenders[i] to window.funcs.
		typedExpr, numFuncsAdded, err := visitor.extract(oldRenders[i])
		if err != nil {
			return nil, err
		}
		if numFuncsAdded == 0 {
			// No window functions in render.
			s.render = append(s.render, oldRenders[i])
			s.columns = append(s.columns, oldColumns[i])
		} else {
			// One or more window functions in render. Create a new render in
			// selectNode for each window function argument.
			window.windowRender[i] = typedExpr
			prevWindowCount := len(window.funcs) - numFuncsAdded
			for i, funcHolder := range window.funcs[prevWindowCount:] {
				funcHolder.funcIdx = prevWindowCount + i
				funcHolder.argIdxStart = len(s.render)
				for _, argExpr := range funcHolder.args {
					arg := argExpr.(parser.TypedExpr)
					s.render = append(s.render, arg)
					s.columns = append(s.columns, ResultColumn{
						Name: arg.String(),
						Typ:  arg.ReturnType(),
					})
				}
			}
		}
	}

	if err := window.constructWindowDefinitions(n, s); err != nil {
		return nil, err
	}

	window.wrappedValues = p.NewRowContainer(s.columns, 0)
	window.windowsAcc = p.session.OpenAccount()

	return window, nil
}

// constructWindowDefinitions creates window definitions for each window
// function application by combining specific window definition from a
// given window function application with referenced window specifications
// on the SelectClause.
func (n *windowNode) constructWindowDefinitions(sc *parser.SelectClause, s *selectNode) error {
	// Process each named window specification on the select clause.
	namedWindowSpecs := make(map[string]*parser.WindowDef, len(sc.Window))
	for _, windowDef := range sc.Window {
		name := sqlbase.NormalizeName(windowDef.Name)
		if _, ok := namedWindowSpecs[name]; ok {
			return errors.Errorf("window %q is already defined", name)
		}
		namedWindowSpecs[name] = windowDef
	}

	// Construct window definitions for each window function application.
	for _, windowFn := range n.funcs {
		windowDef, err := constructWindowDef(*windowFn.expr.WindowDef, namedWindowSpecs)
		if err != nil {
			return err
		}

		// TODO(nvanbenschoten) below we add renders to the selectNode for each
		// partition and order expression. We should handle cases where the expression
		// is already referenced by the query like sortNode does.

		// Validate PARTITION BY clause.
		for _, partition := range windowDef.Partitions {
			windowFn.partitionIdxs = append(windowFn.partitionIdxs, len(s.render))
			if err := s.addRender(parser.SelectExpr{Expr: partition}, nil); err != nil {
				return err
			}
		}

		// Validate ORDER BY clause.
		for _, orderBy := range windowDef.OrderBy {
			direction := encoding.Ascending
			if orderBy.Direction == parser.Descending {
				direction = encoding.Descending
			}
			ordering := sqlbase.ColumnOrderInfo{
				ColIdx:    len(s.render),
				Direction: direction,
			}
			windowFn.columnOrdering = append(windowFn.columnOrdering, ordering)
			if err := s.addRender(parser.SelectExpr{Expr: orderBy.Expr}, nil); err != nil {
				return err
			}
		}

		windowFn.windowDef = windowDef
	}
	return nil
}

// constructWindowDef constructs a WindowDef using the provided WindowDef value and the
// set of named window specifications on the current SELECT clause. If the provided
// WindowDef does not reference a named window spec, then it will simply be returned without
// modification. If the provided WindowDef does reference a named window spec, then the
// referenced spec will be overridden with any extra clauses from the WindowDef and returned.
func constructWindowDef(
	def parser.WindowDef,
	namedWindowSpecs map[string]*parser.WindowDef,
) (parser.WindowDef, error) {
	modifyRef := false
	var refName string
	switch {
	case def.RefName != "":
		// SELECT rank() OVER (w) FROM t WINDOW w as (...)
		// We copy the referenced window specification, and modify it if necessary.
		refName = sqlbase.NormalizeName(def.RefName)
		modifyRef = true
	case def.Name != "":
		// SELECT rank() OVER w FROM t WINDOW w as (...)
		// We use the referenced window specification directly, without modification.
		refName = sqlbase.NormalizeName(def.Name)
	}
	if refName == "" {
		return def, nil
	}

	referencedSpec, ok := namedWindowSpecs[refName]
	if !ok {
		return def, errors.Errorf("window %q does not exist", refName)
	}
	if !modifyRef {
		return *referencedSpec, nil
	}

	// referencedSpec.Partitions is always used.
	if def.Partitions != nil {
		return def, errors.Errorf("cannot override PARTITION BY clause of window %q", refName)
	}
	def.Partitions = referencedSpec.Partitions

	// referencedSpec.OrderBy is used if set.
	if referencedSpec.OrderBy != nil {
		if def.OrderBy != nil {
			return def, errors.Errorf("cannot override ORDER BY clause of window %q", refName)
		}
		def.OrderBy = referencedSpec.OrderBy
	}
	return def, nil
}

// A windowNode implements the planNode interface and handles windowing logic.
// It "wraps" a planNode which is used to retrieve the un-windowed results.
type windowNode struct {
	planner *planner

	// The "wrapped" node (which returns un-windowed results).
	plan          planNode
	wrappedValues *RowContainer

	// A sparse array holding renders specific to this windowNode. This will contain
	// nil entries for renders that do not contain window functions, and which therefore
	// can be propagated directly from the "wrapped" node.
	windowRender []parser.TypedExpr

	// The populated values for this windowNode.
	values    valuesNode
	populated bool

	// The window functions handled by this windowNode. computeWindows will populate
	// an entire column in windowValues for each windowFuncHolder, in order.
	funcs        []*windowFuncHolder
	windowValues [][]parser.Datum
	curRowIdx    int

	windowsAcc WrappableMemoryAccount

	explain explainMode
}

func (n *windowNode) Columns() ResultColumns {
	return n.values.Columns()
}

func (n *windowNode) Ordering() orderingInfo {
	// Window partitions are returned un-ordered.
	return orderingInfo{}
}

func (n *windowNode) Values() parser.DTuple {
	return n.values.Values()
}

func (n *windowNode) MarkDebug(mode explainMode) {
	if mode != explainDebug {
		panic(fmt.Sprintf("unknown debug mode %d", mode))
	}
	n.explain = mode
	n.plan.MarkDebug(mode)
}

func (n *windowNode) DebugValues() debugValues {
	if n.populated {
		return n.values.DebugValues()
	}

	// We are emitting a "buffered" row.
	vals := n.plan.DebugValues()
	if vals.output == debugValueRow {
		vals.output = debugValueBuffered
	}
	return vals
}

func (n *windowNode) expandPlan() error {
	// We do not need to recurse into the child node here; selectTopNode
	// does this for us.

	for _, e := range n.windowRender {
		if err := n.planner.expandSubqueryPlans(e); err != nil {
			return err
		}
	}

	return nil
}

func (n *windowNode) Start() error {
	if err := n.plan.Start(); err != nil {
		return err
	}

	for _, e := range n.windowRender {
		if err := n.planner.startSubqueryPlans(e); err != nil {
			return err
		}
	}

	return nil
}

func (n *windowNode) Next() (bool, error) {
	for !n.populated {
		next, err := n.plan.Next()
		if err != nil {
			return false, err
		}
		if !next {
			n.populated = true
			if err := n.computeWindows(); err != nil {
				return false, err
			}
			if err := n.populateValues(); err != nil {
				return false, err
			}
			break
		}
		if n.explain == explainDebug && n.plan.DebugValues().output != debugValueRow {
			// Pass through non-row debug values.
			return true, nil
		}

		// Add a copy of the row to wrappedValues.
		values := n.plan.Values()
		valuesCopy := make(parser.DTuple, len(values))
		copy(valuesCopy, values)
		if err := n.wrappedValues.AddRow(valuesCopy); err != nil {
			return false, err
		}

		if n.explain == explainDebug {
			// Emit a "buffered" row.
			return true, nil
		}
	}

	return n.values.Next()
}

type partitionSorter struct {
	rows     []parser.IndexedRow
	ordering sqlbase.ColumnOrdering
}

// partitionSorter implements the sort.Interface interface.
func (n *partitionSorter) Len() int           { return len(n.rows) }
func (n *partitionSorter) Swap(i, j int)      { n.rows[i], n.rows[j] = n.rows[j], n.rows[i] }
func (n *partitionSorter) Less(i, j int) bool { return n.Compare(i, j) < 0 }

// partitionSorter implements the peerGroupChecker interface.
func (n *partitionSorter) InSameGroup(i, j int) bool { return n.Compare(i, j) == 0 }

func (n *partitionSorter) Compare(i, j int) int {
	ra, rb := n.rows[i].Row, n.rows[j].Row
	for _, o := range n.ordering {
		da := ra[o.ColIdx]
		db := rb[o.ColIdx]
		if c := da.Compare(db); c != 0 {
			if o.Direction != encoding.Ascending {
				return -c
			}
			return c
		}
	}
	return 0
}

type allPeers struct{}

// allPeers implements the peerGroupChecker interface.
func (allPeers) InSameGroup(i, j int) bool { return true }

// peerGroupChecker can check if a pair of row indexes within a partition are
// in the same peer group.
type peerGroupChecker interface {
	InSameGroup(i, j int) bool
}

// computeWindows populates n.windowValues, adding a column of values to the
// 2D-slice for each window function in n.funcs.
func (n *windowNode) computeWindows() error {
	rowCount := n.wrappedValues.Len()
	if rowCount == 0 {
		return nil
	}

	windowCount := len(n.funcs)
	acc := n.windowsAcc.W(n.planner.session)

	winValSz := uintptr(rowCount) * unsafe.Sizeof([]parser.Datum{})
	winAllocSz := uintptr(rowCount*windowCount) * unsafe.Sizeof(parser.Datum(nil))
	if err := acc.Grow(int64(winValSz + winAllocSz)); err != nil {
		return err
	}

	n.windowValues = make([][]parser.Datum, rowCount)
	windowAlloc := make([]parser.Datum, rowCount*windowCount)
	for i := range n.windowValues {
		n.windowValues[i] = windowAlloc[i*windowCount : (i+1)*windowCount]
	}

	var scratchBytes []byte
	var scratchDatum []parser.Datum
	for windowIdx, windowFn := range n.funcs {
		partitions := make(map[string][]parser.IndexedRow)

		if len(windowFn.partitionIdxs) == 0 {
			// If no partition indexes are included for the window function, all
			// rows are added to the same partition, which need to be pre-allocated.
			sz := int64(uintptr(rowCount) * unsafe.Sizeof(parser.IndexedRow{}))
			if err := acc.Grow(sz); err != nil {
				return err
			}
			partitions[""] = make([]parser.IndexedRow, rowCount)
		}

		if n := len(windowFn.partitionIdxs); n > cap(scratchDatum) {
			sz := int64(uintptr(n) * unsafe.Sizeof(parser.Datum(nil)))
			if err := acc.Grow(sz); err != nil {
				return err
			}
			scratchDatum = make([]parser.Datum, n)
		} else {
			scratchDatum = scratchDatum[:n]
		}

		// Partition rows into separate partitions based on hash values of the
		// window function's PARTITION BY attribute.
		//
		// TODO(nvanbenschoten) Window functions with the same window definition
		// can share partition and sorting work.
		// See Cao et al. [http://vldb.org/pvldb/vol5/p1244_yucao_vldb2012.pdf]
		for rowI := 0; rowI < rowCount; rowI++ {
			row := n.wrappedValues.At(rowI)
			entry := parser.IndexedRow{Idx: rowI, Row: row}
			if len(windowFn.partitionIdxs) == 0 {
				// If no partition indexes are included for the window function, all
				// rows are added to the same partition.
				partitions[""][rowI] = entry
			} else {
				// If the window function has partition indexes, we hash the values of each
				// of these indexes for each row, and partition based on this hashed value.
				for i, idx := range windowFn.partitionIdxs {
					scratchDatum[i] = row[idx]
				}

				encoded, err := sqlbase.EncodeDTuple(scratchBytes, scratchDatum)
				if err != nil {
					return err
				}

				sz := int64(uintptr(len(encoded)) + unsafe.Sizeof(entry))
				if err := acc.Grow(sz); err != nil {
					return err
				}
				partitions[string(encoded)] = append(partitions[string(encoded)], entry)
				scratchBytes = encoded[:0]
			}
		}

		// For each partition, perform necessary sorting based on the window function's
		// ORDER BY attribute. After this, perform the window function computation for
		// each tuple and save the result in n.windowValues.
		//
		// TODO(nvanbenschoten)
		// - Investigate inter- and intra-partition parallelism
		// - Investigate more efficient aggregation techniques
		//   * Removable Cumulative
		//   * Segment Tree
		// See Leis et al. [http://www.vldb.org/pvldb/vol8/p1058-leis.pdf]
		for _, partition := range partitions {
			// TODO(nvanbenschoten) Handle framing here. Right now we only handle the default
			// framing option of RANGE UNBOUNDED PRECEDING. With ORDER BY, this sets the frame
			// to be all rows from the partition start up through the current row's last ORDER BY
			// peer. Without ORDER BY, all rows of the partition are included in the window frame,
			// since all rows become peers of the current row. Once we add better framing support,
			// we should flesh this logic out more.
			builtin := windowFn.expr.GetWindowConstructor()()

			// Since we only support two types of window frames (see TODO above), we only
			// need two possible types of peerGroupChecker's to help determine peer groups
			// for given tuples.
			var peerGrouper peerGroupChecker
			if windowFn.columnOrdering != nil {
				// If an ORDER BY clause is provided, order the partition and use the
				// sorter as our peerGroupChecker.
				sorter := &partitionSorter{rows: partition, ordering: windowFn.columnOrdering}
				// The sort needs to be deterministic because multiple window functions with
				// syntactically equivalent ORDER BY clauses in their window definitions
				// need to be guaranteed to be evaluated in the same order, even if the
				// ORDER BY *does not* uniquely determine an ordering. In the future, this
				// could be guaranteed by only performing a single pass over a sorted partition
				// for functions with syntactically equivalent PARTITION BY and ORDER BY clauses.
				sort.Sort(sorter)
				peerGrouper = sorter
			} else {
				// If no ORDER BY clause is provided, all rows in the partition are peers.
				peerGrouper = allPeers{}
			}

			// Iterate over peer groups within partition using a window frame.
			frame := parser.WindowFrame{
				Rows:        partition,
				ArgIdxStart: windowFn.argIdxStart,
				ArgCount:    windowFn.argCount,
				RowIdx:      0,
			}
			for frame.RowIdx < len(partition) {
				// Compute the size of the current peer group.
				frame.FirstPeerIdx = frame.RowIdx
				frame.PeerRowCount = 1
				for ; frame.FirstPeerIdx+frame.PeerRowCount < len(partition); frame.PeerRowCount++ {
					cur := frame.FirstPeerIdx + frame.PeerRowCount
					if !peerGrouper.InSameGroup(cur, cur-1) {
						break
					}
				}

				// Perform calculations on each row in the current peer group.
				for ; frame.RowIdx < frame.FirstPeerIdx+frame.PeerRowCount; frame.RowIdx++ {
					res, err := builtin.Compute(frame)
					if err != nil {
						return err
					}

					// This may overestimate, because WindowFuncs may perform internal caching.
					sz, _ := res.Size()
					if err := acc.Grow(int64(sz)); err != nil {
						return err
					}

					// Save result into n.windowValues, indexed by original row index.
					valRowIdx := partition[frame.RowIdx].Idx
					n.windowValues[valRowIdx][windowIdx] = res
				}
			}
		}
	}

	return nil
}

// populateValues populates n.values with final datum values after computing
// window result values in n.windowValues.
func (n *windowNode) populateValues() error {
	acc := n.windowsAcc.W(n.planner.session)
	rowCount := n.wrappedValues.Len()
	n.values.rows = n.planner.NewRowContainer(n.values.columns, rowCount)

	rowWidth := len(n.windowRender)

	sz := int64(uintptr(rowCount*rowWidth) * unsafe.Sizeof(parser.DTuple{}))
	if err := acc.Grow(sz); err != nil {
		return err
	}

	rowsAlloc := make(parser.DTuple, rowCount*rowWidth)
	for i := 0; i < rowCount; i++ {
		wrappedRow := n.wrappedValues.At(i)
		curRow := rowsAlloc[i*rowWidth : (i+1)*rowWidth]

		n.curRowIdx = i // Point all windowFuncHolders to the correct row values.
		curColIdx := 0
		curFnIdx := 0
		for j := 0; j < rowWidth; j++ {
			if curWindowRender := n.windowRender[j]; curWindowRender == nil {
				// If the windowRender at this index is nil, propagate the datum
				// directly from the wrapped planNode. It wasn't changed by windowNode.
				curRow[j] = wrappedRow[curColIdx]
				curColIdx++
			} else {
				// If the windowRender is not nil, ignore 0 or more columns from the wrapped
				// planNode. These were used as arguments to window functions all beneath
				// a single windowRender.
				// SELECT rank() over () from t; -> ignore 0 from wrapped values
				// SELECT (rank() over () + avg(b) over ()) from t; -> ignore 1 from wrapped values
				// SELECT (avg(a) over () + avg(b) over ()) from t; -> ignore 2 from wrapped values
				for ; curFnIdx < len(n.funcs); curFnIdx++ {
					windowFn := n.funcs[curFnIdx]
					if windowFn.argIdxStart != curColIdx {
						break
					}
					curColIdx += windowFn.argCount
				}
				// Instead, we evaluate the current window render, which depends on at least
				// one window function, at the given row.
				res, err := curWindowRender.Eval(&n.planner.evalCtx)
				if err != nil {
					return err
				}
				curRow[j] = res
			}
		}

		if err := n.values.rows.AddRow(curRow); err != nil {
			return err
		}
	}

	// Done using the output of computeWindows, release memory and clear
	// accounts.
	n.wrappedValues.Close()
	n.wrappedValues = nil
	n.windowValues = nil
	acc.Close()

	return nil
}

func (n *windowNode) ExplainPlan(_ bool) (name, description string, children []planNode) {
	name = "window"
	var buf bytes.Buffer
	for i, f := range n.funcs {
		if i > 0 {
			buf.WriteString(", ")
		}
		f.Format(&buf, parser.FmtSimple)
	}

	subplans := []planNode{n.plan}
	for _, e := range n.windowRender {
		subplans = n.planner.collectSubqueryPlans(e, subplans)
	}
	return name, buf.String(), subplans
}

func (n *windowNode) ExplainTypes(regTypes func(string, string)) {
	cols := n.Columns()
	for i, rexpr := range n.windowRender {
		if rexpr != nil {
			regTypes(fmt.Sprintf("render %s", cols[i].Name),
				parser.AsStringWithFlags(rexpr, parser.FmtShowTypes))
		}
	}
}

func (*windowNode) SetLimitHint(_ int64, _ bool) {}

func (n *windowNode) Close() {
	n.plan.Close()
	if n.wrappedValues != nil {
		n.wrappedValues.Close()
		n.wrappedValues = nil
	}
	if n.windowValues != nil {
		n.windowValues = nil
		n.windowsAcc.W(n.planner.session).Close()
	}
	n.values.Close()
}

// wrap the supplied planNode with the windowNode if windowing is required.
func (n *windowNode) wrap(plan planNode) planNode {
	if n == nil {
		return plan
	}
	n.plan = plan
	return n
}

type extractWindowFuncsVisitor struct {
	n *windowNode

	// Avoids allocations.
	subWindowVisitor parser.ContainsWindowVisitor

	// Persisted visitor state.
	aggregatesSeen map[*parser.FuncExpr]struct{}
	windowFnCount  int
	err            error
}

var _ parser.Visitor = &extractWindowFuncsVisitor{}

func (v *extractWindowFuncsVisitor) VisitPre(expr parser.Expr) (recurse bool, newExpr parser.Expr) {
	if v.err != nil {
		return false, expr
	}

	switch t := expr.(type) {
	case *parser.FuncExpr:
		switch {
		case t.IsWindowFunctionApplication():
			// Check if a parent node above this window function is an aggregate.
			if len(v.aggregatesSeen) > 0 {
				v.err = errors.Errorf("aggregate function calls cannot contain window function "+
					"call %s", t.Name)
				return false, expr
			}

			// Make sure this window function does not contain another window function.
			for _, argExpr := range t.Exprs {
				if v.subWindowVisitor.ContainsWindowFunc(argExpr) {
					v.err = fmt.Errorf("window function calls cannot be nested under %s", t.Name)
					return false, expr
				}
			}

			f := &windowFuncHolder{
				expr:     t,
				args:     t.Exprs,
				argCount: len(t.Exprs),
				window:   v.n,
			}
			v.windowFnCount++
			v.n.funcs = append(v.n.funcs, f)
			return false, f
		case t.GetAggregateConstructor() != nil:
			// If we see an aggregation that is not used in a window function, we save it
			// in the visitor's seen aggregate set. The aggregate function will remain in
			// this set until the recursion into its children is complete.
			v.aggregatesSeen[t] = struct{}{}
		}
	}
	return true, expr
}

func (v *extractWindowFuncsVisitor) VisitPost(expr parser.Expr) parser.Expr {
	if fn, ok := expr.(*parser.FuncExpr); ok {
		delete(v.aggregatesSeen, fn)
	}
	return expr
}

// Extract windowFuncHolders from exprs that use window functions and check if they are valid.
// It will return the new expression tree, along with the number of window functions seen and
// added to v.n.funcs.
// A window function is valid if:
// - it is not contained in an aggregate function
// - it does not contain another window function
// - it is either the application of a built-in window function
//   or of a built-in aggregate function
//
// For example:
// Invalid: `SELECT AVG(AVG(k) OVER ()) FROM kv`
// - The avg aggregate wraps the window function.
// Valid:      `SELECT AVG(k) OVER () FROM kv`
// Also valid: `SELECT AVG(AVG(k)) OVER () FROM kv`
// - Window functions can wrap aggregates.
// Invalid:    `SELECT NOW() OVER () FROM kv`
// - NOW() is not an aggregate or a window function.
func (v extractWindowFuncsVisitor) extract(typedExpr parser.TypedExpr) (parser.TypedExpr, int, error) {
	expr, _ := parser.WalkExpr(&v, typedExpr)
	if v.err != nil {
		return nil, 0, v.err
	}
	return expr.(parser.TypedExpr), v.windowFnCount, nil
}

var _ parser.TypedExpr = &windowFuncHolder{}
var _ parser.VariableExpr = &windowFuncHolder{}

type windowFuncHolder struct {
	window *windowNode

	expr *parser.FuncExpr
	args []parser.Expr

	funcIdx     int // index of the windowFuncHolder in window.funcs
	argIdxStart int // index of the window function's first arguments in window.wrappedValues
	argCount    int // number of arguments taken by the window function

	windowDef      parser.WindowDef
	partitionIdxs  []int
	columnOrdering sqlbase.ColumnOrdering
}

func (*windowFuncHolder) Variable() {}

func (w *windowFuncHolder) Format(buf *bytes.Buffer, f parser.FmtFlags) {
	w.expr.Format(buf, f)
}

func (w *windowFuncHolder) String() string { return parser.AsString(w) }

func (w *windowFuncHolder) Walk(v parser.Visitor) parser.Expr { return w }

func (w *windowFuncHolder) TypeCheck(_ *parser.SemaContext, desired parser.Datum) (parser.TypedExpr, error) {
	return w, nil
}

func (w *windowFuncHolder) Eval(ctx *parser.EvalContext) (parser.Datum, error) {
	// Index into the windowValues computed in windowNode.computeWindows
	// to determine the Datum value to return. Evaluating this datum
	// is almost certainly the identity.
	return w.window.windowValues[w.window.curRowIdx][w.funcIdx].Eval(ctx)
}

func (w *windowFuncHolder) ReturnType() parser.Datum {
	return w.expr.ReturnType()
}
