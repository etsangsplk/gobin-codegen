package binidl

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"strconv"
)

type Binidl struct {
	ast  *ast.File
	fset *token.FileSet
}

func NewBinidl(filename string) *Binidl {
	fset := token.NewFileSet()
	ast, err := parser.ParseFile(fset, filename, nil, 0) // scanner.InsertSemis)
	if err != nil {
		fmt.Println("Error parsing", filename, ":", err)
		return nil
	}
	return &Binidl{ast, fset}
}

func setbs(b io.Writer, n int, es *EmitState) {
	if es.isStatic {
		return
	}
	if n != es.curBSize {
		fmt.Fprintf(b, "bs = b[:%d]\n", n)
	}
	es.curBSize = n
}

func unmarshalField(b io.Writer, fname, tname string, es *EmitState) {
	tconv := tname
	if mapped, ok := typemap[tname]; ok {
		tconv = mapped
	}

	ti, ok := typedb[tconv]
	if !ok {
		fmt.Fprintf(b, "%s.Unmarshal(r)\n", fname)
		return
	}

	setbs(b, ti.Size, es)
	fmt.Fprintln(b,
		`if _, err := io.ReadFull(r, bs); err != nil {
return err
}`)
	if ti.Size == 1 {
		fmt.Fprintf(b, "%s = %s(b[0])\n", fname, tname)
	} else {
		fmt.Fprintf(b, "%s = %s(%s(bs))\n", fname, tname, decodeFunc[ti.EncodesAs])
	}
}

func marshalField(b io.Writer, fname, tname string, es *EmitState) {
	if mapped, ok := typemap[tname]; ok {
		tname = mapped
	}
	ti, ok := typedb[tname]
	if !ok {
		fmt.Fprintf(b, "%s.Marshal(w)\n", fname)
		return
	}

	setbs(b, ti.Size, es)
	if ti.Size == 1 {
		fmt.Fprintf(b, "b[%d] = byte(%s)\n", es.Bstart(1), fname)
	} else {
		ef := encodeFunc[ti.EncodesAs]
		if !es.isStatic {
			fmt.Fprintf(b, "%s(bs, %s(%s))\n", ef, ti.EncodesAs, fname)
		} else {
			bstart := es.Bstart(ti.Size)
			bend := bstart + ti.Size
			fmt.Fprintf(b, "%s(b[%d:%d], %s(%s))\n", ef, bstart, bend, ti.EncodesAs, fname)
		}
	}
	if !es.isStatic {
		fmt.Fprintln(b, "w.Write(bs)")
	}
}

func walkContents(b io.Writer, st *ast.StructType, pred string, funcname string, fn func(io.Writer, string, string, *EmitState), es *EmitState) {
	for _, f := range st.Fields.List {
		for _, fNameEnt := range f.Names {
			fname := fNameEnt.Name
			newpred := pred + "." + fname
			walkOne(b, f, newpred, funcname, fn, es)
		}
	}
}

const (
	MARSHAL = iota
	UNMARSHAL
)

type EmitState struct {
	op           int // MARSHAL, UNMARSHAL
	nextIdx      int
	isStatic     bool
	staticOffset int
	alenIdx      int
	curBSize     int
}

func (es *EmitState) getNewAlen() string {
	es.alenIdx++
	return fmt.Sprintf("alen%d", es.alenIdx)
}

func (es *EmitState) getIndexStr() string {
	indexes := []string{"i", "j", "k", "ii", "jj", "kk"}
	if es.nextIdx > 5 {
		panic("Array nesting depth too large.  Lazy programmer bites again.")
	}
	i := es.nextIdx
	es.nextIdx++
	return indexes[i]
}

func (es *EmitState) freeIndexStr() {
	es.nextIdx--
}

func (es *EmitState) Bstart(n int) int {
	if !es.isStatic {
		return 0
	}
	o := es.staticOffset
	es.staticOffset += n
	return o
}

var need_bufio = false

var typemap map[string]string = make(map[string]string)

type TypeInfo struct {
	Name      string
	Size      int
	EncodesAs string
}

