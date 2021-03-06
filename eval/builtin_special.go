package eval

// Builtin special forms. Special forms behave mostly like ordinary commands -
// they are valid commands syntactically, and can take part in pipelines - but
// they have special rules for the evaluation of their arguments and can affect
// the compilation phase (whereas ordinary commands can only affect the
// evaluation phase).
//
// For instance, the "and" special form evaluates its arguments from left to
// right, and stops as soon as one booleanly false value is obtained: the
// command "and $false (fail haha)" does not produce an exception.
//
// As another instance, the "del" special form removes a variable, affecting the
// compiler.
//
// Flow control structures are also implemented as special forms in elvish, with
// closures functioning as code blocks.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/elves/elvish/eval/types"
	"github.com/elves/elvish/eval/vartypes"
	"github.com/elves/elvish/parse"
)

type compileBuiltin func(*compiler, *parse.Form) OpFunc

var (
	// ErrNoLibDir is thrown by "use" when the Evaler does not have a library
	// directory.
	ErrNoLibDir = errors.New("Evaler does not have a lib directory")
	// ErrRelativeUseNotFromMod is thrown by "use" when relative use is used
	// not from a module
	ErrRelativeUseNotFromMod = errors.New("Relative use not from module")
	// ErrRelativeUseGoesOutsideLib is thrown when a relative use goes out of
	// the library directory.
	ErrRelativeUseGoesOutsideLib = errors.New("Module outside library directory")
)

var builtinSpecials map[string]compileBuiltin

// IsBuiltinSpecial is the set of all names of builtin special forms. It is
// intended for external consumption, e.g. the syntax highlighter.
var IsBuiltinSpecial = map[string]bool{}

func init() {
	// Needed to avoid initialization loop
	builtinSpecials = map[string]compileBuiltin{
		"del":   compileDel,
		"fn":    compileFn,
		"use":   compileUse,
		"and":   compileAnd,
		"or":    compileOr,
		"if":    compileIf,
		"while": compileWhile,
		"for":   compileFor,
		"try":   compileTry,
	}
	for name := range builtinSpecials {
		IsBuiltinSpecial[name] = true
	}
}

const delArgMsg = "arguments to del must be variable or variable elements"

// DelForm = 'del' { VariablePrimary }
func compileDel(cp *compiler, fn *parse.Form) OpFunc {
	var ops []Op
	for _, cn := range fn.Args {
		cp.compiling(cn)
		if len(cn.Indexings) != 1 {
			cp.errorf(delArgMsg)
			continue
		}
		head, indicies := cn.Indexings[0].Head, cn.Indexings[0].Indicies
		if head.Type != parse.Bareword {
			if head.Type == parse.Variable {
				cp.errorf("arguments to del must drop $")
			} else {
				cp.errorf(delArgMsg)
			}
			continue
		}

		explode, ns, name := ParseVariable(head.Value)
		if explode {
			cp.errorf("arguments to del may be have a leading @")
			continue
		}
		var f OpFunc
		if len(indicies) == 0 {
			switch ns {
			case "", "local":
				if !cp.thisScope().has(name) {
					cp.errorf("no variable $%s in local scope", name)
					continue
				}
				cp.thisScope().del(name)
				f = newDelLocalVariableOp(name)
			case "E":
				f = newDelEnvVariableOp(name)
			default:
				cp.errorf("only variables in local: or E: can be deleted")
				continue
			}
		} else {
			if !cp.registerVariableGet(ns, name) {
				cp.errorf("no variable $%s", head.Value)
				continue
			}
			f = newDelElementOp(ns, name, head.Begin(), head.End(), cp.arrayOps(indicies))
		}
		ops = append(ops, Op{f, cn.Begin(), cn.End()})
	}
	return func(f *Frame) {
		for _, op := range ops {
			op.Exec(f)
		}
	}
}

func newDelLocalVariableOp(name string) OpFunc {
	return func(f *Frame) { delete(f.local, name) }
}

func newDelElementOp(ns, name string, begin, headEnd int, indexOps []ValuesOp) OpFunc {
	ends := make([]int, len(indexOps)+1)
	ends[0] = headEnd
	for i, op := range indexOps {
		ends[i+1] = op.End
	}
	return func(f *Frame) {
		var indicies []types.Value
		for _, indexOp := range indexOps {
			indexValues := indexOp.Exec(f)
			if len(indexValues) != 1 {
				f.errorpf(indexOp.Begin, indexOp.End, "index must evaluate to a single value in argument to del")
			}
			indicies = append(indicies, indexValues[0])
		}
		err := vartypes.DelElement(f.ResolveVar(ns, name), indicies)
		if err != nil {
			if level := vartypes.GetElementErrorLevel(err); level >= 0 {
				f.errorpf(begin, ends[level], "%s", err.Error())
			}
			throw(err)
		}
	}
}

