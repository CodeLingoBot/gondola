package template

import (
	"bytes"
	"fmt"
	"gnd.la/util/stringutil"
	"gnd.la/util/types"
	"html/template"
	"reflect"
	"strings"
	"text/template/parse"
)

var (
	errType          = reflect.TypeOf((*error)(nil)).Elem()
	stringType       = reflect.TypeOf("")
	htmlType         = reflect.TypeOf(HTML(""))
	templateHtmlType = reflect.TypeOf(template.HTML(""))
	jsType           = reflect.TypeOf(JS(""))
	templateJsType   = reflect.TypeOf(template.JS(""))
	stringerType     = reflect.TypeOf((*fmt.Stringer)(nil)).Elem()
	emptyType        = reflect.TypeOf((*interface{})(nil)).Elem()
	zero             = reflect.Zero(emptyType)
)

const (
	templateHtmlEscaper    = "html_template_htmlescaper"
	templateJsEscaper      = "html_template_jsvalescaper"
	templateCommentEscaper = "html_template_commentescaper"
)

// TODO: Remove variables inside if or with when exiting the scope

type opcode uint8

const (
	opNOP opcode = iota
	opFIELD
	opFUNC
	opITER
	opJMP
	opJMPF
	opJMPT
	opMARK
	opNEXT
	opDOT
	opPRINT
	opPUSHDOT
	opPOPDOT
	opPOP
	opSETVAR
	opSTRING
	opTEMPLATE
	opUNSETVAR
	opVAL
	opVAR
	opWB
)

type valType uint32

type inst struct {
	op  opcode
	val valType
}

type instructions []inst

func (i instructions) replace(idx int, count int, repl []inst) []inst {
	// look for jumps before the block which need to be adjusted
	for ii, v := range i[:idx] {
		switch v.op {
		case opJMP, opJMPT, opJMPF, opNEXT:
			val := int(int32(v.val))
			if ii+val > idx {
				i[ii] = inst{v.op, valType(int32(val + len(repl) - count))}
			}
		}
	}
	// look for jumps after the block which need to be adjusted
	start := idx + count
	for ii, v := range i[start:] {
		switch v.op {
		case opJMP, opJMPT, opJMPF, opNEXT:
			val := int(int32(v.val))
			if ii+val < 0 {
				i[ii+start] = inst{v.op, valType(int32(val - len(repl) + count))}
			}
		}
	}
	var ret []inst
	ret = append(ret, i[:idx]...)
	ret = append(ret, repl...)
	ret = append(ret, i[idx+count:]...)
	return ret
}

func encodeVal(high int, low valType) valType {
	return valType(high<<16) | low
}

func decodeVal(val valType) (int, int) {
	return int(val >> 16), int(val & 0xFFFF)
}

type iterator interface {
	Next() (bool, reflect.Value, reflect.Value)
}

type nilIterator struct {
}

func (it *nilIterator) Next() (bool, reflect.Value, reflect.Value) {
	return false, zero, zero
}

type sliceIterator struct {
	val    reflect.Value
	pos    int
	length int
}

func (it *sliceIterator) Next() (bool, reflect.Value, reflect.Value) {
	if it.pos < it.length {
		val := it.val.Index(it.pos)
		pos := reflect.ValueOf(it.pos)
		it.pos++
		return true, pos, val
	}
	return false, zero, zero
}

type mapIterator struct {
	val  reflect.Value
	keys []reflect.Value
	pos  int
}

func (it *mapIterator) Next() (bool, reflect.Value, reflect.Value) {
	if it.pos < len(it.keys) {
		k := it.keys[it.pos]
		val := it.val.MapIndex(k)
		it.pos++
		return true, k, val
	}
	return false, zero, zero
}

type chanIterator struct {
	val reflect.Value
	pos int
}

func (it *chanIterator) Next() (bool, reflect.Value, reflect.Value) {
	val, ok := it.val.Recv()
	if ok {
		pos := reflect.ValueOf(it.pos)
		it.pos++
		return true, pos, val
	}
	return false, zero, zero
}

func newIterator(v reflect.Value) (iterator, error) {
	for v.Kind() == reflect.Ptr || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return &nilIterator{}, nil
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		if v.Len() == 0 {
			return &nilIterator{}, nil
		}
		return &sliceIterator{val: v, length: v.Len()}, nil
	case reflect.Map:
		if v.Len() == 0 {
			return &nilIterator{}, nil
		}
		keys := v.MapKeys()
		types.SortValues(keys)
		return &mapIterator{val: v, keys: keys}, nil
	case reflect.Chan:
		if v.IsNil() {
			return &nilIterator{}, nil
		}
		return &chanIterator{val: v}, nil
	case reflect.Invalid:
		return &nilIterator{}, nil
	}
	return nil, fmt.Errorf("can't range over %T", v.Interface())
}

type variable struct {
	name  string
	value reflect.Value
}

type state struct {
	p         *program
	w         *bytes.Buffer
	vars      []variable
	stack     []reflect.Value
	marks     []int
	dot       []reflect.Value
	iterators []iterator
	scratch   []interface{}   // used for calling variadic functions with ...interface{}
	res       []reflect.Value // used for storing return values in fast paths
	resPtr    *reflect.Value
}

