package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// verbose turns on trace-level debugging.
var verbose = flag.Bool("v", false, "verbose")

func main() {
	log.SetFlags(0)

	// Parse command line arguments.
	flag.Parse()
	root := flag.Arg(0)
	if root == "" {
		log.Fatal("path required")
	}

	// Iterate over the tree and process files importing boltdb/raw.
	if err := filepath.Walk(root, walk); err != nil {
		log.Fatal(err)
	}
}

// Walk recursively iterates over all files in a directory and processes any
// file that imports "github.com/boltdb/raw".
func walk(path string, info os.FileInfo, err error) error {
	traceln("walk:", path)

	if info == nil {
		return fmt.Errorf("file not found: %s", err)
	} else if info.IsDir() {
		traceln("skipping: is directory")
		return nil
	} else if filepath.Ext(path) != ".go" {
		traceln("skipping: is not a go file")
		return nil
	}

	// Check if file imports boltdb/raw.
	if v, err := importsRaw(path); err != nil {
		return err
	} else if !v {
		traceln("skipping: does not import raw")
		return nil
	}

	// Process each file.
	if err := process(path); err != nil {
		return err
	}

	return nil
}

// importsRaw returns true if a given path imports boltdb/raw.
func importsRaw(path string) (bool, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		return false, err
	}
	for _, i := range f.Imports {
		traceln("✓ imports", i.Path.Value)
		if i.Path.Value == `"github.com/boltdb/raw"` {
			return true, nil
		}
	}
	return false, nil
}

// process parses and rewrites a file by generating the appropriate exported
// types for raw types.
func process(path string) error {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return err
	}

	// Remove code between begin/end pragma comments.
	b = regexp.MustCompile(`(?is)//raw:codegen:begin.+?//raw:codegen:end`).ReplaceAll(b, []byte{})
	b = []byte(strings.TrimRight(string(b), " \n\r"))

	// Re-parse the file without the pragmas.
	f, err := parser.ParseFile(token.NewFileSet(), path, b, 0)
	if err != nil {
		return err
	}

	// Iterate over all the nodes and add exported types where appropriate.
	var g generator
	g.w.Write(b)
	g.w.WriteString("\n\n")

	ast.Walk(&g, f)
	if g.err != nil {
		return g.err
	}

	// Rewrite original file.
	ioutil.WriteFile(path, g.w.Bytes(), 0600)

	log.Println("OK", path)

	return nil
}

// generator iterates over every AST node and generates code as appropriate.
type generator struct {
	w   bytes.Buffer
	err error
}

// Visit implements the ast.Visitor interface. It is called once for every AST node.
func (g *generator) Visit(node ast.Node) ast.Visitor {
	if g.err != nil || node == nil {
		return nil
	}

	switch node := node.(type) {
	case *ast.TypeSpec:
		if err := g.visitTypeSpec(node); err != nil {
			g.err = err
		}
	}
	return g
}

// visitTypeSpec is called for every type declaration. Each declaration is
// checked for raw usage and an exported type is generated if appropriate.
func (g *generator) visitTypeSpec(node *ast.TypeSpec) error {
	// Only process struct types.
	s, ok := node.Type.(*ast.StructType)
	if !ok {
		return nil
	}

	// Check if this struct type contains only raw fields.
	if !isRawStructType(s) {
		traceln("not raw:", node.Name.Name)
		return nil
	}

	// Disallow raw structs that are exported.
	if unicode.IsUpper(rune(node.Name.Name[0])) {
		return fmt.Errorf("raw struct cannot be exported: %s", node.Name.Name)
	}

	// Generate an exported name.
	unexp := node.Name.Name
	exp := tocamelcase(node.Name.Name)

	tracef("• processing: %s -> %s", unexp, exp)

	// Generate exported struct and functions.
	fmt.Fprint(&g.w, "//raw:codegen:begin\n\n")
	fmt.Fprint(&g.w, "//\n")
	fmt.Fprint(&g.w, "// DO NOT CHANGE\n")
	fmt.Fprint(&g.w, "// This section has been generated by bolt-rawgen.\n")
	fmt.Fprint(&g.w, "//\n\n")
	if err := writeExportedType(exp, s, &g.w); err != nil {
		return fmt.Errorf("generate exported type: %s", s)
	}
	if err := writeEncodeFunc(unexp, exp, s, &g.w); err != nil {
		return fmt.Errorf("generate encode func: %s", s)
	}
	if err := writeDecodeFunc(unexp, exp, s, &g.w); err != nil {
		return fmt.Errorf("generate decode func: %s", s)
	}
	if err := writeAccessorFuncs(unexp, s, &g.w); err != nil {
		return fmt.Errorf("generate accessor funcs: %s", s)
	}
	fmt.Fprint(&g.w, "//raw:codegen:end\n\n")

	return nil
}

