// Package eval handles evaluation of nodes and consists the runtime of the
// shell.
package eval

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/elves/elvish/errutil"
	"github.com/elves/elvish/parse"
	"github.com/elves/elvish/store"
)

const FnPrefix = "fn-"

// ns is a namespace.
type ns map[string]Variable

// Evaler is used to evaluate elvish sources. It maintains runtime context
// shared among all evalCtx instances.
type Evaler struct {
	global      ns
	mod         map[string]ns
	searchPaths []string
	store       *store.Store
}

// evalCtx maintains an Evaler along with its runtime context. After creation
// an evalCtx is not modified, and new instances are created when needed.
type evalCtx struct {
	*Evaler
	name, text, context string

	local, up ns
	ports     []*port
}

func HasFailure(vs []Value) bool {
	for _, v := range vs {
		v, ok := v.(exitus)
		if !ok {
			// Silently ignore non-exitus values
			continue
		}
		if v.Sort != Ok {
			return true
		}
	}
	return false
}

// NewEvaler creates a new Evaler.
func NewEvaler(st *store.Store, dataDir string) *Evaler {
	// Construct initial global namespace
	pid := str(strconv.Itoa(syscall.Getpid()))
	global := ns{
		"pid":   newInternalVariable(pid),
		"ok":    newInternalVariable(ok),
		"true":  newInternalVariable(boolean(true)),
		"false": newInternalVariable(boolean(false)),
	}
	for _, b := range builtinFns {
		global[FnPrefix+b.Name] = newInternalVariable(b)
	}

	// Construct searchPaths
	var searchPaths []string
	if path := os.Getenv("PATH"); path != "" {
		searchPaths = strings.Split(path, ":")
	} else {
		searchPaths = []string{"/bin"}
	}

	return &Evaler{global, map[string]ns{}, searchPaths, st}
}

func printExitus(e exitus) {
	switch e.Sort {
	case Ok:
		fmt.Print("\033[32mok\033[m")
	case Failure:
		fmt.Print("\033[31;1m" + e.Failure + "\033[m")
	case Traceback:
		fmt.Print("(")
		for i, c := range e.Traceback.causes {
			if i > 0 {
				fmt.Print(" | ")
			}
			printExitus(c)
		}
		fmt.Print(")")
	default:
		// Control flow sorts
		fmt.Print("\033[33m" + flowExitusNames[e.Sort] + "\033[m")
	}
}

func PrintExituses(vs []Value) {
	if !HasFailure(vs) {
		return
	}
	fmt.Print("Status: ")
	for i, v := range vs {
		if i > 0 {
			fmt.Print(" | ")
		}
		printExitus(v.(exitus))
	}
	fmt.Println()
}

const (
	outChanSize   = 32
	outChanLeader = "▶ "
)

// newTopEvalCtx creates a top-level evalCtx.
func newTopEvalCtx(ev *Evaler, name, text string) *evalCtx {
	ch := make(chan Value, outChanSize)
	go func() {
		for v := range ch {
			fmt.Printf("%s%s\n", outChanLeader, v.Repr())
		}
	}()

	return &evalCtx{
		ev,
		name, text, "top",
		ev.global, ns{},
		[]*port{{f: os.Stdin},
			{f: os.Stdout, ch: ch, closeCh: true}, {f: os.Stderr}},
	}
}

// copy returns a copy of ec. The ports are copied deeply, with shouldClose
// flags reset, and the context is changed to the given value. Other fields are
// copied shallowly.
func (ec *evalCtx) copy(newContext string) *evalCtx {
	newPorts := make([]*port, len(ec.ports))
	for i, p := range ec.ports {
		newPorts[i] = &port{p.f, p.ch, false, false}
	}
	return &evalCtx{
		ec.Evaler,
		ec.name, ec.text, newContext,
		ec.local, ec.up,
		newPorts,
	}
}

// port returns ec.ports[i] or nil if i is out of range. This makes it possible
// to treat ec.ports as if it has an infinite tail of nil's.
func (ec *evalCtx) port(i int) *port {
	if i >= len(ec.ports) {
		return nil
	}
	return ec.ports[i]
}

// growPorts makes the size of ec.ports at least n, adding nil's if necessary.
func (ec *evalCtx) growPorts(n int) {
	if len(ec.ports) >= n {
		return
	}
	ports := ec.ports
	ec.ports = make([]*port, n)
	copy(ec.ports, ports)
}

func makeScope(s ns) scope {
	sc := scope{}
	for name, _ := range s {
		sc[name] = true
	}
	return sc
}

// Eval evaluates a chunk node n. The supplied name and text are used in
// diagnostic messages.
func (ev *Evaler) Eval(name, text string, n *parse.Chunk) ([]Value, error) {
	return ev.evalWithOut(name, text, n, nil)
}

func (ev *Evaler) evalWithOut(name, text string, n *parse.Chunk, out *port) ([]Value, error) {
	op, err := compile(name, text, makeScope(ev.global), n)
	if err != nil {
		return nil, err
	}
	ec := newTopEvalCtx(ev, name, text)
	if out != nil {
		ec.ports[1] = out
	}
	return ec.eval(op)
}

// eval evaluates an Op.
func (ec *evalCtx) eval(op valuesOp) (vs []Value, err error) {
	if op == nil {
		return nil, nil
	}
	defer ec.closePorts()
	defer errutil.Catch(&err)
	vs = op(ec)
	return vs, nil
}

// errorf stops the ec.eval immediately by panicking with a diagnostic message.
// The panic is supposed to be caught by ec.eval.
func (ec *evalCtx) errorf(p int, format string, args ...interface{}) {
	errutil.Throw(errutil.NewContextualError(
		fmt.Sprintf("%s (%s)", ec.name, ec.context), "error",
		ec.text, p, format, args...))
}

// mustSingleString returns a String if that is the only element of vs.
// Otherwise it errors.
func (ec *evalCtx) mustSingleString(vs []Value, what string, p int) str {
	if len(vs) != 1 {
		ec.errorf(p, "Expect exactly one word for %s, got %d", what, len(vs))
	}
	v, ok := vs[0].(str)
	if !ok {
		ec.errorf(p, "Expect string for %s, got %s", what, vs[0])
	}
	return v
}

// SourceText evaluates a chunk of elvish source.
func (ev *Evaler) SourceText(name, src, dir string) ([]Value, error) {
	n, err := parse.Parse(name, src)
	if err != nil {
		return nil, err
	}
	return ev.Eval(name, src, n)
}

func readFileUTF8(fname string) (string, error) {
	bytes, err := ioutil.ReadFile(fname)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(bytes) {
		return "", fmt.Errorf("%s: source is not valid UTF-8", fname)
	}
	return string(bytes), nil
}

// Source evaluates the content of a file.
func (ev *Evaler) Source(fname string) ([]Value, error) {
	src, err := readFileUTF8(fname)
	if err != nil {
		return nil, err
	}
	return ev.SourceText(fname, src, path.Dir(fname))
}

// Global returns the global namespace.
func (ev *Evaler) Global() ns {
	return ev.global
}

// ResolveVar resolves a variable. When the variable cannot be found, nil is
// returned.
func (ec *evalCtx) ResolveVar(ns, name string) Variable {
	if ns == "env" {
		return newEnvVariable(name)
	}
	if mod, ok := ec.mod[ns]; ok {
		return mod[name]
	}

	may := func(n string) bool {
		return ns == "" || ns == n
	}
	if may("local") {
		if v, ok := ec.local[name]; ok {
			return v
		}
	}
	if may("up") {
		if v, ok := ec.up[name]; ok {
			return v
		}
	}
	return nil
}
