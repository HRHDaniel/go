// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements typechecking of statements.

package types2

import (
	"cmd/compile/internal/syntax"
	"go/constant"
	. "internal/types/errors"
	"slices"
)

// decl may be nil
func (check *Checker) funcBody(decl *declInfo, name string, sig *Signature, body *syntax.BlockStmt, iota constant.Value) {
	if check.conf.IgnoreFuncBodies {
		panic("function body not ignored")
	}

	if check.conf.Trace {
		check.trace(body.Pos(), "-- %s: %s", name, sig)
	}

	// save/restore current environment and set up function environment
	// (and use 0 indentation at function start)
	defer func(env environment, indent int) {
		check.environment = env
		check.indent = indent
	}(check.environment, check.indent)
	check.environment = environment{
		decl:    decl,
		scope:   sig.scope,
		version: check.version, // TODO(adonovan): would decl.version (if decl != nil) be better?
		iota:    iota,
		sig:     sig,
	}
	check.indent = 0

	check.stmtList(0, body.List)

	if check.hasLabel && !check.conf.IgnoreBranchErrors {
		check.labels(body)
	}

	if sig.results.Len() > 0 && !check.isTerminating(body, "") {
		check.error(body.Rbrace, MissingReturn, "missing return")
	}

	// spec: "Implementation restriction: A compiler may make it illegal to
	// declare a variable inside a function body if the variable is never used."
	check.usage(sig.scope)
}

func (check *Checker) usage(scope *Scope) {
	needUse := func(kind VarKind) bool {
		return !(kind == RecvVar || kind == ParamVar || kind == ResultVar)
	}
	var unused []*Var
	for name, elem := range scope.elems {
		elem = resolve(name, elem)
		if v, _ := elem.(*Var); v != nil && needUse(v.kind) && !check.usedVars[v] {
			unused = append(unused, v)
		}
	}
	slices.SortFunc(unused, func(a, b *Var) int {
		return cmpPos(a.pos, b.pos)
	})
	for _, v := range unused {
		check.softErrorf(v.pos, UnusedVar, "declared and not used: %s", v.name)
	}

	for _, scope := range scope.children {
		// Don't go inside function literal scopes a second time;
		// they are handled explicitly by funcBody.
		if !scope.isFunc {
			check.usage(scope)
		}
	}
}

// stmtContext is a bitset describing which
// control-flow statements are permissible,
// and provides additional context information
// for better error messages.
type stmtContext uint

const (
	// permissible control-flow statements
	breakOk stmtContext = 1 << iota
	continueOk
	fallthroughOk

	// additional context information
	finalSwitchCase
	inTypeSwitch
)

func (check *Checker) simpleStmt(s syntax.Stmt) {
	if s != nil {
		check.stmt(0, s)
	}
}

func trimTrailingEmptyStmts(list []syntax.Stmt) []syntax.Stmt {
	for i := len(list); i > 0; i-- {
		if _, ok := list[i-1].(*syntax.EmptyStmt); !ok {
			return list[:i]
		}
	}
	return nil
}

func (check *Checker) stmtList(ctxt stmtContext, list []syntax.Stmt) {
	ok := ctxt&fallthroughOk != 0
	inner := ctxt &^ fallthroughOk
	list = trimTrailingEmptyStmts(list) // trailing empty statements are "invisible" to fallthrough analysis
	for i, s := range list {
		inner := inner
		if ok && i+1 == len(list) {
			inner |= fallthroughOk
		}
		check.stmt(inner, s)
	}
}

func (check *Checker) multipleSwitchDefaults(list []*syntax.CaseClause) {
	var first *syntax.CaseClause
	for _, c := range list {
		if c.Cases == nil {
			if first != nil {
				check.errorf(c, DuplicateDefault, "multiple defaults (first at %s)", first.Pos())
				// TODO(gri) probably ok to bail out after first error (and simplify this code)
			} else {
				first = c
			}
		}
	}
}

func (check *Checker) multipleSelectDefaults(list []*syntax.CommClause) {
	var first *syntax.CommClause
	for _, c := range list {
		if c.Comm == nil {
			if first != nil {
				check.errorf(c, DuplicateDefault, "multiple defaults (first at %s)", first.Pos())
				// TODO(gri) probably ok to bail out after first error (and simplify this code)
			} else {
				first = c
			}
		}
	}
}

func (check *Checker) openScope(node syntax.Node, comment string) {
	scope := NewScope(check.scope, node.Pos(), syntax.EndPos(node), comment)
	check.recordScope(node, scope)
	check.scope = scope
}