// writeExportedType writes a generated exported type for a raw struct type.
func writeExportedType(name string, node *ast.StructType, w io.Writer) error {
	fmt.Fprintf(w, "type %s struct {\n", name)

	for _, f := range node.Fields.List {
		var typ string
		switch tostr(f.Type) {
		case "bool":
			typ = "bool"
		case "int8", "int16", "int32", "int64":
			typ = "int"
		case "uint8", "uint16", "uint32", "uint64":
			typ = "uint"
		case "float32":
			typ = "float32"
		case "float64":
			typ = "float64"
		case "raw.Time":
			typ = "time.Time"
		case "raw.Duration":
			typ = "time.Duration"
		case "raw.String":
			typ = "string"
		default:
			return fmt.Errorf("invalid raw type: %s", tostr(f.Type))
		}

		for _, n := range f.Names {
			fmt.Fprintf(w, "\t%s %s\n", tocamelcase(n.Name), typ)
		}
	}

	fmt.Fprintf(w, "}\n\n")
	return nil
}

// writeEncodeFunc writes a generated encoding function for a raw struct type.
func writeEncodeFunc(unexp, exp string, node *ast.StructType, w io.Writer) error {
	fmt.Fprintf(w, "func (o *%s) Encode() []byte {\n", exp)
	fmt.Fprintf(w, "\tvar r %s\n", unexp)
	fmt.Fprintf(w, "\tb := make([]byte, unsafe.Sizeof(r), int(unsafe.Sizeof(r)))\n")

	for _, f := range node.Fields.List {
		typ := tostr(f.Type)
		for _, n := range f.Names {
			switch typ {
			case "bool":
				fmt.Fprintf(w, "\tr.%s = o.%s\n", n.Name, tocamelcase(n.Name))
			case "int8", "int16", "int32", "int64", "uint8", "uint16", "uint32", "uint64", "float32", "float64":
				fmt.Fprintf(w, "\tr.%s = %s(o.%s)\n", n.Name, typ, tocamelcase(n.Name))
				typ = "uint"
			case "raw.Time":
				fmt.Fprintf(w, "\tr.%s = raw.Time(o.%s.UnixNano())\n", n.Name, tocamelcase(n.Name))
			case "raw.Duration":
				fmt.Fprintf(w, "\tr.%s = raw.Duration(o.%s)\n", n.Name, tocamelcase(n.Name))
			case "raw.String":
				fmt.Fprintf(w, "\tr.%s.Encode(o.%s, &b)\n", n.Name, tocamelcase(n.Name))
			default:
				return fmt.Errorf("invalid raw type: %s", tostr(f.Type))
			}
		}
	}

	fmt.Fprintf(w, "\tcopy(b, (*[unsafe.Sizeof(r)]byte)(unsafe.Pointer(&r))[:])\n")
	fmt.Fprintf(w, "\treturn b\n")
	fmt.Fprintf(w, "}\n\n")
	return nil
}