func newDelEnvVariableOp(name string) OpFunc {
	return func(*Frame) {
		maybeThrow(os.Unsetenv(name))
	}
}

// makeFnOp wraps an op such that a return is converted to an ok.
func makeFnOp(op Op) Op {
	return Op{func(ec *Frame) {
		err := ec.PEval(op)
		if err != nil && err.(*Exception).Cause != Return {
			// rethrow
			throw(err)
		}
	}, op.Begin, op.End}
}

// FnForm = 'fn' StringPrimary LambdaPrimary
//
// fn f []{foobar} is a shorthand for set '&'f = []{foobar}.
func compileFn(cp *compiler, fn *parse.Form) OpFunc {
	args := cp.walkArgs(fn)
	nameNode := args.next()
	varName := mustString(cp, nameNode, "must be a literal string") + FnSuffix
	bodyNode := args.nextMustLambda()
	args.mustEnd()

	cp.registerVariableSetQname(":" + varName)
	op := cp.lambda(bodyNode)

	return func(ec *Frame) {
		// Initialize the function variable with the builtin nop
		// function. This step allows the definition of recursive
		// functions; the actual function will never be called.
		ec.local[varName] = vartypes.NewPtr(&BuiltinFn{"<shouldn't be called>", nop})
		closure := op(ec)[0].(*Closure)
		closure.Op = makeFnOp(closure.Op)
		err := ec.local[varName].Set(closure)
		maybeThrow(err)
	}
}

// UseForm = 'use' StringPrimary
func compileUse(cp *compiler, fn *parse.Form) OpFunc {
	if len(fn.Args) == 0 {
		end := fn.Head.End()
		cp.errorpf(end, end, "lack module name")
	} else if len(fn.Args) >= 2 {
		cp.errorpf(fn.Args[1].Begin(), fn.Args[len(fn.Args)-1].End(), "superfluous argument(s)")
	}

	spec := mustString(cp, fn.Args[0], "should be a literal string")

	// When modspec = "a/b/c:d", modname is c:d, and modpath is a/b/c/d
	modname := spec[strings.LastIndexByte(spec, '/')+1:]
	modpath := strings.Replace(spec, ":", "/", -1)
	cp.thisScope().set(modname + NsSuffix)

	return func(ec *Frame) {
		use(ec, modname, modpath)
	}
}

func use(ec *Frame, modname, modpath string) {
	resolvedPath := ""
	if strings.HasPrefix(modpath, "./") || strings.HasPrefix(modpath, "../") {
		if ec.srcMeta.typ != SrcModule {
			throw(ErrRelativeUseNotFromMod)
		}
		// Resolve relative modpath.
		resolvedPath = filepath.Clean(filepath.Dir(ec.srcMeta.name) + "/" + modpath)
	} else {
		resolvedPath = filepath.Clean(modpath)
	}
	if strings.HasPrefix(resolvedPath, "../") {
		throw(ErrRelativeUseGoesOutsideLib)
	}

	// Put the just loaded module into local scope.
	ec.local[modname+NsSuffix] = vartypes.NewPtr(loadModule(ec, resolvedPath))
}

func loadModule(ec *Frame, name string) Ns {
	if ns, ok := ec.Evaler.modules[name]; ok {
		// Module already loaded.
		return ns
	}

	// Load the source.
	var path, code string

	if ec.libDir == "" {
		throw(ErrNoLibDir)
	}

	path = filepath.Join(ec.libDir, name+".elv")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		// File does not exist. Try loading from the table of builtin
		// modules.
		var ok bool
		if code, ok = ec.bundled[name]; ok {
			// Source is loaded. Do nothing more.
			path = "<builtin module>"
		} else {
			throw(fmt.Errorf("cannot load %s: %s does not exist", name, path))
		}
	} else {
		// File exists. Load it.
		code, err = readFileUTF8(path)
		maybeThrow(err)
	}

	n, err := parse.Parse(name, code)
	maybeThrow(err)

	// Make an empty scope to evaluate the module in.
	meta := NewModuleSource(name, path, code)
	modGlobal := Ns{}

	newEc := &Frame{
		ec.Evaler, meta,
		modGlobal, make(Ns),
		ec.ports,
		0, len(code), ec.addTraceback(), false,
	}

	op, err := newEc.Compile(n, meta)
	maybeThrow(err)

	// Load the namespace before executing. This avoids mutual and self use's to
	// result in an infinite recursion.
	ec.Evaler.modules[name] = modGlobal
	err = newEc.PEval(op)
	if err != nil {
		// Unload the namespace.
		delete(ec.modules, name)
		throw(err)
	}
	return modGlobal
}