func (check *Checker) closeScope() {
	check.scope = check.scope.Parent()
}

func (check *Checker) suspendedCall(keyword string, call syntax.Expr) {
	code := InvalidDefer
	if keyword == "go" {
		code = InvalidGo
	}

	if _, ok := call.(*syntax.CallExpr); !ok {
		check.errorf(call, code, "expression in %s must be function call", keyword)
		check.use(call)
		return
	}

	var x operand
	var msg string
	switch check.rawExpr(nil, &x, call, nil, false) {
	case conversion:
		msg = "requires function call, not conversion"
	case expression:
		msg = "discards result of"
		code = UnusedResults
	case statement:
		return
	default:
		panic("unreachable")
	}
	check.errorf(&x, code, "%s %s %s", keyword, msg, &x)
}

// goVal returns the Go value for val, or nil.
func goVal(val constant.Value) interface{} {
	// val should exist, but be conservative and check
	if val == nil {
		return nil
	}
	// Match implementation restriction of other compilers.
	// gc only checks duplicates for integer, floating-point
	// and string values, so only create Go values for these
	// types.
	switch val.Kind() {
	case constant.Int:
		if x, ok := constant.Int64Val(val); ok {
			return x
		}
		if x, ok := constant.Uint64Val(val); ok {
			return x
		}
	case constant.Float:
		if x, ok := constant.Float64Val(val); ok {
			return x
		}
	case constant.String:
		return constant.StringVal(val)
	}
	return nil
}

// A valueMap maps a case value (of a basic Go type) to a list of positions
// where the same case value appeared, together with the corresponding case
// types.
// Since two case values may have the same "underlying" value but different
// types we need to also check the value's types (e.g., byte(1) vs myByte(1))
// when the switch expression is of interface type.
type (
	valueMap  map[interface{}][]valueType // underlying Go value -> valueType
	valueType struct {
		pos syntax.Pos
		typ Type
	}
)

func (check *Checker) caseValues(x *operand, values []syntax.Expr, seen valueMap) {
L:
	for _, e := range values {
		var v operand
		check.expr(nil, &v, e)
		if x.mode == invalid || v.mode == invalid {
			continue L
		}
		check.convertUntyped(&v, x.typ)
		if v.mode == invalid {
			continue L
		}
		// Order matters: By comparing v against x, error positions are at the case values.
		res := v // keep original v unchanged
		check.comparison(&res, x, syntax.Eql, true)
		if res.mode == invalid {
			continue L
		}
		if v.mode != constant_ {
			continue L // we're done
		}
		// look for duplicate values
		if val := goVal(v.val); val != nil {
			// look for duplicate types for a given value
			// (quadratic algorithm, but these lists tend to be very short)
			for _, vt := range seen[val] {
				if Identical(v.typ, vt.typ) {
					err := check.newError(DuplicateCase)
					err.addf(&v, "duplicate case %s in expression switch", &v)
					err.addf(vt.pos, "previous case")
					err.report()
					continue L
				}
			}
			seen[val] = append(seen[val], valueType{v.Pos(), v.typ})
		}
	}
}

// isNil reports whether the expression e denotes the predeclared value nil.
func (check *Checker) isNil(e syntax.Expr) bool {
	// The only way to express the nil value is by literally writing nil (possibly in parentheses).
	if name, _ := syntax.Unparen(e).(*syntax.Name); name != nil {
		_, ok := check.lookup(name.Value).(*Nil)
		return ok
	}
	return false
}