// writeDecodeFunc writes a generated decoding function for a raw struct type.
func writeDecodeFunc(unexp, exp string, node *ast.StructType, w io.Writer) error {
	fmt.Fprintf(w, "func (o *%s) Decode(b []byte) {\n", exp)
	fmt.Fprintf(w, "\tr := (*%s)(unsafe.Pointer(&b[0]))\n", unexp)

	for _, f := range node.Fields.List {
		for _, n := range f.Names {
			fmt.Fprintf(w, "\to.%s = r.%s()\n", tocamelcase(n.Name), tocamelcase(n.Name))
		}
	}

	fmt.Fprintf(w, "}\n\n")
	return nil
}

// writeAccessorFuncs writes a accessor functions for a raw struct type.
func writeAccessorFuncs(name string, node *ast.StructType, w io.Writer) error {
	for _, f := range node.Fields.List {
		typ := tostr(f.Type)
		for _, n := range f.Names {
			switch typ {
			case "bool":
				fmt.Fprintf(w, "func (r *%s) %s() bool { return r.%s }\n\n", name, tocamelcase(n.Name), n.Name)
			case "int8", "int16", "int32", "int64":
				fmt.Fprintf(w, "func (r *%s) %s() int { return int(r.%s) }\n\n", name, tocamelcase(n.Name), n.Name)
			case "uint8", "uint16", "uint32", "uint64":
				fmt.Fprintf(w, "func (r *%s) %s() uint { return uint(r.%s) }\n\n", name, tocamelcase(n.Name), n.Name)
			case "float32", "float64":
				fmt.Fprintf(w, "func (r *%s) %s() %s { return r.%s }\n\n", name, tocamelcase(n.Name), typ, n.Name)
			case "raw.Time":
				fmt.Fprintf(w, "func (r *%s) %s() time.Time { return time.Unix(0, int64(r.%s)).UTC() }\n\n", name, tocamelcase(n.Name), n.Name)
			case "raw.Duration":
				fmt.Fprintf(w, "func (r *%s) %s() time.Duration { return time.Duration(r.%s) }\n\n", name, tocamelcase(n.Name), n.Name)
			case "raw.String":
				fmt.Fprintf(w, "func (r *%s) %s() string { return r.%s.String(((*[0xFFFF]byte)(unsafe.Pointer(r)))[:]) }\n", name, tocamelcase(n.Name), n.Name)
				fmt.Fprintf(w, "func (r *%s) %sBytes() []byte { return r.%s.Bytes(((*[0xFFFF]byte)(unsafe.Pointer(r)))[:]) }\n\n", name, tocamelcase(n.Name), n.Name)
			default:
				return fmt.Errorf("invalid raw type: %s", tostr(f.Type))
			}
		}
	}
	return nil
}

// isRawStructType returns true when a type declaration uses all raw types.
func isRawStructType(node *ast.StructType) bool {
	for _, f := range node.Fields.List {
		switch tostr(f.Type) {
		case "bool":
		case "int8", "int16", "int32", "int64":
		case "uint8", "uint16", "uint32", "uint64":
		case "float32", "float64":
		case "raw.Time", "raw.Duration":
		case "raw.String":
		default:
			return false
		}
	}
	return true
}

// tostr converts a node to a string.
func tostr(node ast.Node) string {
	switch node := node.(type) {
	case *ast.Ident:
		return node.Name
	case *ast.SelectorExpr:
		return tostr(node.X) + "." + tostr(node.Sel)
	}
	return ""
}

func tocamelcase(s string) string {
	if s == "" {
		return s
	}
	return string(unicode.ToUpper(rune(s[0]))) + string(s[1:])
}

func trace(v ...interface{}) {
	if *verbose {
		log.Print(v...)
	}
}

func tracef(format string, v ...interface{}) {
	if *verbose {
		log.Printf(format, v...)
	}
}

func traceln(v ...interface{}) {
	if *verbose {
		log.Println(v...)
	}
}