// compileAnd compiles the "and" special form.
// The and special form evaluates arguments until a false-ish values is found
// and outputs it; the remaining arguments are not evaluated. If there are no
// false-ish values, the last value is output. If there are no arguments, it
// outputs $true, as if there is a hidden $true before actual arguments.
func compileAnd(cp *compiler, fn *parse.Form) OpFunc {
	return compileAndOr(cp, fn, true, false)
}

// compileOr compiles the "or" special form.
// The or special form evaluates arguments until a true-ish values is found and
// outputs it; the remaining arguments are not evaluated. If there are no
// true-ish values, the last value is output. If there are no arguments, it
// outputs $false, as if there is a hidden $false before actual arguments.
func compileOr(cp *compiler, fn *parse.Form) OpFunc {
	return compileAndOr(cp, fn, false, true)
}

func compileAndOr(cp *compiler, fn *parse.Form, init, stopAt bool) OpFunc {
	argOps := cp.compoundOps(fn.Args)
	return func(ec *Frame) {
		var lastValue types.Value = types.Bool(init)
		for _, op := range argOps {
			values := op.Exec(ec)
			for _, value := range values {
				if types.ToBool(value) == stopAt {
					ec.OutputChan() <- value
					return
				}
				lastValue = value
			}
		}
		ec.OutputChan() <- lastValue
	}
}

func compileIf(cp *compiler, fn *parse.Form) OpFunc {
	args := cp.walkArgs(fn)
	var condNodes []*parse.Compound
	var bodyNodes []*parse.Primary
	for {
		condNodes = append(condNodes, args.next())
		bodyNodes = append(bodyNodes, args.nextMustLambda())
		if !args.nextIs("elif") {
			break
		}
	}
	elseNode := args.nextMustLambdaIfAfter("else")
	args.mustEnd()

	condOps := cp.compoundOps(condNodes)
	bodyOps := cp.primaryOps(bodyNodes)
	var elseOp ValuesOp
	if elseNode != nil {
		elseOp = cp.primaryOp(elseNode)
	}

	return func(ec *Frame) {
		bodies := make([]Callable, len(bodyOps))
		for i, bodyOp := range bodyOps {
			bodies[i] = bodyOp.execlambdaOp(ec)
		}
		else_ := elseOp.execlambdaOp(ec)
		for i, condOp := range condOps {
			if allTrue(condOp.Exec(ec.fork("if cond"))) {
				bodies[i].Call(ec.fork("if body"), NoArgs, NoOpts)
				return
			}
		}
		if elseOp.Func != nil {
			else_.Call(ec.fork("if else"), NoArgs, NoOpts)
		}
	}
}

func compileWhile(cp *compiler, fn *parse.Form) OpFunc {
	args := cp.walkArgs(fn)
	condNode := args.next()
	bodyNode := args.nextMustLambda()
	args.mustEnd()

	condOp := cp.compoundOp(condNode)
	bodyOp := cp.primaryOp(bodyNode)

	return func(ec *Frame) {
		body := bodyOp.execlambdaOp(ec)

		for {
			cond := condOp.Exec(ec.fork("while cond"))
			if !allTrue(cond) {
				break
			}
			err := ec.fork("while").PCall(body, NoArgs, NoOpts)
			if err != nil {
				exc := err.(*Exception)
				if exc.Cause == Continue {
					// do nothing
				} else if exc.Cause == Break {
					continue
				} else {
					throw(err)
				}
			}
		}
	}
}