// caseTypes typechecks the type expressions of a type case, checks for duplicate types
// using the seen map, and verifies that each type is valid with respect to the type of
// the operand x corresponding to the type switch expression. If that expression is not
// valid, x must be nil.
//
//	switch <x>.(type) {
//	case <types>: ...
//	...
//	}
//
// caseTypes returns the case-specific type for a variable v introduced through a short
// variable declaration by the type switch:
//
//	switch v := <x>.(type) {
//	case <types>: // T is the type of <v> in this case
//	...
//	}
//
// If there is exactly one type expression, T is the type of that expression. If there
// are multiple type expressions, or if predeclared nil is among the types, the result
// is the type of x. If x is invalid (nil), the result is the invalid type.
func (check *Checker) caseTypes(x *operand, types []syntax.Expr, seen map[Type]syntax.Expr) Type {
	var T Type
	var dummy operand
L:
	for _, e := range types {
		// The spec allows the value nil instead of a type.
		if check.isNil(e) {
			T = nil
			check.expr(nil, &dummy, e) // run e through expr so we get the usual Info recordings
		} else {
			T = check.varType(e)
			if !isValid(T) {
				continue L
			}
		}
		// look for duplicate types
		// (quadratic algorithm, but type switches tend to be reasonably small)
		for t, other := range seen {
			if T == nil && t == nil || T != nil && t != nil && Identical(T, t) {
				// talk about "case" rather than "type" because of nil case
				Ts := "nil"
				if T != nil {
					Ts = TypeString(T, check.qualifier)
				}
				err := check.newError(DuplicateCase)
				err.addf(e, "duplicate case %s in type switch", Ts)
				err.addf(other, "previous case")
				err.report()
				continue L
			}
		}
		seen[T] = e
		if x != nil && T != nil {
			check.typeAssertion(e, x, T, true)
		}
	}

	// spec: "In clauses with a case listing exactly one type, the variable has that type;
	// otherwise, the variable has the type of the expression in the TypeSwitchGuard.
	if len(types) != 1 || T == nil {
		T = Typ[Invalid]
		if x != nil {
			T = x.typ
		}
	}

	assert(T != nil)
	return T
}

// TODO(gri) Once we are certain that typeHash is correct in all situations, use this version of caseTypes instead.
// (Currently it may be possible that different types have identical names and import paths due to ImporterFrom.)
func (check *Checker) caseTypes_currently_unused(x *operand, xtyp *Interface, types []syntax.Expr, seen map[string]syntax.Expr) Type {
	var T Type
	var dummy operand
L:
	for _, e := range types {
		// The spec allows the value nil instead of a type.
		var hash string
		if check.isNil(e) {
			check.expr(nil, &dummy, e) // run e through expr so we get the usual Info recordings
			T = nil
			hash = "<nil>" // avoid collision with a type named nil
		} else {
			T = check.varType(e)
			if !isValid(T) {
				continue L
			}
			panic("enable typeHash(T, nil)")
			// hash = typeHash(T, nil)
		}
		// look for duplicate types
		if other := seen[hash]; other != nil {
			// talk about "case" rather than "type" because of nil case
			Ts := "nil"
			if T != nil {
				Ts = TypeString(T, check.qualifier)
			}
			err := check.newError(DuplicateCase)
			err.addf(e, "duplicate case %s in type switch", Ts)
			err.addf(other, "previous case")
			err.report()
			continue L
		}
		seen[hash] = e
		if T != nil {
			check.typeAssertion(e, x, T, true)
		}
	}

	// spec: "In clauses with a case listing exactly one type, the variable has that type;
	// otherwise, the variable has the type of the expression in the TypeSwitchGuard.
	if len(types) != 1 || T == nil {
		T = Typ[Invalid]
		if x != nil {
			T = x.typ
		}
	}

	assert(T != nil)
	return T
}