func newState(p *program, w *bytes.Buffer) *state {
	if s := getState(); s != nil {
		s.reset()
		s.p = p
		s.w = w
		return s
	}
	res := make([]reflect.Value, 1)
	resPtr := &res[0]
	return &state{
		p:      p,
		w:      w,
		res:    res,
		resPtr: resPtr,
	}
}

func (s *state) reset() {
	s.vars = s.vars[:0]
	s.stack = s.stack[:0]
	s.marks = s.marks[:0]
	s.dot = s.dot[:0]
	s.iterators = s.iterators[:0]
}

func (s *state) countDefines(name string, tr *parse.Tree, node parse.Node) int {
	search := []*Template{s.p.tmpl}
	search = append(search, s.p.tmpl.children...)
	for _, v := range s.p.tmpl.hooks {
		search = append(search, v.Template)
	}
	var ns string
	if ns = namespace(name); ns != "" {
		name = name[len(ns)+len(nsMark):]
	}
	var text string
	for _, v := range search {
		if v.Namespace() == ns {
			text = v.texts[name]
			if text != "" {
				break
			}
		}
	}
	pos := int(node.Position())
	if len(text) > pos {
		return len(defineRe.FindAllStringIndex(text[:pos], -1))
	}
	return 0
}

func (s *state) formatTreeErr(name string, tr *parse.Tree, node parse.Node, err error) error {
	loc, _ := tr.ErrorContext(node)
	if loc != "" {
		// Need to substract the number of lines prepended
		// which is 1 + (number of define nodes before this line)
		p := strings.SplitN(loc, ":", 2)
		if len(p) == 2 {
			var line int
			var pos int
			if _, err := fmt.Sscanf(p[1], "%d:%d", &line, &pos); err == nil {
				defines := s.countDefines(name, tr, node)
				line -= 1 + defines
				loc = fmt.Sprintf("%s:%d:%d", p[0], line, pos)
			}
		}
		err = fmt.Errorf("%s: %s", loc, err.Error())
	}
	return err
}

func (s *state) formatErr(pc int, tmpl string, err error) error {
	if p := strings.Index(tmpl, "$htmltemplate"); p >= 0 {
		// This is a mangled tree generated by html/template,
		// which has no text. Use the unmangled version instead.
		tmpl = tmpl[:p-1]
	}
	tr := s.p.tmpl.trees[tmpl]
	if tr != nil {
		ctx := s.p.context[tmpl]
		for _, v := range ctx {
			if v.pc >= pc {
				return s.formatTreeErr(tmpl, tr, v.node, err)
			}
		}
	}
	return err
}

func (s *state) errorf(pc int, tmpl string, format string, args ...interface{}) error {
	err := fmt.Errorf(format, args...)
	return s.formatErr(pc, tmpl, err)
}

func (s *state) requiresPointerErr(val reflect.Value, name string) error {
	if _, ok := reflect.PtrTo(val.Type()).MethodByName(name); ok {
		return fmt.Errorf("method %q requires pointer receiver (%s)", name, reflect.PtrTo(val.Type()))
	}
	return nil
}

func (s *state) pushVar(name string, value reflect.Value) {
	s.vars = append(s.vars, variable{name, value})
}