var encodeFunc map[string]string = map[string]string{
	"uint64": "binary.LittleEndian.PutUint64",
	"uint32": "binary.LittleEndian.PutUint32",
	"uint16": "binary.LittleEndian.PutUint16",
}

var decodeFunc map[string]string = map[string]string{
	"uint64": "binary.LittleEndian.Uint64",
	"uint32": "binary.LittleEndian.Uint32",
	"uint16": "binary.LittleEndian.Uint16",
}

var typedb map[string]TypeInfo = map[string]TypeInfo{
	"int":    {"int", 8, "uint64"},
	"uint64": {"uint64", 8, "uint64"},
	"int64":  {"int64", 8, "uint64"},
	"int32":  {"int32", 4, "uint32"},
	"uint32": {"uint32", 4, "uint32"},
	"int16":  {"int16", 2, "uint16"},
	"uint16": {"uint16", 2, "uint16"},
	"int8":   {"int8", 1, "byte"},
	"uint8":  {"uint8", 1, "byte"},
	"byte":   {"byte", 1, "byte"},
}

func walkOne(b io.Writer, f *ast.Field, pred string, funcname string, fn func(io.Writer, string, string, *EmitState), es *EmitState) {
	switch f.Type.(type) {
	case *ast.Ident:
		t := f.Type.(*ast.Ident)
		_, is_mapped := typemap[t.Name]

		if dispatchTo, ok := globalDeclMap[t.Name]; !is_mapped && ok && es.isStatic {
			if strucType, ok := dispatchTo.Type.(*ast.StructType); ok {
				walkContents(b, strucType, pred, funcname, fn, es)
			} else {
				panic("Eek, a type I don't handle properly")
			}
		} else {
			fn(b, pred, t.Name, es)
		}
	case *ast.SelectorExpr:
		//se := f.Type.(*ast.SelectorExpr)
		//fmt.Printf("%s.%s%s(&%s, w)\n",
		//	se.X, funcname, se.Sel.Name, pred)
		var ioid string
		switch es.op {
		case UNMARSHAL:  ioid = "r"
		case MARSHAL:    ioid = "w"
		}

		fmt.Fprintf(b, "%s.%s(%s)\n",
			pred, funcname, ioid)
	case *ast.ArrayType:
		s := f.Type.(*ast.ArrayType)
		i := es.getIndexStr()
		arrayLen := 0
		alenid := es.getNewAlen()
		if s.Len == nil {
			// If we are unmarshaling we need to allocate.
			if es.op == UNMARSHAL {
				fmt.Fprintf(b, "%s, err := binary.ReadVarint(r)\n", alenid)
				fmt.Fprintf(b, "if err != nil {\n")
				fmt.Fprintf(b, "return err\n")
				fmt.Fprintf(b, "}\n")
				fmt.Fprintf(b, "%s = make([]%s, %s)\n", pred, s.Elt, alenid)
			} else {
				setbs(b, 10, es)
				fmt.Fprintf(b, "%s := int64(len(%s))\n", alenid, pred)
				fmt.Fprintf(b, "if wlen := binary.PutVarint(bs, %s); wlen >= 0 {\n", alenid)
				fmt.Fprintf(b, "w.Write(b[0:wlen])\n")
				fmt.Fprintf(b, "}\n")
			}
			fmt.Fprintf(b, "for %s := int64(0); %s < %s; %s++ {\n", i, i, alenid, i)
		} else {
			e, ok := s.Len.(*ast.BasicLit)
			if !ok {
				panic("Bad literal in array decl")
			}
			var err error
			arrayLen, err = strconv.Atoi(e.Value)
			if err != nil {
				panic("Bad array length value.  Must be a simple int.")
			}
			if !es.isStatic {
				fmt.Fprintf(b, "for %s := 0; %s < %d; %s++ {\n", i, i, arrayLen, i)
			}
		}

		fsub := fmt.Sprintf("%s[%s]", pred, i)
		pseudofield := &ast.Field{nil, nil, s.Elt, nil, nil}
		if es.isStatic {
			for idx := 0; idx < arrayLen; idx++ {
				fsub := fmt.Sprintf("%s[%d]", pred, idx)
				walkOne(b, pseudofield, fsub, funcname, fn, es)
			}
		} else {
			walkOne(b, pseudofield, fsub, funcname, fn, es)
			fmt.Fprintln(b, "}")
		}
		es.freeIndexStr()
	default:
		fmt.Println("Unknown type: ", f)
		panic("Unknown type in struct")
	}
}