// stmt typechecks statement s.
func (check *Checker) stmt(ctxt stmtContext, s syntax.Stmt) {
	// statements must end with the same top scope as they started with
	if debug {
		defer func(scope *Scope) {
			// don't check if code is panicking
			if p := recover(); p != nil {
				panic(p)
			}
			assert(scope == check.scope)
		}(check.scope)
	}

	// process collected function literals before scope changes
	defer check.processDelayed(len(check.delayed))

	// reset context for statements of inner blocks
	inner := ctxt &^ (fallthroughOk | finalSwitchCase | inTypeSwitch)

	switch s := s.(type) {
	case *syntax.EmptyStmt:
		// ignore

	case *syntax.DeclStmt:
		check.declStmt(s.DeclList)

	case *syntax.LabeledStmt:
		check.hasLabel = true
		check.stmt(ctxt, s.Stmt)

	case *syntax.ExprStmt:
		// spec: "With the exception of specific built-in functions,
		// function and method calls and receive operations can appear
		// in statement context. Such statements may be parenthesized."
		var x operand
		kind := check.rawExpr(nil, &x, s.X, nil, false)
		var msg string
		var code Code
		switch x.mode {
		default:
			if kind == statement {
				return
			}
			msg = "is not used"
			code = UnusedExpr
		case builtin:
			msg = "must be called"
			code = UncalledBuiltin
		case typexpr:
			msg = "is not an expression"
			code = NotAnExpr
		}
		check.errorf(&x, code, "%s %s", &x, msg)

	case *syntax.SendStmt:
		var ch, val operand
		check.expr(nil, &ch, s.Chan)
		check.expr(nil, &val, s.Value)
		if ch.mode == invalid || val.mode == invalid {
			return
		}
		if elem := check.chanElem(s, &ch, false); elem != nil {
			check.assignment(&val, elem, "send")
		}

	case *syntax.ThrowStmt:
		lhs := syntax.UnpackListExpr(s.AssignStmt.Lhs)
		rhs := syntax.UnpackListExpr(s.AssignStmt.Rhs)

		last := lhs[len(lhs)-1:]
		if _, ok := last[0].(*syntax.Throw); ok {
			res := check.sig.results
			if res.Len() == 0 {
				check.error(s, IncompatibleAssign, "cannot use <throw expr> from func that does not return error")
			}

			lastLhs := res.vars[res.Len()-1:]
			check.initVars(lastLhs, last, s)
		}

		switch s.AssignStmt.Op {
		case 0:
			check.assignVars(lhs, rhs)
			return
		case syntax.Def:
			check.shortVarDecl(s.AssignStmt.Pos(), lhs, rhs)
			return
		}

		assert(false) // should never get here

	case *syntax.AssignStmt:
		if s.Rhs == nil {
			// x++ or x--
			// (no need to call unpackExpr as s.Lhs must be single-valued)
			var x operand
			check.expr(nil, &x, s.Lhs)
			if x.mode == invalid {
				return
			}
			if !allNumeric(x.typ) {
				check.errorf(s.Lhs, NonNumericIncDec, invalidOp+"%s%s%s (non-numeric type %s)", s.Lhs, s.Op, s.Op, x.typ)
				return
			}
			check.assignVar(s.Lhs, nil, &x, "assignment")
			return
		}

		lhs := syntax.UnpackListExpr(s.Lhs)
		rhs := syntax.UnpackListExpr(s.Rhs)
		switch s.Op {
		case 0:
			check.assignVars(lhs, rhs)
			return
		case syntax.Def:
			check.shortVarDecl(s.Pos(), lhs, rhs)
			return
		}

		// assignment operations
		if len(lhs) != 1 || len(rhs) != 1 {
			check.errorf(s, MultiValAssignOp, "assignment operation %s requires single-valued expressions", s.Op)
			return
		}

		var x operand
		check.binary(&x, nil, lhs[0], rhs[0], s.Op)
		check.assignVar(lhs[0], nil, &x, "assignment")

	case *syntax.CallStmt:
		kind := "go"
		if s.Tok == syntax.Defer {
			kind = "defer"
		}
		check.suspendedCall(kind, s.Call)

	case *syntax.ReturnStmt:
		res := check.sig.results
		// Return with implicit results allowed for function with named results.
		// (If one is named, all are named.)
		results := syntax.UnpackListExpr(s.Results)
		if len(results) == 0 && res.Len() > 0 && res.vars[0].name != "" {
			// spec: "Implementation restriction: A compiler may disallow an empty expression
			// list in a "return" statement if a different entity (constant, type, or variable)
			// with the same name as a result parameter is in scope at the place of the return."
			for _, obj := range res.vars {
				if alt := check.lookup(obj.name); alt != nil && alt != obj {
					err := check.newError(OutOfScopeResult)
					err.addf(s, "result parameter %s not in scope at return", obj.name)
					err.addf(alt, "inner declaration of %s", obj)
					err.report()
					// ok to continue
				}
			}
		} else {
			var lhs []*Var
			if res.Len() > 0 {
				lhs = res.vars
			}
			check.initVars(lhs, results, s)
		}

	case *syntax.BranchStmt:
		if s.Label != nil {
			check.hasLabel = true
			break // checked in 2nd pass (check.labels)
		}
		if check.conf.IgnoreBranchErrors {
			break
		}
		switch s.Tok {
		case syntax.Break:
			if ctxt&breakOk == 0 {
				check.error(s, MisplacedBreak, "break not in for, switch, or select statement")
			}
		case syntax.Continue:
			if ctxt&continueOk == 0 {
				check.error(s, MisplacedContinue, "continue not in for statement")
			}
		case syntax.Fallthrough:
			if ctxt&fallthroughOk == 0 {
				var msg string
				switch {
				case ctxt&finalSwitchCase != 0:
					msg = "cannot fallthrough final case in switch"
				case ctxt&inTypeSwitch != 0:
					msg = "cannot fallthrough in type switch"
				default:
					msg = "fallthrough statement out of place"
				}
				check.error(s, MisplacedFallthrough, msg)
			}
		case syntax.Goto:
			// goto's must have labels, should have been caught above
			fallthrough
		default:
			check.errorf(s, InvalidSyntaxTree, "branch statement: %s", s.Tok)
		}

	case *syntax.BlockStmt:
		check.openScope(s, "block")
		defer check.closeScope()

		check.stmtList(inner, s.List)

	case *syntax.IfStmt:
		check.openScope(s, "if")
		defer check.closeScope()

		check.simpleStmt(s.Init)
		var x operand
		check.expr(nil, &x, s.Cond)
		if x.mode != invalid && !allBoolean(x.typ) {
			check.error(s.Cond, InvalidCond, "non-boolean condition in if statement")
		}
		check.stmt(inner, s.Then)
		// The parser produces a correct AST but if it was modified
		// elsewhere the else branch may be invalid. Check again.
		switch s.Else.(type) {
		case nil:
			// valid or error already reported
		case *syntax.IfStmt, *syntax.BlockStmt:
			check.stmt(inner, s.Else)
		default:
			check.error(s.Else, InvalidSyntaxTree, "invalid else branch in if statement")
		}

	case *syntax.SwitchStmt:
		inner |= breakOk
		check.openScope(s, "switch")
		defer check.closeScope()

		check.simpleStmt(s.Init)

		if g, _ := s.Tag.(*syntax.TypeSwitchGuard); g != nil {
			check.typeSwitchStmt(inner|inTypeSwitch, s, g)
		} else {
			check.switchStmt(inner, s)
		}

	case *syntax.SelectStmt:
		inner |= breakOk

		check.multipleSelectDefaults(s.Body)

		for _, clause := range s.Body {
			if clause == nil {
				continue // error reported before
			}

			// clause.Comm must be a SendStmt, RecvStmt, or default case
			valid := false
			var rhs syntax.Expr // rhs of RecvStmt, or nil
			switch s := clause.Comm.(type) {
			case nil, *syntax.SendStmt:
				valid = true
			case *syntax.AssignStmt:
				if _, ok := s.Rhs.(*syntax.ListExpr); !ok {
					rhs = s.Rhs
				}
			case *syntax.ExprStmt:
				rhs = s.X
			}

			// if present, rhs must be a receive operation
			if rhs != nil {
				if x, _ := syntax.Unparen(rhs).(*syntax.Operation); x != nil && x.Y == nil && x.Op == syntax.Recv {
					valid = true
				}
			}

			if !valid {
				check.error(clause.Comm, InvalidSelectCase, "select case must be send or receive (possibly with assignment)")
				continue
			}
			check.openScope(clause, "case")
			if clause.Comm != nil {
				check.stmt(inner, clause.Comm)
			}
			check.stmtList(inner, clause.Body)
			check.closeScope()
		}

	case *syntax.ForStmt:
		inner |= breakOk | continueOk

		if rclause, _ := s.Init.(*syntax.RangeClause); rclause != nil {
			// extract sKey, sValue, s.Extra from the range clause
			sKey := rclause.Lhs            // possibly nil
			var sValue, sExtra syntax.Expr // possibly nil
			if p, _ := sKey.(*syntax.ListExpr); p != nil {
				if len(p.ElemList) < 2 {
					check.error(s, InvalidSyntaxTree, "invalid lhs in range clause")
					return
				}
				// len(p.ElemList) >= 2
				sKey = p.ElemList[0]
				sValue = p.ElemList[1]
				if len(p.ElemList) > 2 {
					// delay error reporting until we know more
					sExtra = p.ElemList[2]
				}
			}
			check.rangeStmt(inner, s, s, sKey, sValue, sExtra, rclause.X, rclause.Def)
			break
		}

		check.openScope(s, "for")
		defer check.closeScope()

		check.simpleStmt(s.Init)
		if s.Cond != nil {
			var x operand
			check.expr(nil, &x, s.Cond)
			if x.mode != invalid && !allBoolean(x.typ) {
				check.error(s.Cond, InvalidCond, "non-boolean condition in for statement")
			}
		}
		check.simpleStmt(s.Post)
		// spec: "The init statement may be a short variable
		// declaration, but the post statement must not."
		if s, _ := s.Post.(*syntax.AssignStmt); s != nil && s.Op == syntax.Def {
			// The parser already reported an error.
			check.use(s.Lhs) // avoid follow-up errors
		}
		check.stmt(inner, s.Body)

	default:
		check.error(s, InvalidSyntaxTree, "invalid statement")
	}
}