func (s *state) unsetVar(name string) error {
	for i := s.varMark() - 1; i >= 0; i-- {
		if s.vars[i].name == name {
			s.vars = append(s.vars[:i], s.vars[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("undefined variable: %q", name)
}

func (s *state) varMark() int {
	return len(s.vars)
}

func (s *state) popVar(mark int) {
	s.vars = s.vars[0:mark]
}

func (s *state) setVar(n int, value reflect.Value) {
	s.vars[len(s.vars)-n].value = value
}

func (s *state) varValue(name string) (reflect.Value, error) {
	for i := s.varMark() - 1; i >= 0; i-- {
		if s.vars[i].name == name {
			return s.vars[i].value, nil
		}
	}
	return reflect.Value{}, fmt.Errorf("undefined variable: %q", name)
}

// call fn, remove its args from the stack and push the result
func (s *state) call(fn reflect.Value, name string, args int, fp fastPath) error {
	pos := len(s.stack) - args
	in := s.stack[pos : pos+args]
	ftyp := fn.Type()
	numIn := ftyp.NumIn()
	last := numIn
	isVariadic := ftyp.IsVariadic()
	if isVariadic {
		last--
		if args < last {
			return fmt.Errorf("function %q requires at least %d arguments, %d given", name, last, args)
		}
	} else {
		if args != numIn {
			return fmt.Errorf("function %q requires exactly %d arguments, %d given", name, numIn, args)
		}
	}
	// arguments are in reverse order
	for ii := 0; ii < len(in)/2; ii++ {
		in[ii], in[len(in)-1-ii] = in[len(in)-1-ii], in[ii]
	}
	var lastType reflect.Type
	for ii, v := range in {
		var ityp reflect.Type
		if ii < last {
			ityp = ftyp.In(ii)
		} else {
			ityp = ftyp.In(last).Elem()
			lastType = ityp
		}
		if !v.IsValid() {
			in[ii] = reflect.Zero(ityp)
			continue
		}
		vtyp := v.Type()
		if !vtyp.AssignableTo(ityp) {
			k := vtyp.Kind()
			if (k == reflect.Ptr || k == reflect.Interface) && !v.IsNil() && vtyp.Elem().AssignableTo(ityp) {
				in[ii] = v.Elem()
				continue
			}
			if reflect.PtrTo(vtyp).AssignableTo(ityp) && v.CanAddr() {
				in[ii] = v.Addr()
				continue
			}
			return fmt.Errorf("can't call %q with %s as argument %d, need %s", name, vtyp, ii+1, ityp)
		}
	}
	var res []reflect.Value
	if isVariadic && lastType == emptyType {
		// empty the scratch here, so it's only pointless
		// the first time. the alternative would be emptying it
		// after the call and in reset(), because CallSlice can end
		// up calling a function which panics. that would cause
		// a lot of unrequired emptys because templates don't
		// necessarily use the scratch.
		s.scratch = s.scratch[:0]
		for _, v := range in[last:] {
			s.scratch = append(s.scratch, v.Interface())
		}
		if fp != nil {
			if err := fp(in, s.scratch, s.resPtr); err != nil {
				return fmt.Errorf("%q returned an error: %s", name, err)
			}
			res = s.res
		} else {
			in[last] = reflect.ValueOf(s.scratch)
			in = in[:last+1]
			res = fn.CallSlice(in)
		}
	} else {
		res = fn.Call(in)
	}
	if len(res) == 2 && !res[1].IsNil() {
		return fmt.Errorf("%q returned an error: %s", name, res[1].Interface())
	}
	s.stack = append(s.stack[:pos], stackable(res[0]))
	return nil
}

func (s *state) recover(pc *int, tmpl *string, err *error) {
	if r := recover(); r != nil {
		e, ok := r.(error)
		if !ok {
			e = fmt.Errorf("%v", r)
		}
		*err = s.formatErr(*pc, *tmpl, e)
	}
}

func (s *state) execute(tmpl string, ns string, dot reflect.Value) (err error) {
	code := s.p.code[tmpl]
	s.pushVar("", dot)
	if ns != "" {
		if vars, err := s.varValue("Vars"); err == nil {
			if !vars.IsNil() {
				s.pushVar("Vars", reflect.ValueOf(vars.Interface().(VarMap).unpack(ns)))
			}
		}
	}
	var pc int
	defer s.recover(&pc, &tmpl, &err)
	for pc = 0; pc < len(code); pc++ {
		v := code[pc]
		switch v.op {
		case opMARK:
			s.marks = append(s.marks, len(s.stack))
		case opPOP:
			if v.val == 0 {
				// POP until mark
				// if there's no mark, let it crash, it must be a bug
				// and it will be easier to find
				p := len(s.marks) - 1
				s.stack = s.stack[:s.marks[p]]
				s.marks = s.marks[:p]
			} else {
				s.stack = s.stack[:len(s.stack)-int(v.val)]
			}
		case opFIELD:
			res := zero
			p := len(s.stack) - 1
			top := s.stack[p]
			args, i := decodeVal(v.val)
			if top.IsValid() {
				if top.Kind() == reflect.Map && (top.Type().Key().Kind() == reflect.String || stringType.AssignableTo(top.Type().Key())) {
					if !top.IsNil() {
						k := s.p.rstrings[i]
						res = stackable(top.MapIndex(k))
					}
				} else {
					name := s.p.strings[i]
					// get pointer methods and try to call a method by that name
					ptr := top
					kind := ptr.Kind()
					if kind != reflect.Interface && kind != reflect.Ptr && ptr.CanAddr() {
						ptr = ptr.Addr()
						kind = reflect.Ptr
					}
					fn := ptr.MethodByName(name)
					if !fn.IsValid() && kind == reflect.Ptr && ptr.Type().Elem().Kind() == reflect.Interface {
						ptr = ptr.Elem()
						fn = ptr.MethodByName(name)
					}
					if fn.IsValid() {
						// when calling a function from a field, there will be
						// and extra argument at the top of the stack, either
						// the dot or the result of the last field lookup, so
						// we have to remove it.
						s.stack = s.stack[:p]
						if err := s.call(fn, name, args, nil); err != nil {
							return err
						}
						// s.call already puts the result in the stack
						break
					}
					// try to get a field by that name
					for top.Kind() == reflect.Ptr || top.Kind() == reflect.Interface {
						if top.IsNil() {
							// nil pointer or interface, put a nil on the stack.
							// this is different from Go templates, which return an
							// error when evaluating nil
							s.stack[p] = res
							break
						}
						top = top.Elem()
					}
					if top.Kind() != reflect.Struct {
						if err := s.requiresPointerErr(top, name); err != nil {
							return s.formatErr(pc, tmpl, err)
						}
						return s.errorf(pc, tmpl, "can't evaluate field on type %T", top.Interface())
					}
					res = top.FieldByName(name)
					if !res.IsValid() {
						if err := s.requiresPointerErr(top, name); err != nil {
							return s.formatErr(pc, tmpl, err)
						}
						return s.errorf(pc, tmpl, "%q is not a field of struct type %T", name, top.Interface())
					}
				}
			}
			// opFIELD overwrites the stack
			s.stack[p] = res
		case opFUNC:
			args, i := decodeVal(v.val)
			// function existence is checked at compile time
			fn := s.p.funcs[i]
			if err := s.call(fn.val, fn.name, args, fn.fp); err != nil {
				return s.formatErr(pc, tmpl, err)
			}
		case opVAR:
			name := s.p.strings[int(v.val)]
			v, err := s.varValue(name)
			if err != nil {
				return s.formatErr(pc, tmpl, err)
			}
			s.stack = append(s.stack, v)
		case opDOT:
			s.stack = append(s.stack, dot)
		case opITER:
			iter, err := newIterator(s.stack[len(s.stack)-1])
			if err != nil {
				return s.formatErr(pc, tmpl, err)
			}
			s.iterators = append(s.iterators, iter)
		case opNEXT:
			p := len(s.iterators) - 1
			// let it crash if there are no iterators, it would
			// be a compiler error
			iter := s.iterators[p]
			next, idx, val := iter.Next()
			if next {
				s.stack = append(s.stack, idx, val)
			} else {
				s.iterators = s.iterators[:p]
				pc += int(int32(v.val))
			}
		case opJMP:
			pc += int(int32(v.val))
		case opJMPF:
			p := len(s.stack)
			if p == 0 || !isTrue(s.stack[p-1]) {
				pc += int(int32(v.val))
			}
		case opSETVAR:
			name := s.p.strings[int(v.val)]
			p := len(s.stack) - 1
			s.pushVar(name, s.stack[p])
			// SETVAR pops
			s.stack = s.stack[:p]
		case opUNSETVAR:
			name := s.p.strings[int(v.val)]
			if err := s.unsetVar(name); err != nil {
				return err
			}
		case opTEMPLATE:
			n, t := decodeVal(v.val)
			name := s.p.strings[t]
			ns := s.p.strings[n]
			mark := s.varMark()
			dupDot := s.stack[len(s.stack)-1]
			err := s.execute(name, ns, dupDot)
			if err != nil {
				// execute already returns the formatted error
				return err
			}
			s.vars = s.vars[:mark]
		case opVAL:
			s.stack = append(s.stack, s.p.values[v.val])
		case opJMPT:
			p := len(s.stack)
			if p > 0 && isTrue(s.stack[p-1]) {
				pc += int(int32(v.val))
			}
		case opPRINT:
			v := s.stack[len(s.stack)-1]
			if v.IsValid() && v.Type() == stringType {
				if _, err := s.w.WriteString(v.String()); err != nil {
					return s.formatErr(pc, tmpl, err)
				}
				break
			}
			val, doPrint, ok := printableValue(v)
			if !ok {
				return s.errorf(pc, tmpl, "can't print value of type %s", v.Type())
			}
			if doPrint {
				if _, err := fmt.Fprint(s.w, val); err != nil {
					return s.formatErr(pc, tmpl, err)
				}
			}
		case opPUSHDOT:
			s.dot = append(s.dot, dot)
			dot = s.stack[len(s.stack)-1]
		case opPOPDOT:
			p := len(s.dot) - 1
			dot = s.dot[p]
			s.dot = s.dot[:p]
		case opSTRING:
			s.stack = append(s.stack, s.p.rstrings[int(v.val)])
		case opWB:
			if _, err := s.w.Write(s.p.bs[int(v.val)]); err != nil {
				return s.formatErr(pc, tmpl, err)
			}
		default:
			return s.errorf(pc, tmpl, "invalid opcode %d", v.op)
		}
	}
	return nil
}

type fn struct {
	name     string
	val      reflect.Value
	variadic bool
	numIn    int
	fp       fastPath
}

func newFn(v reflect.Value, name string) *fn {
	typ := v.Type()
	variadic := typ.IsVariadic()
	numIn := typ.NumIn()
	return &fn{
		name:     name,
		val:      v,
		variadic: variadic,
		numIn:    numIn,
		fp:       newFastPath(v),
	}
}

type context struct {
	pc   int
	node parse.Node
}

type scratch struct {
	name       string
	buf        []inst
	cmd        []int
	pipe       []int
	ctx        []*context
	noPrint    bool
	branchPipe bool
}

// snap returns a *scratch with the buf amd ctx of this
// scratch, while setting this scratch's buf and
// ctx to nil. used while compiling branches
func (s *scratch) snap() *scratch {
	ret := &scratch{buf: s.buf, ctx: s.ctx}
	s.buf = nil
	s.ctx = nil
	return ret
}

// restore sets the scrach buf and ctx to p.buf and p.ctx
func (s *scratch) restore(p *scratch) {
	s.buf = p.buf
	s.ctx = p.ctx
}

// prepend prepends one instruction to the scrach, adjusting
// the values in ctx
func (s *scratch) prepend(op opcode, val valType) *scratch {
	s.buf = append([]inst{{op: op, val: val}}, s.buf...)
	for _, v := range s.ctx {
		v.pc++
	}
	return s
}

func (s *scratch) popFront(count int) *scratch {
	s.buf = s.buf[count:]
	for _, v := range s.ctx {
		v.pc -= count
	}
	return s
}

func (s *scratch) append(op opcode, val valType) *scratch {
	s.buf = append(s.buf, inst{op: op, val: val})
	return s
}

// add adds another scratch at the end of this one. ctx in p
// is adjusted.
func (s *scratch) add(p *scratch) {
	c := len(s.buf)
	if c > 0 {
		for _, v := range p.ctx {
			v.pc += c
		}
	}
	s.buf = append(s.buf, p.buf...)
	s.ctx = append(s.ctx, p.ctx...)
}

func (s *scratch) marksAndPops() bool {
	if len(s.buf) > 0 {
		if last := s.buf[len(s.buf)-1]; last.op == opPOP && last.val == 0 {
			return true
		}
	}
	return false
}

func (s *scratch) pushes() int {
	if s.marksAndPops() {
		return 0
	}
	pushes := 0
	for _, v := range s.buf {
		switch v.op {
		case opSTRING, opDOT, opVAL, opVAR, opFUNC:
			pushes++
		case opNEXT:
			pushes += 2
		case opFIELD:
			// can't know in advance, since it might
			// overwrite when there's a lookup or
			// might reduce the number of arguments
			// if it's a function call
			return -1
		}
	}
	return pushes
}

func (s *scratch) pops() int {
	if s.marksAndPops() {
		return 0
	}
	pops := 0
	for _, v := range s.buf {
		switch v.op {
		case opPOP:
			pops += int(v.val)
		case opSETVAR:
			pops++
		case opFUNC:
			args, _ := decodeVal(v.val)
			pops += args
		}
	}
	return pops
}

func (s *scratch) putPop() error {
	pushes := s.pushes()
	if pushes == -1 {
		// can't know in advance, have to mark
		s.prepend(opMARK, 0).append(opPOP, 0)
	} else {
		if delta := pushes - s.pops(); delta > 0 {
			s.append(opPOP, valType(delta))
		} else if delta < 0 {
			return fmt.Errorf("can't POP more than you PUSH!! %d", delta)
		}
	}
	return nil
}

func (s *scratch) takePop() int {
	if len(s.buf) > 0 {
		p := len(s.buf) - 1
		last := s.buf[p]
		if last.op == opPOP {
			s.buf = s.buf[:p]
			return int(last.val)
		}
	}
	return -1
}

func (s *scratch) addPop(pop int) {
	if pop >= 0 {
		s.append(opPOP, valType(pop))
	}
}

// argc returns the number of arguments for the
// function/field being parsed
func (s *scratch) argc() int {
	var argc int
	if len(s.cmd) > 0 {
		argc = s.cmd[len(s.cmd)-1]
		if len(s.pipe) > 0 && s.pipe[len(s.pipe)-1] > 0 {
			argc++
		}
	}
	return argc
}

func (s *scratch) clear() {
	s.buf = nil
	s.ctx = nil
}

type program struct {
	tmpl     *Template
	funcs    []*fn
	strings  []string
	rstrings []reflect.Value
	values   []reflect.Value
	bs       [][]byte
	code     map[string][]inst
	context  map[string][]*context
	// used only during compilation
	s *scratch
}

func (p *program) inst(op opcode, val valType) {
	p.s.buf = append(p.s.buf, inst{op: op, val: val})
}

func (p *program) addString(s string) valType {
	for ii, v := range p.strings {
		if v == s {
			return valType(ii)
		}
	}
	p.strings = append(p.strings, s)
	p.rstrings = append(p.rstrings, reflect.ValueOf(s))
	return valType(len(p.strings) - 1)
}

func (p *program) addFunc(f interface{}, name string) valType {
	for ii, v := range p.funcs {
		if v.name == name {
			return valType(ii)
		}
	}
	// TODO: Check it's really a reflect.Func
	val := reflect.ValueOf(f)
	p.funcs = append(p.funcs, newFn(val, name))
	return valType(len(p.funcs) - 1)
}

func (p *program) addValue(v interface{}) valType {
	p.values = append(p.values, reflect.ValueOf(v))
	return valType(len(p.values) - 1)
}

func (p *program) addWB(b []byte) {
	pos := len(p.bs)
	p.bs = append(p.bs, b)
	p.inst(opWB, valType(pos))
}

func (p *program) addSTRING(s string) {
	p.inst(opSTRING, p.addString(s))
}

func (p *program) addFIELD(argc int, name string) {
	if argc < 0 {
		argc = p.s.argc()
	}
	p.inst(opFIELD, encodeVal(argc, p.addString(name)))
}

func (p *program) addFIELDs(names []string) {
	argc := 0
	for ii, v := range names {
		if ii == len(names)-1 {
			argc = -1
		}
		p.addFIELD(argc, v)
	}
}

func (p *program) prevFuncReturnType() reflect.Type {
	if len(p.s.buf) > 0 {
		if in := p.s.buf[len(p.s.buf)-1]; in.op == opFUNC {
			_, i := decodeVal(in.val)
			fn := p.funcs[i]
			return fn.val.Type().Out(0)
		}
	}
	return nil
}

func (p *program) walkBranch(nt parse.NodeType, b *parse.BranchNode) error {
	saved := p.s.snap()
	p.s.branchPipe = true
	if err := p.walk(b.Pipe); err != nil {
		return err
	}
	p.s.branchPipe = false
	pipe := p.s.snap()
	if err := p.walk(b.List); err != nil {
		return err
	}
	list := p.s.snap()
	var elseList *scratch
	if b.ElseList != nil {
		if err := p.walk(b.ElseList); err != nil {
			return err
		}
		// if the else is empty, just ignore it
		if len(p.s.buf) > 0 {
			elseList = p.s.snap()
		}
	}
	p.s.restore(saved)
	if err := pipe.putPop(); err != nil {
		return err
	}
	pop := pipe.takePop()
	p.s.add(pipe)
	if nt != parse.NodeRange {
		if len(b.Pipe.Decl) > 0 {
			if elseList != nil {
				elseList.addPop(pop)
			}
			pop = -1
			val := p.addString(b.Pipe.Decl[0].Ident[0][1:])
			list.prepend(opSETVAR, val).append(opUNSETVAR, val)
		}
	}
	skip := len(list.buf)
	if elseList != nil {
		// Skip the JMP at the start of the elseList
		skip += 1
	}
	switch nt {
	case parse.NodeIf:
		p.inst(opJMPF, valType(skip))
		p.s.add(list)
	case parse.NodeWith:
		// if false, skip the PUSHDOT and POPDOT
		p.inst(opJMPF, valType(skip+2))
		p.inst(opPUSHDOT, 0)
		p.s.add(list)
		p.inst(opPOPDOT, 0)
	case parse.NodeRange:
		// pop the dot at the end of every iteration
		list.append(opPOPDOT, 0)
		// if there are variables declared, add instructions
		// for setting them, then pop until the iterator is at
		// the top
		if len(b.Pipe.Decl) > 0 {
			if len(b.Pipe.Decl) > 1 {
				list.prepend(opSETVAR, p.addString(b.Pipe.Decl[0].Ident[0][1:]))
				list.prepend(opSETVAR, p.addString(b.Pipe.Decl[1].Ident[0][1:]))
			} else {
				list.prepend(opPOP, 1).prepend(opSETVAR, p.addString(b.Pipe.Decl[0].Ident[0][1:]))
			}
		} else {
			list.prepend(opPOP, 2)
		}
		// start each iteration with the dot set. note that we're
		// prepending here, so this executes before setting the vars
		// and popping
		list.prepend(opPUSHDOT, 0)
		// add a jump back to 1 instruction before the
		// list, which will call NEXT again.
		list.append(opJMP, valType(-len(list.buf)-2))
		// initialize the iter
		p.inst(opITER, 0)
		if elseList == nil {
			// no elseList. just iterate and jump out of the
			// loop once we reach the end of the iteration
			p.inst(opNEXT, valType(len(list.buf)))
		} else {
			// if the iteration stopped in the first step, we
			// need to jump to elseList, skipping the JMP at its
			// start (for range loops the JMP is not really needed,
			// but one extra instruction won't hurt much). We also
			// need to skip the 2 instructions following this one.
			p.inst(opNEXT, valType(len(list.buf)+1+2))
			// Now jump the following instruction, it's used for
			// subsequent iterations
			p.inst(opJMP, 1)
			// If ended, jump outside list and elseList
			out := len(list.buf) + 1
			if elseList != nil {
				out += len(elseList.buf)
			}
			// 2nd and the rest of iterations start here
			p.inst(opNEXT, valType(out))
		}
		p.s.add(list)
	default:
		return fmt.Errorf("invalid branch type %v", nt)
	}
	if elseList != nil {
		p.inst(opJMP, valType(len(elseList.buf)))
		p.s.add(elseList)
	}
	p.s.addPop(pop)
	return nil
}

func (p *program) walk(n parse.Node) error {
	switch x := n.(type) {
	case *parse.ActionNode:
		s := p.s.snap()
		if err := p.walk(x.Pipe); err != nil {
			return err
		}
		if len(x.Pipe.Decl) == 0 {
			if p.s.noPrint {
				p.s.noPrint = false
			} else {
				p.inst(opPRINT, 0)
			}
		}
		pipe := p.s.snap()
		p.s.restore(s)
		if len(pipe.buf) == 2 && pipe.buf[0].op == opVAL && pipe.buf[1].op == opPRINT {
			// this pipe only generates a value and then
			// prints it. we can generate the value now
			// and translate it to an opWB
			v := p.values[pipe.buf[0].val]
			val, doPrint, ok := printableValue(v)
			if ok {
				// otherwise let it generate a runtime error
				if doPrint {
					var buf bytes.Buffer
					fmt.Fprint(&buf, val)
					p.addWB(buf.Bytes())
				}
				break
			}
		}
		if err := pipe.putPop(); err != nil {
			return err
		}
		p.s.add(pipe)
	case *parse.BoolNode:
		p.inst(opVAL, p.addValue(x.True))
	case *parse.ChainNode:
		p.walk(x.Node)
		p.addFIELDs(x.Field)
	case *parse.CommandNode:
		// Command nodes are pushed on reverse order, so they are
		// evaluated from right to left. If we encounter a function
		// while executing it, we can just grab the arguments from the stack
		argc := 0
		p.s.cmd = append(p.s.cmd, argc)
		for ii := len(x.Args) - 1; ii >= 0; ii-- {
			p.s.cmd[len(p.s.cmd)-1] = argc
			node := x.Args[ii]
			if err := p.walk(node); err != nil {
				return err
			}
			argc++
		}
		p.s.cmd = p.s.cmd[:len(p.s.cmd)-1]
	case *parse.DotNode:
		p.inst(opDOT, 0)
	case *parse.FieldNode:
		p.inst(opDOT, 0)
		p.addFIELDs(x.Ident)
	case *parse.IdentifierNode:
		if len(p.s.cmd) == 0 {
			return fmt.Errorf("identifier %q outside of command?", x.Ident)
		}
		if x.Ident == topAssetsFuncName || x.Ident == bottomAssetsFuncName {
			// These functions don't exist anymore, we translate
			// them to WB calls, since assets can't be added once the
			// template is compiled
			var b []byte
			if x.Ident == topAssetsFuncName {
				b = p.tmpl.topAssets
			} else {
				b = p.tmpl.bottomAssets
			}
			if len(b) > 0 {
				p.addWB(b)
			}
			p.s.noPrint = true
			break
		}
		name := x.Ident
		if strings.HasPrefix(name, "html_") {
			if p.s.noPrint {
				// previous pipeline was precomputed
				// and translated to a opWB
				break
			}
			if name == templateCommentEscaper && !p.tmpl.tmpl.DropComments {
				// Don't remove HTML comments, that's an idiotic
				// behavior from html/template
				break
			}
			// Check if the input of this function is a string or template.HTML
			// and either use the specialized function or remove the escaping
			// entirely when possible.
			if typ := p.prevFuncReturnType(); typ != nil {
				switch {
				case types.IsNumeric(typ):
					name = ""
				case typ.Kind() == reflect.String:
					switch typ {
					case stringType:
						if name == templateHtmlEscaper {
							// specialized to avoid type assertions
							name = "html_template_htmlstringescaper"
						}
					case htmlType, templateHtmlType:
						if name == templateHtmlEscaper {
							name = ""
						}
					case jsType, templateJsType:
						if name == templateJsEscaper {
							name = ""
						}
					}
				}
			}
		}
		if name == "" {
			// Function optimized away
			break
		}
		// check for the stable function first
		fn := p.tmpl.funcMap["#"+name]
		if fn == nil {
			fn = p.tmpl.funcMap[name]
		}
		if fn == nil {
			return fmt.Errorf("undefined function %q", name)
		}
		p.inst(opFUNC, encodeVal(p.s.argc(), p.addFunc(fn, name)))
	case *parse.IfNode:
		if err := p.walkBranch(parse.NodeIf, &x.BranchNode); err != nil {
			return err
		}
	case *parse.ListNode:
		for _, node := range x.Nodes {
			if err := p.walk(node); err != nil {
				return err
			}
		}
	case *parse.NumberNode:
		var val valType
		switch {
		case x.IsComplex:
			val = p.addValue(x.Complex128)
		case x.IsFloat && (strings.Contains(x.Text, ".") || strings.Contains(strings.ToLower(x.Text), "e")):
			val = p.addValue(x.Float64)
		case x.IsInt:
			val = p.addValue(int(x.Int64))
		case x.IsUint:
			val = p.addValue(int(x.Uint64))
		default:
			return fmt.Errorf("invalid number node %v", x)
		}
		p.inst(opVAL, val)
	case *parse.NilNode:
		val := p.addValue(nil)
		p.inst(opVAL, val)
	case *parse.PipeNode:
		s := p.s.snap()
		for ii, v := range x.Cmds {
			p.s.pipe = append(p.s.pipe, ii)
			if err := p.walk(v); err != nil {
				return err
			}
			p.s.pipe = p.s.pipe[:len(p.s.pipe)-1]
		}
		pipe := p.s.snap()
		p.s.restore(s)
		if len(pipe.buf) > 1 && p.scratchIsPure(pipe) {
			state, err := p.executeScratch(pipe)
			if err == nil {
				pipe.clear()
				for _, v := range state.stack {
					pipe.prepend(opVAL, p.addValue(v.Interface()))
				}
			}
		}
		p.s.add(pipe)
		if !p.s.branchPipe {
			for _, variable := range x.Decl {
				// Remove $
				p.inst(opSETVAR, p.addString(variable.Ident[0][1:]))
			}
		}
	case *parse.RangeNode:
		if err := p.walkBranch(parse.NodeRange, &x.BranchNode); err != nil {
			return err
		}
	case *parse.StringNode:
		p.addSTRING(x.Text)
	case *parse.TemplateNode:
		s := p.s.snap()
		pop := -1
		if x.Pipe != nil {
			if err := p.walk(x.Pipe); err != nil {
				return err
			}
			pipe := p.s.snap()
			p.s.restore(s)
			pipe.putPop()
			pop = pipe.takePop()
			p.s.add(pipe)
		}
		ns := namespace(x.Name)
		if pns := namespace(p.s.name); pns != "" {
			ns = ns[len(pns):]
		}
		p.inst(opTEMPLATE, encodeVal(int(p.addString(ns)), p.addString(x.Name)))
		p.s.addPop(pop)
	case *parse.TextNode:
		text := x.Text
		if len(p.s.buf) == 0 && len(x.Text) > 1 && strings.Contains(p.tmpl.contentType, "html") && x.Text[0] == '\n' && x.Text[1] == '<' {
			text = text[1:]
		}
		if len(text) > 0 {
			p.addWB(text)
		}
	case *parse.VariableNode:
		// Remove $ sign
		p.inst(opVAR, p.addString(x.Ident[0][1:]))
		p.addFIELDs(x.Ident[1:])
	case *parse.WithNode:
		if err := p.walkBranch(parse.NodeWith, &x.BranchNode); err != nil {
			return err
		}
	default:
		return fmt.Errorf("can't compile node %T", n)
	}
	p.s.ctx = append(p.s.ctx, &context{pc: len(p.s.buf), node: n})
	return nil
}

func (p *program) fnIsPure(idx int) bool {
	fn := p.funcs[idx]
	if fn == nil {
		return false
	}
	return htmlEscapeFuncs[fn.name] != nil || p.tmpl.funcMap["#"+fn.name] != nil
}

// scratchIsPure returns if the scratch does not
// depend on the dot nor variables and only calls pure
// functions. In that case, it can be executed at compile time.
func (p *program) scratchIsPure(s *scratch) bool {
	for _, v := range s.buf {
		switch v.op {
		case opSTRING, opVAL:
		case opFUNC:
			_, i := decodeVal(v.val)
			if !p.fnIsPure(i) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func (p *program) executeScratch(s *scratch) (*state, error) {
	name := stringutil.Random(16)
	p.code[name] = s.buf
	// We don't really need a buffer here, but let's
	// pass one just in case. If a bug were found when
	// executing a scratch, wasting a few bytes is better
	// than crashing
	var buf bytes.Buffer
	st := newState(p, &buf)
	err := st.execute(name, "", reflect.Value{})
	putState(st)
	delete(p.code, name)
	if err != nil {
		return nil, err
	}
	return st, nil
}

func (p *program) stitchTree(name string) {
	// TODO: Save the name of the original template somewhere
	// so we can recover it for error messages. Until we fix
	// that problem we're only stitching trees which are just
	// a WB. In most cases, this will inline the top and bottom
	// hooks, giving already a nice performance boost.
	code := p.code[name]
	for ii := 0; ii < len(code); ii++ {
		v := code[ii]
		if v.op == opTEMPLATE {
			_, t := decodeVal(v.val)
			tmpl := p.strings[t]
			repl := p.code[tmpl]
			if len(repl) == 1 && repl[0].op == opWB {
				// replace the tree
				code = instructions(code).replace(ii, 1, repl)
				ii--
			}
		}
	}
	p.code[name] = code
}

func (p *program) stitch() {
	p.stitchTree(p.tmpl.root)
}

func (p *program) execute(w *bytes.Buffer, name string, data interface{}, vars VarMap) error {
	s := newState(p, w)
	s.pushVar("Vars", reflect.ValueOf(vars))
	err := s.execute(name, "", reflect.ValueOf(data))
	putState(s)
	return err
}

func compileTemplate(p *program, tmpl *Template) error {
	for k, v := range tmpl.trees {
		p.s = new(scratch)
		p.s.name = k
		if err := p.walk(v.Root); err != nil {
			return err
		}
		p.code[k] = p.s.buf
		p.context[k] = p.s.ctx
		p.s = nil
	}
	return nil
}

func newProgram(tmpl *Template) (*program, error) {
	if strings.Contains(tmpl.contentType, "html") {
		tmpl.addHtmlEscaping()
	}
	p := &program{tmpl: tmpl, code: make(map[string][]inst), context: make(map[string][]*context)}
	if err := compileTemplate(p, tmpl); err != nil {
		return nil, err
	}
	p.stitch()
	return p, nil
}

func isTrue(v reflect.Value) bool {
	t, _ := types.IsTrueVal(v)
	return t
}

func printableValue(v reflect.Value) (interface{}, bool, bool) {
	if k := v.Kind(); k == reflect.Ptr || k == reflect.Interface {
		var isNil bool
		v, isNil = indirect(v)
		if isNil && v.Type() == emptyType {
			return nil, false, true
		}
	}
	if !v.IsValid() {
		return nil, false, true
	}

	typ := v.Type()
	if !typ.Implements(errType) && !typ.Implements(stringerType) {
		ptyp := reflect.PtrTo(typ)
		if v.CanAddr() && (ptyp.Implements(errType) || ptyp.Implements(stringerType)) {
			v = v.Addr()
		} else {
			switch v.Kind() {
			case reflect.Chan, reflect.Func:
				return nil, false, false
			}
		}
	}
	return v.Interface(), true, true
}

func stackable(v reflect.Value) reflect.Value {
	if v.IsValid() && v.Type() == emptyType && !v.IsNil() {
		v = reflect.ValueOf(v.Interface())
	}
	return v
}

func namespace(name string) string {
	if p := strings.Index(name, nsMark); p >= 0 {
		return name[:p]
	}
	return ""
}