func compileFor(cp *compiler, fn *parse.Form) OpFunc {
	args := cp.walkArgs(fn)
	varNode := args.next()
	iterNode := args.next()
	bodyNode := args.nextMustLambda()
	elseNode := args.nextMustLambdaIfAfter("else")
	args.mustEnd()

	varOp, restOp := cp.lvaluesOp(varNode.Indexings[0])
	if restOp.Func != nil {
		cp.errorpf(restOp.Begin, restOp.End, "rest not allowed")
	}

	iterOp := cp.compoundOp(iterNode)
	bodyOp := cp.primaryOp(bodyNode)
	var elseOp ValuesOp
	if elseNode != nil {
		elseOp = cp.primaryOp(elseNode)
	}

	return func(ec *Frame) {
		variables := varOp.Exec(ec)
		if len(variables) != 1 {
			ec.errorpf(varOp.Begin, varOp.End, "only one variable allowed")
		}
		variable := variables[0]

		iterable := ec.ExecAndUnwrap("value being iterated", iterOp).One().Iterable()

		body := bodyOp.execlambdaOp(ec)
		elseBody := elseOp.execlambdaOp(ec)

		iterated := false
		iterable.Iterate(func(v types.Value) bool {
			iterated = true
			err := variable.Set(v)
			maybeThrow(err)
			err = ec.fork("for").PCall(body, NoArgs, NoOpts)
			if err != nil {
				exc := err.(*Exception)
				if exc.Cause == Continue {
					// do nothing
				} else if exc.Cause == Break {
					return false
				} else {
					throw(err)
				}
			}
			return true
		})

		if !iterated && elseBody != nil {
			elseBody.Call(ec.fork("for else"), NoArgs, NoOpts)
		}
	}
}

func compileTry(cp *compiler, fn *parse.Form) OpFunc {
	logger.Println("compiling try")
	args := cp.walkArgs(fn)
	bodyNode := args.nextMustLambda()
	logger.Printf("body is %q", bodyNode.SourceText())
	var exceptVarNode *parse.Indexing
	var exceptNode *parse.Primary
	if args.nextIs("except") {
		logger.Println("except-ing")
		n := args.peek()
		// Is this a variable?
		if len(n.Indexings) == 1 && n.Indexings[0].Head.Type == parse.Bareword {
			exceptVarNode = n.Indexings[0]
			args.next()
		}
		exceptNode = args.nextMustLambda()
	}
	elseNode := args.nextMustLambdaIfAfter("else")
	finallyNode := args.nextMustLambdaIfAfter("finally")
	args.mustEnd()

	var exceptVarOp LValuesOp
	var bodyOp, exceptOp, elseOp, finallyOp ValuesOp
	bodyOp = cp.primaryOp(bodyNode)
	if exceptVarNode != nil {
		var restOp LValuesOp
		exceptVarOp, restOp = cp.lvaluesOp(exceptVarNode)
		if restOp.Func != nil {
			cp.errorpf(restOp.Begin, restOp.End, "may not use @rest in except variable")
		}
	}
	if exceptNode != nil {
		exceptOp = cp.primaryOp(exceptNode)
	}
	if elseNode != nil {
		elseOp = cp.primaryOp(elseNode)
	}
	if finallyNode != nil {
		finallyOp = cp.primaryOp(finallyNode)
	}

	return func(ec *Frame) {
		body := bodyOp.execlambdaOp(ec)
		exceptVar := exceptVarOp.execMustOne(ec)
		except := exceptOp.execlambdaOp(ec)
		else_ := elseOp.execlambdaOp(ec)
		finally := finallyOp.execlambdaOp(ec)

		err := ec.fork("try body").PCall(body, NoArgs, NoOpts)
		if err != nil {
			if except != nil {
				if exceptVar != nil {
					err := exceptVar.Set(err.(*Exception))
					maybeThrow(err)
				}
				err = ec.fork("try except").PCall(except, NoArgs, NoOpts)
			}
		} else {
			if else_ != nil {
				err = ec.fork("try else").PCall(else_, NoArgs, NoOpts)
			}
		}
		if finally != nil {
			finally.Call(ec.fork("try finally"), NoArgs, NoOpts)
		}
		if err != nil {
			throw(err)
		}
	}
}

// execLambdaOp executes a ValuesOp that is known to yield a lambda and returns
// the lambda. If the ValuesOp is empty, it returns a nil.
func (op ValuesOp) execlambdaOp(ec *Frame) Callable {
	if op.Func == nil {
		return nil
	}

	return op.Exec(ec)[0].(Callable)
}

// execMustOne executes the LValuesOp and raises an exception if it does not
// evaluate to exactly one Variable. If the given LValuesOp is empty, it returns
// nil.
func (op LValuesOp) execMustOne(ec *Frame) vartypes.Variable {
	if op.Func == nil {
		return nil
	}
	variables := op.Exec(ec)
	if len(variables) != 1 {
		ec.errorpf(op.Begin, op.End, "should be one variable")
	}
	return variables[0]
}