func (check *Checker) switchStmt(inner stmtContext, s *syntax.SwitchStmt) {
	// init statement already handled

	var x operand
	if s.Tag != nil {
		check.expr(nil, &x, s.Tag)
		// By checking assignment of x to an invisible temporary
		// (as a compiler would), we get all the relevant checks.
		check.assignment(&x, nil, "switch expression")
		if x.mode != invalid && !Comparable(x.typ) && !hasNil(x.typ) {
			check.errorf(&x, InvalidExprSwitch, "cannot switch on %s (%s is not comparable)", &x, x.typ)
			x.mode = invalid
		}
	} else {
		// spec: "A missing switch expression is
		// equivalent to the boolean value true."
		x.mode = constant_
		x.typ = Typ[Bool]
		x.val = constant.MakeBool(true)
		// TODO(gri) should have a better position here
		pos := s.Rbrace
		if len(s.Body) > 0 {
			pos = s.Body[0].Pos()
		}
		x.expr = syntax.NewName(pos, "true")
	}

	check.multipleSwitchDefaults(s.Body)

	seen := make(valueMap) // map of seen case values to positions and types
	for i, clause := range s.Body {
		if clause == nil {
			check.error(clause, InvalidSyntaxTree, "incorrect expression switch case")
			continue
		}
		inner := inner
		if i+1 < len(s.Body) {
			inner |= fallthroughOk
		} else {
			inner |= finalSwitchCase
		}
		check.caseValues(&x, syntax.UnpackListExpr(clause.Cases), seen)
		check.openScope(clause, "case")
		check.stmtList(inner, clause.Body)
		check.closeScope()
	}
}