type StructInfo struct {
	size         int
	maxSize      int
	firstSize    int
	varLen       bool
	mustDispatch bool
	totalSize    int // Including embedded types, if known
}

var structInfoMap map[string]*StructInfo 

func mergeInfo(parent, child *StructInfo, childcount int) {
	if parent.firstSize == 0 {
		parent.firstSize = child.firstSize
	}
	if child.maxSize > parent.maxSize {
		parent.maxSize = child.maxSize
	}
	parent.varLen = parent.varLen || child.varLen
	parent.mustDispatch = parent.mustDispatch || child.mustDispatch

	if childcount > 0 {
		parent.size += child.size * childcount
		parent.totalSize += child.totalSize * childcount
	}

}

// Wouldn't it be nice to cache a lot of this? :-)
func analyze(n interface{}) (info *StructInfo) {
	info = new(StructInfo)
	switch n.(type) {
	case *ast.StructType:
		st := n.(*ast.StructType)
		for _, field := range st.Fields.List {
			for _, _ = range field.Names {
				mergeInfo(info, analyze(field), 1)
			}
		}
	case *ast.Field:
		f := n.(*ast.Field)
		switch f.Type.(type) {
		case *ast.Ident:
			tname := f.Type.(*ast.Ident).Name
			if mapped, ok := typemap[tname]; ok {
				tname = mapped
			}
			if tinfo, ok := typedb[tname]; ok {
				info.firstSize = tinfo.Size
				info.maxSize = tinfo.Size
				info.size = tinfo.Size
			} else {
				seinfo := analyzeType(tname)
				if (seinfo != nil && seinfo.mustDispatch == false && seinfo.varLen == false) {
					mergeInfo(info, seinfo, 1)
				} else {
					info.mustDispatch = true
				}
			}
			return
		case *ast.SelectorExpr:
			info.mustDispatch = true
			return
		case *ast.ArrayType:
			s := f.Type.(*ast.ArrayType)
			arraylen := 0
			if s.Len == nil {
				// If we are unmarshaling we need to allocate.
				info.varLen = true
				need_bufio = true // eventually just in info
			} else {
				e, ok := s.Len.(*ast.BasicLit)
				if !ok {
					panic("Bad literal in array decl")
				}
				var err error
				arraylen, err = strconv.Atoi(e.Value)
				if err != nil {
					panic("Bad array length value.  Must be a simple int.")
				}
			}

			pseudofield := &ast.Field{nil, nil, s.Elt, nil, nil}
			mergeInfo(info, analyze(pseudofield), arraylen)
		default:
			fmt.Println("Unknown type in struct: ", f)
			panic("Unknown type in struct")
		}
	default:
		panic("Unknown ast type")
	}
	return
}

func analyzeType(typeName string) (info *StructInfo) {
	ts, ok := globalDeclMap[typeName]
	if !ok {
		return nil
	}
	
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
  		if id, ok := ts.Type.(*ast.Ident); ok {
			tname := id.Name
			if _, ok := typedb[tname]; ok {
				typemap[typeName] = tname
				return
			}
		}
		panic("Can't handle decl!")
	}
	info = analyze(st)
	return
}