func (check *Checker) typeSwitchStmt(inner stmtContext, s *syntax.SwitchStmt, guard *syntax.TypeSwitchGuard) {
	// init statement already handled

	// A type switch guard must be of the form:
	//
	//     TypeSwitchGuard = [ identifier ":=" ] PrimaryExpr "." "(" "type" ")" .
	//                          \__lhs__/        \___rhs___/

	// check lhs, if any
	lhs := guard.Lhs
	if lhs != nil {
		if lhs.Value == "_" {
			// _ := x.(type) is an invalid short variable declaration
			check.softErrorf(lhs, NoNewVar, "no new variable on left side of :=")
			lhs = nil // avoid declared and not used error below
		} else {
			check.recordDef(lhs, nil) // lhs variable is implicitly declared in each cause clause
		}
	}

	// check rhs
	var sx *operand // switch expression against which cases are compared against; nil if invalid
	{
		var x operand
		check.expr(nil, &x, guard.X)
		if x.mode != invalid {
			if isTypeParam(x.typ) {
				check.errorf(&x, InvalidTypeSwitch, "cannot use type switch on type parameter value %s", &x)
			} else if IsInterface(x.typ) {
				sx = &x
			} else {
				check.errorf(&x, InvalidTypeSwitch, "%s is not an interface", &x)
			}
		}
	}

	check.multipleSwitchDefaults(s.Body)

	var lhsVars []*Var                 // list of implicitly declared lhs variables
	seen := make(map[Type]syntax.Expr) // map of seen types to positions
	for _, clause := range s.Body {
		if clause == nil {
			check.error(s, InvalidSyntaxTree, "incorrect type switch case")
			continue
		}
		// Check each type in this type switch case.
		cases := syntax.UnpackListExpr(clause.Cases)
		T := check.caseTypes(sx, cases, seen)
		check.openScope(clause, "case")
		// If lhs exists, declare a corresponding variable in the case-local scope.
		if lhs != nil {
			obj := newVar(LocalVar, lhs.Pos(), check.pkg, lhs.Value, T)
			check.declare(check.scope, nil, obj, clause.Colon)
			check.recordImplicit(clause, obj)
			// For the "declared and not used" error, all lhs variables act as
			// one; i.e., if any one of them is 'used', all of them are 'used'.
			// Collect them for later analysis.
			lhsVars = append(lhsVars, obj)
		}
		check.stmtList(inner, clause.Body)
		check.closeScope()
	}

	// If lhs exists, we must have at least one lhs variable that was used.
	// (We can't use check.usage because that only looks at one scope; and
	// we don't want to use the same variable for all scopes and change the
	// variable type underfoot.)
	if lhs != nil {
		var used bool
		for _, v := range lhsVars {
			if check.usedVars[v] {
				used = true
			}
			check.usedVars[v] = true // avoid usage error when checking entire function
		}
		if !used {
			check.softErrorf(lhs, UnusedVar, "%s declared and not used", lhs.Value)
		}
	}
}