func structmap(out io.Writer, ts *ast.TypeSpec) {
	typeName := ts.Name.Name
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		//fmt.Println("Type of type is ", reflect.TypeOf(ts.Type))
		if id, ok := ts.Type.(*ast.Ident); ok {
			tname := id.Name
			if _, ok := typedb[tname]; ok {
				typemap[typeName] = tname
				return
			}
		}
		panic("Can't handle decl!")
	}
	info := analyze(st)
	//fmt.Println("Analysis result: ", info)

	fmt.Fprintf(out, "func (t *%s) BinarySize() (nbytes int, sizeKnown bool) {\n", typeName)
	if !info.varLen && !info.mustDispatch {
		fmt.Fprintf(out, "  return %d, true\n", info.size)
	} else {
		fmt.Fprintf(out, "return 0, false\n")
	}
	fmt.Fprintln(out, "}")

	b := new(bytes.Buffer)


	//fmt.Println("ts: ", typeName)
	mes := &EmitState{}
	mes.op = MARSHAL
	if info.size < 64 && !info.varLen && !info.mustDispatch {
		mes.isStatic = true
	}
	blen := 8
	if info.varLen {
		blen = 10
	}
	if mes.isStatic {
		blen = info.size
	}
	fmt.Fprintf(out, "func (t *%s) Marshal(w io.Writer) {\n", typeName)
	fmt.Fprintf(out, "var b [%d]byte\n", blen)
	if !mes.isStatic {
		fmt.Fprintf(out, "bs := b[:%d]\n", info.firstSize)
	}
	mes.curBSize = info.firstSize
	walkContents(b, st, "t", "Marshal", marshalField, mes)

	b.WriteTo(out)
	if mes.isStatic {
		fmt.Fprintln(out, "w.Write(b[:])")
	}
	fmt.Fprintln(out, "}\n")

	b.Reset()
	ues := &EmitState{}
	ues.op = UNMARSHAL
	ues.curBSize = info.firstSize
	blen = 8
	if info.varLen {
		blen = 10
	}
	if info.size < 64 && !info.varLen && !info.mustDispatch {
		ues.isStatic = true
	}
	walkContents(b, st, "t", "Unmarshal", unmarshalField, ues)
	paramname := "r"
	if info.varLen {
		paramname = "rr"
	}
	fmt.Fprintf(out, "func (t *%s) Unmarshal(%s io.Reader) error {\n", typeName, paramname)

	if info.varLen {
		fmt.Fprintln(out,
			`var r byteReader
var ok bool
if r, ok = rr.(byteReader); !ok {
    r = bufio.NewReader(rr)
}`)
	}
	fmt.Fprintf(out, "var b [%d]byte\n", blen)
	fmt.Fprintf(out, "bs := b[:%d]\n", info.firstSize)
	b.WriteTo(out)
	fmt.Fprintln(out, "return nil\n}\n")
	return
}

var globalDeclMap map[string]*ast.TypeSpec = make(map[string]*ast.TypeSpec)

func createGlobalDeclMap(decls []ast.Decl) {
	for _, d := range decls {
		decl, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		if decl.Tok != token.TYPE {
			continue
		}
		//fmt.Println("Got a type!")
		ts := decl.Specs[0].(*ast.TypeSpec)
		decltypeName := ts.Name.Name
		globalDeclMap[decltypeName] = ts
	}
}

func (bf *Binidl) PrintGo() {
	createGlobalDeclMap(bf.ast.Decls) // still a temporary hack
	rest := new(bytes.Buffer)
	for _, d := range globalDeclMap {
		structmap(rest, d)
	}
	b := new(bytes.Buffer)
	fmt.Fprintln(b, "package", bf.ast.Name.Name)
	imports := []string{"io", "encoding/binary"}
	if need_bufio {
		imports = append(imports, "bufio")
	}
	fmt.Fprintln(b, "import (")
	for _, imp := range imports {
		fmt.Fprintf(b, "\"%s\"\n", imp)
	}
	fmt.Fprintln(b, ")")
	if need_bufio {
		fmt.Fprintln(b, `type byteReader interface {
io.Reader
ReadByte() (c byte, err error)
}`)
	}
	// Output and then gofmt it to make it pretty and shiny.  And readable.
	tf, _ := ioutil.TempFile("", "gobin-codegen")
	tfname := tf.Name()
	defer os.Remove(tfname)
	defer tf.Close()

	b.WriteTo(tf)
	rest.WriteTo(tf)
	tf.Sync()

	fset := token.NewFileSet()
	ast, err := parser.ParseFile(fset, tfname, nil, 0)
	if err != nil {
		panic(err.Error())
	}
	printer.Fprint(os.Stdout, fset, ast)
}
