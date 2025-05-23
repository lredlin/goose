// Package goose implements conversion from Go source to Perennial definitions.
//
// The exposed interface allows converting individual files as well as whole
// packages to a single Coq Ast with all the converted definitions, which
// include user-defined structs in Go as Coq records and a Perennial procedure
// for each Go function.
//
// See the Goose README at https://github.com/goose-lang/goose for a high-level
// overview. The source also has some design documentation at
// https://github.com/goose-lang/goose/tree/master/docs.
package goose

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/printer"
	"go/token"
	"go/types"
	"strconv"
	"strings"
	"unicode"

	"github.com/goose-lang/goose/internal/coq"
	"golang.org/x/tools/go/packages"
)

const preludeImport string = `
From Perennial.goose_lang Require Import prelude.
`

const basePreludeImport string = `
From Perennial.goose_lang Require Import base_prelude.
`

// Ctx is a context for resolving Go code's types and source code
type Ctx struct {
	idents  identCtx
	info    *types.Info
	Fset    *token.FileSet
	pkgPath string
	errorReporter
	PkgConfig

	dep *depTracker
}

// ExprValUsage says how the result of the currently generated expression will be used
type ExprValUsage int

const (
	// ExprValLocal means result of this expression will only be used locally,
	// or entirely discarded
	ExprValLocal ExprValUsage = iota
	// ExprValReturned means the result of this expression will be returned from
	// the current function (i.e., the "early return" control effect is
	// available here)
	ExprValReturned
	// ExprValLoop the result of this expression will control the current loop
	// (i.e., the "break/continue" control effect is available here)
	ExprValLoop
)

// PkgConfig holds package configuration for Coq conversion
type PkgConfig struct {
	TranslationConfig
	Ffi     string
	Prelude string
}

func getFfi(pkg *packages.Package) string {
	seenFfis := make(map[string]struct{})
	packages.Visit([]*packages.Package{pkg},
		func(pkg *packages.Package) bool {
			// the dependencies of an FFI are not considered as being used; this
			// allows one FFI to be built on top of another
			if _, ok := ffiMapping[pkg.PkgPath]; ok {
				return false
			}
			return true
		},
		func(pkg *packages.Package) {
			if ffi, ok := ffiMapping[pkg.PkgPath]; ok {
				seenFfis[ffi] = struct{}{}
			}
		},
	)

	if len(seenFfis) > 1 {
		panic(fmt.Sprintf("multiple ffis used %v", seenFfis))
	}
	for ffi := range seenFfis {
		return ffi
	}
	return "none"
}

// Get the prelude import, which in most cases will include both the base prelude and the prelude for translated Go models. When we are translating the Go models themselves, we include only the base prelude so we don't create a circular dependency.
func getPrelude(pkgPath string) string {
	if isTranslatedPreludeFile[pkgPath] {
		return basePreludeImport
	}
	return preludeImport
}

// NewPkgCtx initializes a context based on a properly loaded package
func NewPkgCtx(pkg *packages.Package, tr TranslationConfig) Ctx {
	// Figure out which FFI we're using
	config := PkgConfig{
		TranslationConfig: tr,
		Ffi:               getFfi(pkg),
		Prelude:           getPrelude(pkg.PkgPath),
	}

	return Ctx{
		idents:        newIdentCtx(),
		info:          pkg.TypesInfo,
		Fset:          pkg.Fset,
		pkgPath:       pkg.PkgPath,
		errorReporter: newErrorReporter(pkg.Fset),
		PkgConfig:     config,
	}
}

// NewCtx loads a context for files passed directly,
// rather than loaded from a packages.
//
// NOTE: this is only used to load the negative tests by file; prefer to use
// NewPkgCtx and let [packages.Load] load and type check the Go code.
func NewCtx(pkgPath string, conf PkgConfig) Ctx {
	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
		// TODO: these instances give the generic arguments of function
		//  calls, use those
		Instances: make(map[*ast.Ident]types.Instance),
		Types:     make(map[ast.Expr]types.TypeAndValue),
		Scopes:    make(map[ast.Node]*types.Scope),
	}
	fset := token.NewFileSet()
	return Ctx{
		idents:        newIdentCtx(),
		info:          info,
		Fset:          fset,
		pkgPath:       pkgPath,
		errorReporter: newErrorReporter(fset),
		PkgConfig:     conf,
	}
}

// TypeCheck type-checks a set of files and stores the result in the Ctx
//
// NOTE: this is only needed when using NewCtx in the negative tests, which load
// individual files rather than a package.
func (ctx Ctx) TypeCheck(files []*ast.File) error {
	imp := importer.ForCompiler(ctx.Fset, "source", nil)
	conf := types.Config{Importer: imp}
	_, err := conf.Check(ctx.pkgPath, ctx.Fset, files, ctx.info)
	return err
}

func (ctx Ctx) where(node ast.Node) string {
	return ctx.Fset.Position(node.Pos()).String()
}

func (ctx Ctx) printGo(node ast.Node) string {
	var what bytes.Buffer
	err := printer.Fprint(&what, ctx.Fset, node)
	if err != nil {
		panic(err.Error())
	}
	return what.String()
}

func (ctx Ctx) field(f *ast.Field) coq.FieldDecl {
	if len(f.Names) > 1 {
		ctx.futureWork(f, "multiple fields for same type (split them up)")
		return coq.FieldDecl{}
	}
	if len(f.Names) == 0 {
		ctx.unsupported(f, "unnamed field/parameter")
		return coq.FieldDecl{}
	}
	return coq.FieldDecl{
		Name: f.Names[0].Name,
		Type: ctx.coqType(f.Type),
	}
}

func (ctx Ctx) paramList(fs *ast.FieldList) []coq.FieldDecl {
	var decls []coq.FieldDecl
	for _, f := range fs.List {
		ty := ctx.coqType(f.Type)
		for _, name := range f.Names {
			decls = append(decls, coq.FieldDecl{
				Name: name.Name,
				Type: ty,
			})
		}
		if len(f.Names) == 0 { // Unnamed parameter
			decls = append(decls, coq.FieldDecl{
				Name: "",
				Type: ty,
			})
		}
	}
	return decls
}

func (ctx Ctx) typeParamList(fs *ast.FieldList) []coq.TypeIdent {
	var typeParams []coq.TypeIdent
	if fs == nil {
		return nil
	}
	for _, f := range fs.List {
		for _, name := range f.Names {
			typeParams = append(typeParams, coq.TypeIdent(name.Name))
		}
		if len(f.Names) == 0 { // Unnamed parameter
			ctx.unsupported(fs, "unnamed type parameters")
		}
	}
	return typeParams
}

func (ctx Ctx) structFields(fs *ast.FieldList) []coq.FieldDecl {
	var decls []coq.FieldDecl
	for _, f := range fs.List {
		if len(f.Names) > 1 {
			ctx.futureWork(f, "multiple fields for same type (split them up)")
			return nil
		}
		if len(f.Names) == 0 {
			ctx.unsupported(f, "unnamed (embedded) field")
			return nil
		}
		ty := ctx.coqType(f.Type)
		decls = append(decls, coq.FieldDecl{
			Name: f.Names[0].Name,
			Type: ty,
		})
	}
	return decls
}

// Get the type parameters for a struct declaration. This is used to create a descriptor
// function that takes type parameters as arguments.
func (ctx Ctx) structTypeParams(fs *ast.FieldList) []coq.TypeIdent {
	var typeParams []coq.TypeIdent
	if fs == nil {
		return nil
	}
	for _, f := range fs.List {
		for _, name := range f.Names {
			typeParams = append(typeParams, coq.TypeIdent(name.Name))
		}
		if len(f.Names) == 0 { // Unnamed parameter
			ctx.unsupported(fs, "unnamed type parameters")
		}
	}
	return typeParams
}

// This function uses DFS on the tree of generic struct fields to construct a struct coq type
// which is represented as a tree where nodes store the name of the struct and a list of the
// coq types of type params for generic structs. So for non-generic structs, the coq type will
// only store the name.
func getStructType(ctx Ctx, curr_type types.Type) coq.Type {
	if t, ok := curr_type.(*types.Named); ok {
		name := ctx.qualifiedName(t.Obj())
		var children []coq.Type
		// Recursive case: generic struct, call on each type parameter.
		if t.TypeParams() != nil {
			for i := 0; i < t.TypeParams().Len(); i++ {
				children = append(children, getStructType(ctx, t.TypeArgs().At(i)))
			}
		}
		return coq.StructType{Name: name, TypeParams: children}
	}
	// Base case: type is not a struct, just get the coq type.
	return ctx.coqTypeOfType(nil, curr_type)

}

func addSourceDoc(doc *ast.CommentGroup, comment *string) {
	if doc == nil {
		return
	}
	if *comment != "" {
		*comment += "\n\n"
	}
	*comment += strings.TrimSuffix(doc.Text(), "\n")
}

func (ctx Ctx) addSourceFile(node ast.Node, comment *string) {
	if !ctx.AddSourceFileComments {
		return
	}
	if *comment != "" {
		*comment += "\n\n   "
	}
	*comment += fmt.Sprintf("go: %s", ctx.where(node))
}

func (ctx Ctx) typeDecl(doc *ast.CommentGroup, spec *ast.TypeSpec) coq.Decl {
	switch goTy := spec.Type.(type) {
	case *ast.StructType:
		ty := coq.StructDecl{
			Name: spec.Name.Name,
		}
		addSourceDoc(doc, &ty.Comment)
		ctx.addSourceFile(spec, &ty.Comment)
		ty.Fields = ctx.structFields(goTy.Fields)
		// This is a generic struct and we must keep track of type parameters.
		if spec.TypeParams != nil {
			ty.TypeParameters = ctx.structTypeParams(spec.TypeParams)
		}
		return ty
	case *ast.InterfaceType:
		ty := coq.InterfaceDecl{
			Name: spec.Name.Name,
		}
		addSourceDoc(doc, &ty.Comment)
		ctx.addSourceFile(spec, &ty.Comment)
		ty.Methods = ctx.structFields(goTy.Methods)
		return ty
	default:
		if spec.TypeParams != nil {
			ctx.futureWork(spec, "generic named type only allowed for generic struct")
		}
		if spec.Assign == 0 {
			return coq.TypeDef{
				Name: spec.Name.Name,
				Type: ctx.coqType(spec.Type),
			}
		} else {
			return coq.AliasDecl{
				Name: spec.Name.Name,
				Type: ctx.coqType(spec.Type),
			}
		}
	}
}

func toInitialLower(s string) string {
	pastFirstLetter := false
	return strings.Map(func(r rune) rune {
		if !pastFirstLetter {
			newR := unicode.ToLower(r)
			pastFirstLetter = true
			return newR
		}
		return r
	}, s)
}

func (ctx Ctx) lenExpr(e *ast.CallExpr) coq.CallExpr {
	x := e.Args[0]
	xTy := ctx.typeOf(x)
	switch ty := xTy.Underlying().(type) {
	case *types.Slice:
		return coq.NewCallExpr(coq.GallinaIdent("slice.len"), ctx.expr(x))
	case *types.Map:
		return coq.NewCallExpr(coq.GallinaIdent("MapLen"), ctx.expr(x))
	case *types.Basic:
		if ty.Kind() == types.String {
			return coq.NewCallExpr(coq.GallinaIdent("StringLength"), ctx.expr(x))
		}
	}
	ctx.unsupported(e, "length of object of type %v", xTy)
	return coq.CallExpr{}
}

func (ctx Ctx) capExpr(e *ast.CallExpr) coq.CallExpr {
	x := e.Args[0]
	xTy := ctx.typeOf(x)
	switch xTy.Underlying().(type) {
	case *types.Slice:
		return coq.NewCallExpr(coq.GallinaIdent("slice.cap"), ctx.expr(x))
	}
	ctx.unsupported(e, "capacity of object of type %v", xTy)
	return coq.CallExpr{}
}

func (ctx Ctx) lockMethod(f *ast.SelectorExpr) coq.CallExpr {
	l := ctx.expr(f.X)
	switch f.Sel.Name {
	case "Lock":
		return coq.NewCallExpr(coq.GallinaIdent("Mutex__Lock"), l)
	case "Unlock":
		return coq.NewCallExpr(coq.GallinaIdent("Mutex__Unlock"), l)
	case "TryLock":
		return coq.NewCallExpr(coq.GallinaIdent("Mutex__TryLock"), l)
	default:
		ctx.nope(f, "method %s of sync.Mutex", ctx.printGo(f))
		return coq.CallExpr{}
	}
}

func (ctx Ctx) rwLockMethod(f *ast.SelectorExpr) coq.CallExpr {
	l := ctx.expr(f.X)
	switch f.Sel.Name {
	case "Lock":
		return coq.NewCallExpr(coq.GallinaIdent("RWMutex__Lock"), l)
	case "RLock":
		return coq.NewCallExpr(coq.GallinaIdent("RWMutex__RLock"), l)
	case "RUnlock":
		return coq.NewCallExpr(coq.GallinaIdent("RWMutex__RUnlock"), l)
	case "TryLock":
		return coq.NewCallExpr(coq.GallinaIdent("RWMutex__TryLock"), l)
	case "TryRLock":
		return coq.NewCallExpr(coq.GallinaIdent("RWMutex__TryRLock"), l)
	case "Unlock":
		return coq.NewCallExpr(coq.GallinaIdent("RWMutex__Unlock"), l)
	default:
		ctx.nope(f, "method %s of sync.RWMutex", ctx.printGo(f))
		return coq.CallExpr{}
	}
}

func (ctx Ctx) condVarMethod(f *ast.SelectorExpr) coq.CallExpr {
	l := ctx.expr(f.X)
	switch f.Sel.Name {
	case "Signal":
		return coq.NewCallExpr(coq.GallinaIdent("Cond__Signal"), l)
	case "Broadcast":
		return coq.NewCallExpr(coq.GallinaIdent("Cond__Broadcast"), l)
	case "Wait":
		return coq.NewCallExpr(coq.GallinaIdent("Cond__Wait"), l)
	default:
		ctx.unsupported(f, "method %s of sync.Cond", f.Sel.Name)
		return coq.CallExpr{}
	}
}

func (ctx Ctx) waitGroupMethod(f *ast.SelectorExpr, args []ast.Expr) coq.CallExpr {
	callArgs := append([]ast.Expr{f.X}, args...)
	switch f.Sel.Name {
	case "Add":
		return ctx.newCoqCall("waitgroup.Add", callArgs)
	case "Done":
		return ctx.newCoqCall("waitgroup.Done", callArgs)
	case "Wait":
		return ctx.newCoqCall("waitgroup.Wait", callArgs)
	default:
		ctx.unsupported(f, "method %s of sync.WaitGroup", f.Sel.Name)
		return coq.CallExpr{}
	}
}

func (ctx Ctx) prophIdMethod(f *ast.SelectorExpr, args []ast.Expr) coq.CallExpr {
	callArgs := append([]ast.Expr{f.X}, args...)
	switch f.Sel.Name {
	case "ResolveBool", "ResolveU64":
		return ctx.newCoqCall("ResolveProph", callArgs)
	default:
		ctx.unsupported(f, "method %s of primitive.ProphId", f.Sel.Name)
		return coq.CallExpr{}
	}
}

func (ctx Ctx) packageMethod(f *ast.SelectorExpr,
	call *ast.CallExpr) coq.Expr {
	args := call.Args
	// TODO: replace this with an import that has all the right definitions with
	// names that match Go
	if isIdent(f.X, "filesys") {
		return ctx.newCoqCall("FS."+toInitialLower(f.Sel.Name), args)
	}
	if isIdent(f.X, "disk") {
		return ctx.newCoqCall("disk."+f.Sel.Name, args)
	}
	if isIdent(f.X, "atomic") {
		return ctx.newCoqCall("atomic."+f.Sel.Name, args)
	}
	if isIdent(f.X, "machine") || isIdent(f.X, "primitive") {
		switch f.Sel.Name {
		case "UInt64Get", "UInt64Put", "UInt32Get", "UInt32Put":
			return ctx.newCoqCall(f.Sel.Name, args)
		case "RandomUint64":
			return ctx.newCoqCall("rand.RandomUint64", args)
		case "UInt64ToString":
			return ctx.newCoqCall("uint64_to_string", args)
		case "Linearize":
			return coq.GallinaIdent("Linearize")
		case "Assume":
			return ctx.newCoqCall("control.impl.Assume", args)
		case "Assert":
			return ctx.newCoqCall("control.impl.Assert", args)
		case "Exit":
			return ctx.newCoqCall("control.impl.Exit", args)
		case "WaitTimeout":
			return ctx.newCoqCall("lock.condWaitTimeout", args)
		case "Sleep":
			return ctx.newCoqCall("time.Sleep", args)
		case "TimeNow":
			return ctx.newCoqCall("time.TimeNow", args)
		case "MapClear":
			return ctx.newCoqCall("MapClear", args)
		case "NewProph":
			return ctx.newCoqCall("NewProph", args)
		default:
			ctx.futureWork(f, "unhandled call to primitive.%s", f.Sel.Name)
			return coq.CallExpr{}
		}
	}
	if isIdent(f.X, "log") {
		switch f.Sel.Name {
		case "Print", "Printf", "Println":
			return coq.LoggingStmt{GoCall: ctx.printGo(call)}
		}
	}
	// FIXME: this hack ensures util.DPrintf runs correctly in goose-nfsd.
	//
	//  We always pass #() instead of a slice with the variadic arguments. The
	//  function is important to handle but has no observable behavior in
	//  GooseLang, so it's ok to skip the arguments.
	//
	// See https://github.com/mit-pdos/goose-nfsd/blob/master/util/util.go
	if isIdent(f.X, "util") && f.Sel.Name == "DPrintf" {
		return coq.NewCallExpr(coq.GallinaIdent("util.DPrintf"),
			ctx.expr(args[0]),
			ctx.expr(args[1]),
			coq.UnitLiteral{})
	}
	if isIdent(f.X, "fmt") {
		switch f.Sel.Name {
		case "Println", "Printf":
			return coq.LoggingStmt{GoCall: ctx.printGo(call)}
		}
	}
	if isIdent(f.X, "sync") {
		switch f.Sel.Name {
		case "NewCond":
			return ctx.newCoqCall("NewCond", args)
		}
	}
	pkg := f.X.(*ast.Ident)
	return ctx.newCoqCallTypeArgs(
		coq.GallinaIdent(coq.PackageIdent{Package: pkg.Name, Ident: f.Sel.Name}.Coq(true)),
		ctx.typeList(call, ctx.info.Instances[f.Sel].TypeArgs),
		args)
}

// typeName attempts to get the type name, but this doesn't always make sense
// (e.g., an anonymous struct)
func (ctx Ctx) typeName(n ast.Node, t types.Type) string {
	if t, ok := t.(*types.Named); ok {
		return ctx.qualifiedName(t.Obj())
	}
	if t, ok := t.(*types.Alias); ok {
		return ctx.qualifiedName(t.Obj())
	}
	ctx.errorReporter.todo(n, "don't have a name for type %v", t)
	return ""
}

func (ctx Ctx) selectorMethod(f *ast.SelectorExpr, call *ast.CallExpr) coq.Expr {
	args := call.Args
	selectorType, ok := ctx.getType(f.X)
	if !ok {
		return ctx.packageMethod(f, call)
	}
	if isLockRef(selectorType) {
		return ctx.lockMethod(f)
	}
	if isRWLockRef(selectorType) {
		return ctx.rwLockMethod(f)
	}
	if isCFMutexRef(selectorType) {
		return ctx.lockMethod(f)
	}
	if isCondVar(selectorType) {
		return ctx.condVarMethod(f)
	}
	if isWaitGroup(selectorType) {
		return ctx.waitGroupMethod(f, args)
	}
	if isProphId(selectorType) {
		return ctx.prophIdMethod(f, args)
	}
	if isDisk(selectorType) {
		method := fmt.Sprintf("disk.%s", f.Sel)
		// skip disk argument (f.X) and just pass the method arguments
		return ctx.newCoqCall(method, call.Args)
	}

	// Tricky: need the deref'd type for exact Underlying() case handling
	// and name extraction, but also need original type for knowing
	// whether to deref struct func field.
	deref := selectorType
	if pt, ok := selectorType.(*types.Pointer); ok {
		deref = pt.Elem()
	}
	switch deref.Underlying().(type) {
	case *types.Interface:
		interfaceInfo, ok := ctx.getInterfaceInfo(selectorType)
		if ok {
			callArgs := append([]ast.Expr{f.X}, args...)
			return ctx.newCoqCall(
				coq.InterfaceMethodName(interfaceInfo.name, f.Sel.Name),
				callArgs)
		}
	case *types.Struct:
		structInfo, ok := ctx.getStructInfo(selectorType)
		if !ok {
			panic("expected struct")
		}

		// see if f.Sel.Name is a struct field, and translate accordingly if so
		for _, name := range structInfo.fields() {
			if f.Sel.Name == name {
				return ctx.newCoqCallWithExpr(
					ctx.structSelector(structInfo, f),
					args)
			}
		}
	}

	tyName := ctx.typeName(f, deref)
	callArgs := append([]ast.Expr{f.X}, args...)
	var typeArgs []coq.Expr
	// Get type parameters for named structs.
	if isPointerToNamedType(selectorType) {
		NamedPointerTy := selectorType.(*types.Pointer).Elem().(*types.Named)
		typeArgs = append(typeArgs, ctx.typeList(call, NamedPointerTy.TypeArgs())...)
	}
	if isNamedType(selectorType) {
		NamedTy := selectorType.(*types.Named)
		typeArgs = append(typeArgs, ctx.typeList(call, NamedTy.TypeArgs())...)
	}
	// append the type arguments specific to this function
	typeArgs = append(typeArgs, ctx.typeList(call, ctx.info.Instances[f.Sel].TypeArgs)...)
	fullName := coq.MethodName(tyName, f.Sel.Name)
	ctx.dep.addDep(fullName)
	coqCall := ctx.coqRecurFunc(fullName, f.Sel)
	return ctx.newCoqCallTypeArgs(coqCall, typeArgs, callArgs)
}

func (ctx Ctx) newCoqCallTypeArgs(method coq.Expr, typeArgs []coq.Expr,
	es []ast.Expr) coq.CallExpr {
	var args []coq.Expr
	for _, e := range es {
		args = append(args, ctx.expr(e))
	}
	call := coq.NewCallExpr(method, args...)
	call.TypeArgs = typeArgs
	return call
}

func (ctx Ctx) newCoqCall(method string, es []ast.Expr) coq.CallExpr {
	return ctx.newCoqCallTypeArgs(coq.GallinaIdent(method), nil, es)
}

func (ctx Ctx) newCoqCallWithExpr(method coq.Expr, es []ast.Expr) coq.CallExpr {
	return ctx.newCoqCallTypeArgs(method, nil, es)
}

func (ctx Ctx) methodExpr(call *ast.CallExpr) coq.Expr {
	args := call.Args
	// discovered this API via
	// https://go.googlesource.com/example/+/HEAD/gotypes#named-types
	if ctx.info.Types[call.Fun].IsType() {
		// string -> []byte conversions are handled specially
		if f, ok := call.Fun.(*ast.ArrayType); ok {
			if f.Len == nil && isIdent(f.Elt, "byte") {
				arg := args[0]
				if isString(ctx.typeOf(arg)) {
					return ctx.newCoqCall("StringToBytes", args)
				}
			}
		}
		// []byte -> string are handled specially
		if f, ok := call.Fun.(*ast.Ident); ok && f.Name == "string" {
			arg := args[0]
			if isString(ctx.typeOf(arg).Underlying()) {
				return ctx.expr(args[0])
			}
			if !isByteSlice(ctx.typeOf(arg)) {
				ctx.unsupported(call,
					"conversion from type %v to string", ctx.typeOf(arg))
				return coq.CallExpr{}
			}
			return ctx.newCoqCall("StringFromBytes", args)
		}
		// a different type conversion, which is a noop in GooseLang (which is
		// untyped)
		// TODO: handle integer conversions here, checking if call.Fun is an integer
		//  type; see https://github.com/goose-lang/goose/issues/14
		return ctx.expr(args[0])
	}

	var retExpr coq.Expr

	f := call.Fun

	// IndexExpr and IndexListExpr represent calls like `f[T](x)`;
	// we get rid of the `[T]` since we can figure that out from the
	// ctx.info.Instances thing like we would need to for implicit type
	// arguments
	switch indexF := f.(type) {
	case *ast.IndexExpr:
		f = indexF.X
	case *ast.IndexListExpr:
		f = indexF.X
	}

	switch f := f.(type) {
	case *ast.Ident:
		typeArgs := ctx.typeList(call, ctx.info.Instances[f].TypeArgs)

		// XXX: this could be a struct field of type `func()`; right now we
		// don't support generic structs, so code with a generic function field
		// will be rejected. But, in the future, that might change.

		// We technically need to pass the channel's type in to the close function because
		// the model is made using generic structs which don't allow you to define methods
		// a struct without the type parameter.
		if f.Name == "close" {
			chan_type, ok := ctx.typeOf(args[0]).Underlying().(*types.Chan)
			if ok {
				typeArgs = append(typeArgs, ctx.coqTypeOfType(args[0], chan_type.Elem()))

			}
		}
		retExpr = ctx.newCoqCallTypeArgs(ctx.identExpr(f), typeArgs, args)
	case *ast.SelectorExpr:
		retExpr = ctx.selectorMethod(f, call)
	case *ast.IndexExpr:
		// generic type instantiation f[T]
		ctx.nope(call, "double explicit generic type instantiation")
	case *ast.IndexListExpr:
		// generic type instantiation f[T, V]
		ctx.nope(call, "double explicit generic type instantiation with multiple arguments")
	default:
		ctx.unsupported(call, "call to unexpected function (of type %T)", call.Fun)
	}

	return retExpr
}

func (ctx Ctx) makeSliceExpr(elt coq.Type, args []ast.Expr) coq.CallExpr {
	if len(args) == 2 {
		return coq.NewCallExpr(coq.GallinaIdent("NewSlice"), elt, ctx.expr(args[1]))
	} else if len(args) == 3 {
		return coq.NewCallExpr(coq.GallinaIdent("NewSliceWithCap"), elt, ctx.expr(args[1]), ctx.expr(args[2]))
	} else {
		ctx.unsupported(args[0], "Too many or too few arguments in slice construction")
		return coq.CallExpr{}
	}
}

// makeExpr parses a call to make() into the appropriate data-structure Call
func (ctx Ctx) makeExpr(args []ast.Expr) coq.CallExpr {
	switch typeArg := args[0].(type) {
	case *ast.MapType:
		mapTy := ctx.mapType(typeArg)
		return coq.NewCallExpr(coq.GallinaIdent("NewMap"), mapTy.Key, mapTy.Value, coq.UnitLiteral{})
	case *ast.ArrayType:
		if typeArg.Len != nil {
			ctx.nope(typeArg, "can't make() arrays (only slices)")
		}
		elt := ctx.coqType(typeArg.Elt)
		return ctx.makeSliceExpr(elt, args)
	}
	switch ty := ctx.typeOf(args[0]).Underlying().(type) {
	case *types.Slice:
		elt := ctx.coqTypeOfType(args[0], ty.Elem())
		return ctx.makeSliceExpr(elt, args)
	case *types.Map:
		return coq.NewCallExpr(coq.GallinaIdent("NewMap"),
			ctx.coqTypeOfType(args[0], ty.Key()),
			ctx.coqTypeOfType(args[0], ty.Elem()),
			coq.UnitLiteral{})
	case *types.Chan:
		// For unbuffered channels, we explicitly pass 0, otherwise pass the buffer size.
		buffer_size := 0
		if len(args) > 1 {
			if e, ok := args[1].(*ast.BasicLit); ok {
				if e.Kind == token.INT {
					buffer_size, _ = strconv.Atoi(e.Value)
				}
			}
		}
		return coq.NewCallExpr(coq.GallinaIdent("NewChannelRef"),
			ctx.coqTypeOfType(args[0], ty.Elem()),
			coq.IntLiteral{Value: uint64(buffer_size)})
	default:
		ctx.unsupported(args[0],
			"make type should be slice or map, got %v", ty)
	}
	return coq.CallExpr{}
}

// newExpr parses a call to new() into an appropriate allocation
func (ctx Ctx) newExpr(ty ast.Expr) coq.CallExpr {
	if sel, ok := ty.(*ast.SelectorExpr); ok {
		if isIdent(sel.X, "sync") && isIdent(sel.Sel, "Mutex") {
			return coq.NewCallExpr(coq.GallinaIdent("newMutex"))
		}
		if isIdent(sel.X, "sync") && isIdent(sel.Sel, "RWMutex") {
			return coq.NewCallExpr(coq.GallinaIdent("newRWMutex"))
		}
		if isIdent(sel.X, "sync") && isIdent(sel.Sel, "WaitGroup") {
			return coq.NewCallExpr(coq.GallinaIdent("waitgroup.New"))
		}
		if isIdent(sel.X, "cfmutex") && isIdent(sel.Sel, "CFMutex") {
			return coq.NewCallExpr(coq.GallinaIdent("newMutex"))
		}
	}
	if t, ok := ctx.typeOf(ty).(*types.Array); ok {
		return coq.NewCallExpr(coq.GallinaIdent("zero_array"),
			ctx.coqTypeOfType(ty, t.Elem()),
			coq.IntLiteral{Value: uint64(t.Len())})
	}
	e := coq.NewCallExpr(coq.GallinaIdent("zero_val"), ctx.coqType(ty))
	// check for new(T) where T is a struct, but not a pointer to a struct
	// (new(*T) should be translated to ref (zero_val ptrT) as usual,
	// a pointer to a nil pointer)
	if info, ok := ctx.getStructInfo(ctx.typeOf(ty)); ok && !info.throughPointer {
		return coq.NewCallExpr(coq.GallinaIdent("struct.alloc"), coq.StructDesc(info.structCoqType), e)
	}
	return coq.NewCallExpr(coq.GallinaIdent("ref"), e)
}

// integerConversion generates an expression for converting x to an integer
// of a specific width
//
// s is only used for error reporting
func (ctx Ctx) integerConversion(s ast.Node, x ast.Expr, width int) coq.Expr {
	if info, ok := getIntegerType(ctx.typeOf(x)); ok {
		if info.isUntyped {
			ctx.todo(s, "conversion from untyped int to uint64")
		}
		if info.width == width {
			return ctx.expr(x)
		}
		return coq.NewCallExpr(coq.GallinaIdent(fmt.Sprintf("to_u%d", width)),
			ctx.expr(x))
	}
	ctx.unsupported(s, "casts from unsupported type %v to uint%d",
		ctx.typeOf(x), width)
	return nil
}

func (ctx Ctx) copyExpr(n ast.Node, dst ast.Expr, src ast.Expr) coq.Expr {
	e := sliceElem(ctx.typeOf(dst))
	return coq.NewCallExpr(coq.GallinaIdent("SliceCopy"),
		ctx.coqTypeOfType(n, e),
		ctx.expr(dst), ctx.expr(src))
}

func (ctx Ctx) callExpr(s *ast.CallExpr) coq.Expr {
	if isIdent(s.Fun, "make") {
		return ctx.makeExpr(s.Args)
	}
	if isIdent(s.Fun, "new") {
		return ctx.newExpr(s.Args[0])
	}
	if isIdent(s.Fun, "len") {
		return ctx.lenExpr(s)
	}
	if isIdent(s.Fun, "cap") {
		return ctx.capExpr(s)
	}
	if isIdent(s.Fun, "append") {
		elemTy := sliceElem(ctx.typeOf(s.Args[0]).Underlying())
		if s.Ellipsis == token.NoPos {
			return coq.NewCallExpr(coq.GallinaIdent("SliceAppend"),
				ctx.coqTypeOfType(s, elemTy),
				ctx.expr(s.Args[0]),
				ctx.expr(s.Args[1]))
		}
		// append(s1, s2...)
		return coq.NewCallExpr(coq.GallinaIdent("SliceAppendSlice"),
			ctx.coqTypeOfType(s, elemTy),
			ctx.expr(s.Args[0]),
			ctx.expr(s.Args[1]))
	}
	if isIdent(s.Fun, "copy") {
		return ctx.copyExpr(s, s.Args[0], s.Args[1])
	}
	if isIdent(s.Fun, "delete") {
		if _, ok := ctx.typeOf(s.Args[0]).Underlying().(*types.Map); !ok {
			ctx.unsupported(s, "delete on non-map")
		}
		return coq.NewCallExpr(coq.GallinaIdent("MapDelete"), ctx.expr(s.Args[0]), ctx.expr(s.Args[1]))
	}
	if isIdent(s.Fun, "uint64") {
		return ctx.integerConversion(s, s.Args[0], 64)
	}
	if isIdent(s.Fun, "uint32") {
		return ctx.integerConversion(s, s.Args[0], 32)
	}
	if isIdent(s.Fun, "uint8") {
		return ctx.integerConversion(s, s.Args[0], 8)
	}
	if isIdent(s.Fun, "panic") {
		msg := "oops"
		if e, ok := s.Args[0].(*ast.BasicLit); ok {
			if e.Kind == token.STRING {
				v := ctx.info.Types[e].Value
				msg = constant.StringVal(v)
			}
		}
		return coq.NewCallExpr(coq.GallinaIdent("Panic"), coq.GallinaString(msg))
	}
	// Special case for *sync.NewCond
	if _, ok := s.Fun.(*ast.SelectorExpr); ok {
	} else if ctx.SkipInterfaces {
	} else {
		if signature, ok := ctx.typeOf(s.Fun).(*types.Signature); ok {
			for j := 0; j < signature.Params().Len(); j++ {
				if _, ok := signature.Params().At(j).Type().Underlying().(*types.Interface); ok {
					interfaceName := signature.Params().At(j).Type().String()
					structName := ctx.typeOf(s.Args[0]).String()
					interfaceName = unqualifyName(interfaceName)
					structName = unqualifyName(structName)
					if interfaceName != structName && interfaceName != "" && structName != "" {
						conversion := coq.StructToInterfaceDecl{
							Fun:       ctx.expr(s.Fun).Coq(true),
							Struct:    structName,
							Interface: interfaceName,
							Arg:       ctx.expr(s.Args[0]).Coq(true),
						}.Coq(true)
						for i, arg := range s.Args {
							if i > 0 {
								conversion += " " + ctx.expr(arg).Coq(true)
							}
						}
						return coq.CallExpr{MethodName: coq.GallinaIdent(conversion)}
					}
				}
			}
		}
	}
	return ctx.methodExpr(s)
}

func (ctx Ctx) qualifiedName(obj types.Object) string {
	name := obj.Name()
	if ctx.pkgPath == obj.Pkg().Path() {
		// no module name needed
		return name
	}
	return fmt.Sprintf("%s.%s", obj.Pkg().Name(), name)
}

func (ctx Ctx) selectExpr(e *ast.SelectorExpr) coq.Expr {
	selectorType, ok := ctx.getType(e.X)
	if !ok {
		if isIdent(e.X, "filesys") {
			return coq.GallinaIdent("FS." + e.Sel.Name)
		}
		if isIdent(e.X, "disk") {
			return coq.GallinaIdent("disk." + e.Sel.Name)
		}
		if pkg, ok := getIdent(e.X); ok {
			return coq.PackageIdent{
				Package: pkg,
				Ident:   e.Sel.Name,
			}
		}
	}
	structInfo, ok := ctx.getStructInfo(selectorType)

	// Check if the select expression is actually referring to a function object
	// If it is, we need to translate to 'StructName__FuncName varName' instead
	// of a struct access
	_, isFuncType := (ctx.typeOf(e)).(*types.Signature)
	if isFuncType {
		m := coq.MethodName(structInfo.structCoqType.Name, e.Sel.Name)
		ctx.dep.addDep(m)
		return coq.NewCallExpr(coq.GallinaIdent(m), ctx.expr(e.X))
	}
	if ok {
		return ctx.structSelector(structInfo, e)
	}
	ctx.unsupported(e, "unexpected select expression")
	return nil
}

func (ctx Ctx) structSelector(info structTypeInfo, e *ast.SelectorExpr) coq.StructFieldAccessExpr {
	ctx.dep.addDep(info.structCoqType.Name)
	return coq.StructFieldAccessExpr{
		Struct:         info.structCoqType,
		Field:          e.Sel.Name,
		X:              ctx.expr(e.X),
		ThroughPointer: info.throughPointer,
	}
}

func (ctx Ctx) compositeLiteral(e *ast.CompositeLit) coq.Expr {
	if _, ok := ctx.typeOf(e).Underlying().(*types.Slice); ok {
		if len(e.Elts) == 0 {
			elemTy := ctx.coqType(e.Type).(coq.SliceType).Value
			zeroLit := coq.IntLiteral{Value: 0}
			return coq.NewCallExpr(coq.GallinaIdent("NewSlice"), elemTy, zeroLit)
		}
		if len(e.Elts) == 1 {
			return ctx.newCoqCall("SliceSingleton", []ast.Expr{e.Elts[0]})
		}
		ctx.unsupported(e, "slice literal with multiple elements")
		return nil
	}
	info, ok := ctx.getStructInfo(ctx.typeOf(e))
	if ok {
		return ctx.structLiteral(info, e)
	}
	ctx.unsupported(e, "composite literal of type %v", ctx.typeOf(e))
	return nil
}

func (ctx Ctx) structLiteral(info structTypeInfo,
	e *ast.CompositeLit) coq.StructLiteral {
	ctx.dep.addDep(info.structCoqType.Name)
	lit := coq.NewStructLiteral(info.structCoqType)
	for _, el := range e.Elts {
		switch el := el.(type) {
		case *ast.KeyValueExpr:
			ident, ok := getIdent(el.Key)
			if !ok {
				ctx.noExample(el.Key, "struct field keyed by non-identifier %+v", el.Key)
				return coq.StructLiteral{}
			}
			lit.AddField(ident, ctx.expr(el.Value))
		default:
			ctx.unsupported(e,
				"un-keyed struct literal field %v", ctx.printGo(el))
		}
	}
	return lit
}

// basicLiteral parses a basic literal
//
// (unsigned) ints, strings, and booleans are supported
func (ctx Ctx) basicLiteral(e *ast.BasicLit) coq.Expr {
	if e.Kind == token.STRING {
		v := ctx.info.Types[e].Value
		s := constant.StringVal(v)
		if strings.ContainsRune(s, '"') {
			ctx.unsupported(e, "string literals with quotes")
		}
		return coq.StringLiteral{Value: s}
	}
	if e.Kind == token.INT {
		info, _ := getIntegerType(ctx.typeOf(e))
		v := ctx.info.Types[e].Value
		n, ok := constant.Uint64Val(v)
		if !ok {
			ctx.unsupported(e,
				"int literals must be positive numbers")
			return nil
		}
		if info.isUint64() {
			return coq.IntLiteral{Value: n}
		} else if info.isUint32() {
			return coq.Int32Literal{Value: uint32(n)}
		} else if info.isUint8() {
			return coq.ByteLiteral{Value: uint8(n)}
		}
	}
	ctx.unsupported(e, "literal with kind %s", e.Kind)
	return nil
}

func (ctx Ctx) isNilCompareExpr(e *ast.BinaryExpr) bool {
	if !(e.Op == token.EQL || e.Op == token.NEQ) {
		return false
	}
	return ctx.info.Types[e.Y].IsNil()
}

func (ctx Ctx) binExpr(e *ast.BinaryExpr) coq.Expr {
	op, ok := map[token.Token]coq.BinOp{
		token.LSS:  coq.OpLessThan,
		token.GTR:  coq.OpGreaterThan,
		token.SUB:  coq.OpMinus,
		token.EQL:  coq.OpEquals,
		token.NEQ:  coq.OpNotEquals,
		token.MUL:  coq.OpMul,
		token.QUO:  coq.OpQuot,
		token.REM:  coq.OpRem,
		token.LEQ:  coq.OpLessEq,
		token.GEQ:  coq.OpGreaterEq,
		token.AND:  coq.OpAnd,
		token.LAND: coq.OpLAnd,
		token.OR:   coq.OpOr,
		token.LOR:  coq.OpLOr,
		token.XOR:  coq.OpXor,
		token.SHL:  coq.OpShl,
		token.SHR:  coq.OpShr,
	}[e.Op]
	if e.Op == token.ADD {
		if isString(ctx.typeOf(e.X)) {
			op = coq.OpAppend
		} else {
			op = coq.OpPlus
		}
		ok = true
	}
	if ok {
		expr := coq.BinaryExpr{
			X:  ctx.expr(e.X),
			Op: op,
			Y:  ctx.expr(e.Y),
		}
		if ctx.isNilCompareExpr(e) {
			if _, ok := ctx.typeOf(e.X).(*types.Pointer); ok {
				expr.Y = coq.Null
			}
		}
		return expr
	}
	ctx.unsupported(e, "binary operator %v", e.Op)
	return nil
}

func (ctx Ctx) sliceExpr(e *ast.SliceExpr) coq.Expr {
	if e.Slice3 {
		ctx.unsupported(e, "3-index slice")
		return nil
	}
	if e.Max != nil {
		ctx.unsupported(e, "setting the max capacity in a slice expression is not supported")
		return nil
	}
	x := ctx.expr(e.X)
	if e.Low != nil && e.High == nil {
		return coq.NewCallExpr(coq.GallinaIdent("SliceSkip"),
			ctx.coqTypeOfType(e, sliceElem(ctx.typeOf(e.X))),
			x, ctx.expr(e.Low))
	}
	if e.Low == nil && e.High != nil {
		return coq.NewCallExpr(coq.GallinaIdent("SliceTake"),
			x, ctx.expr(e.High))
	}
	if e.Low != nil && e.High != nil {
		return coq.NewCallExpr(coq.GallinaIdent("SliceSubslice"),
			ctx.coqTypeOfType(e, sliceElem(ctx.typeOf(e.X))),
			x, ctx.expr(e.Low), ctx.expr(e.High))
	}
	if e.Low == nil && e.High == nil {
		ctx.unsupported(e, "complete slice doesn't do anything")
	}
	return nil
}

func (ctx Ctx) nilExpr(e *ast.Ident) coq.Expr {
	t := ctx.typeOf(e)
	switch t.(type) {
	case *types.Pointer:
		return coq.GallinaIdent("null")
	case *types.Slice:
		return coq.GallinaIdent("slice.nil")
	case *types.Basic:
		// TODO: this gets triggered for all of our unit tests because the
		//  nil identifier is mapped to an untyped nil object.
		//  This seems wrong; the runtime representation of each of these
		//  uses depends on the type, so Go must know how they're being used.
		return coq.GallinaIdent("slice.nil")
	default:
		ctx.unsupported(e, "nil of type %v (not pointer or slice)", t)
		return nil
	}
}

func (ctx Ctx) unaryExpr(e *ast.UnaryExpr) coq.Expr {
	if e.Op == token.NOT {
		return coq.NotExpr{X: ctx.expr(e.X)}
	}
	if e.Op == token.XOR {
		return coq.NotExpr{X: ctx.expr(e.X)}
	}
	if e.Op == token.AND {
		if x, ok := e.X.(*ast.IndexExpr); ok {
			// e is &a[b] where x is a.b
			if xTy, ok := ctx.typeOf(x.X).(*types.Slice); ok {
				return coq.NewCallExpr(coq.GallinaIdent("SliceRef"),
					ctx.coqTypeOfType(e, xTy.Elem()),
					ctx.expr(x.X), ctx.expr(x.Index))
			}
		}
		if isAtomicPointerType(ctx.typeOf(e.X)) {
			return coq.RefZeroExpr{Ty: ctx.coqTypeOfType(e, ctx.typeOf(e.X))}
		}
		if info, ok := ctx.getStructInfo(ctx.typeOf(e.X)); ok {
			structLit, ok := e.X.(*ast.CompositeLit)
			if ok {
				// e is &s{...} (a struct literal)
				sl := ctx.structLiteral(info, structLit)
				sl.Allocation = true
				return sl
			}
		}
		// e is something else
		return ctx.refExpr(e.X)
	}
	if e.Op == token.ARROW {
		// If we are using the ok variable that <- can return optionally, we have a different
		// function in the model.
		tuple_type, ok := ctx.typeOf(e).(*types.Tuple)
		if ok {
			return coq.NewCallExpr(coq.GallinaIdent("Channel__Receive"), ctx.coqTypeOfType(e, tuple_type.At(0).Type()), ctx.expr(e.X))
		} else {

			return coq.NewCallExpr(coq.GallinaIdent("Channel__ReceiveDiscardOk"), ctx.coqTypeOfType(e, ctx.typeOf(e)), ctx.expr(e.X))
		}

	}
	ctx.unsupported(e, "unary expression %s", e.Op)
	return nil
}

func (ctx Ctx) variable(s *ast.Ident) coq.Expr {
	if ctx.isGlobalVar(s) {
		ctx.dep.addDep(s.Name)
		return coq.GallinaIdent(s.Name)
	}
	e := coq.IdentExpr(s.Name)
	if ctx.isPtrWrapped(s) {
		return coq.DerefExpr{X: e, Ty: ctx.coqTypeOfType(s, ctx.typeOf(s))}
	}
	return e
}

func (ctx Ctx) coqRecurFunc(fullFuncName string, e *ast.Ident) coq.Expr {
	obj, ok := ctx.info.Uses[e]
	if !ok {
		panic("type checker doesn't have func")
	}
	// Can't get scopes for imported funcs.
	if ctx.pkgPath != obj.Pkg().Path() {
		return coq.GallinaIdent(fullFuncName)
	}
	fun := obj.(*types.Func)

	// scope is nil for instantiated functions
	if fun.Scope() != nil && fun.Scope().Contains(e.Pos()) {
		return coq.GallinaString(fullFuncName)
	} else {
		return coq.GallinaIdent(fullFuncName)
	}
}

func (ctx Ctx) function(s *ast.Ident) coq.Expr {
	ctx.dep.addDep(s.Name)
	return ctx.coqRecurFunc(s.Name, s)
}

func (ctx Ctx) goBuiltin(e *ast.Ident) bool {
	s, ok := ctx.info.Uses[e]
	if !ok {
		return false
	}
	return s.Parent() == types.Universe
}

func (ctx Ctx) identExpr(e *ast.Ident) coq.Expr {
	if ctx.goBuiltin(e) {
		switch e.Name {
		case "nil":
			return ctx.nilExpr(e)
		case "true":
			return coq.True
		case "false":
			return coq.False
		case "close":
			return coq.GallinaIdent("Channel__Close")

		}
		ctx.unsupported(e, "special identifier")
	}

	// check if e refers to a variable,
	obj := ctx.info.ObjectOf(e)
	if _, ok := obj.(*types.Const); ok {
		// is a variable
		return ctx.variable(e)
	}
	if _, ok := obj.(*types.Var); ok {
		// is a variable
		return ctx.variable(e)
	}
	if _, ok := obj.(*types.Func); ok {
		// is a function
		return ctx.function(e)
	}
	ctx.unsupported(e, "unrecognized kind of identifier; not local variable or global function")
	panic("")
}

func (ctx Ctx) indexExpr(e *ast.IndexExpr, isSpecial bool) coq.CallExpr {
	xTy := ctx.typeOf(e.X).Underlying()
	switch xTy := xTy.(type) {
	case *types.Map:
		e := coq.NewCallExpr(coq.GallinaIdent("MapGet"), ctx.expr(e.X), ctx.expr(e.Index))
		if !isSpecial {
			e = coq.NewCallExpr(coq.GallinaIdent("Fst"), e)
		}
		return e
	case *types.Slice:
		return coq.NewCallExpr(coq.GallinaIdent("SliceGet"),
			ctx.coqTypeOfType(e, xTy.Elem()),
			ctx.expr(e.X), ctx.expr(e.Index))
	}
	ctx.unsupported(e, "index into unknown type %v", xTy)
	return coq.CallExpr{}
}

func (ctx Ctx) derefExpr(e ast.Expr) coq.Expr {
	info, ok := ctx.getStructInfo(ctx.typeOf(e))
	if ok && info.throughPointer {
		return coq.NewCallExpr(coq.GallinaIdent("struct.load"),
			coq.StructDesc(info.structCoqType),
			ctx.expr(e))
	}
	return coq.DerefExpr{
		X:  ctx.expr(e),
		Ty: ctx.coqTypeOfType(e, ptrElem(ctx.typeOf(e))),
	}
}

func (ctx Ctx) expr(e ast.Expr) coq.Expr {
	return ctx.exprSpecial(e, false)
}

func (ctx Ctx) funcLit(e *ast.FuncLit) coq.FuncLit {
	fl := coq.FuncLit{}

	fl.Args = ctx.paramList(e.Type.Params)
	// fl.ReturnType = ctx.returnType(d.Type.Results)
	fl.Body = ctx.blockStmt(e.Body, ExprValReturned)
	return fl
}

func (ctx Ctx) exprSpecial(e ast.Expr, isSpecial bool) coq.Expr {
	switch e := e.(type) {
	case *ast.CallExpr:
		return ctx.callExpr(e)
	case *ast.MapType:
		return ctx.mapType(e)
	case *ast.Ident:
		return ctx.identExpr(e)
	case *ast.SelectorExpr:
		return ctx.selectExpr(e)
	case *ast.CompositeLit:
		return ctx.compositeLiteral(e)
	case *ast.BasicLit:
		return ctx.basicLiteral(e)
	case *ast.BinaryExpr:
		return ctx.binExpr(e)
	case *ast.SliceExpr:
		return ctx.sliceExpr(e)
	case *ast.IndexExpr:
		return ctx.indexExpr(e, isSpecial)
	case *ast.UnaryExpr:
		return ctx.unaryExpr(e)
	case *ast.ParenExpr:
		return ctx.expr(e.X)
	case *ast.StarExpr:
		return ctx.derefExpr(e.X)
	case *ast.TypeAssertExpr:
		// TODO: do something with the type
		return ctx.expr(e.X)
	case *ast.FuncLit:
		return ctx.funcLit(e)
	default:
		ctx.unsupported(e, "unexpected expr")
	}
	return nil
}

func (ctx Ctx) blockStmt(s *ast.BlockStmt, usage ExprValUsage) coq.BlockExpr {
	return ctx.stmts(s.List, usage)
}

type cursor struct {
	Stmts []ast.Stmt
}

// HasNext returns true if the cursor has any remaining statements
func (c *cursor) HasNext() bool {
	return len(c.Stmts) > 0
}

// Next returns the next statement. Requires that the cursor is non-empty (check with HasNext()).
func (c *cursor) Next() ast.Stmt {
	s := c.Stmts[0]
	c.Stmts = c.Stmts[1:]
	return s
}

// Remainder consumes and returns all remaining statements
func (c *cursor) Remainder() []ast.Stmt {
	s := c.Stmts
	c.Stmts = nil
	return s
}

// If this returns true, the body truly *must* always end in an early return or loop control effect.
func (ctx Ctx) endsWithReturn(s ast.Stmt) bool {
	if s == nil {
		return false
	}
	switch s := s.(type) {
	case *ast.BlockStmt:
		return ctx.stmtsEndWithReturn(s.List)
	default:
		return ctx.stmtsEndWithReturn([]ast.Stmt{s})
	}
}

func (ctx Ctx) stmtsEndWithReturn(ss []ast.Stmt) bool {
	if len(ss) == 0 {
		return false
	}
	switch s := ss[len(ss)-1].(type) {
	case *ast.ReturnStmt, *ast.BranchStmt:
		return true
	case *ast.IfStmt:
		left := ctx.endsWithReturn(s.Body)
		right := ctx.endsWithReturn(s.Else)
		return left && right // we can only return true if we *always* end in a control effect
	}
	return false
}

func (ctx Ctx) stmts(ss []ast.Stmt, usage ExprValUsage) coq.BlockExpr {
	c := &cursor{ss}
	var bindings []coq.Binding
	var finalized bool
	for c.HasNext() {
		s := c.Next()
		// ifStmt is special, it gets a chance to "wrap" the entire remainder
		// to better support early returns.
		switch s := s.(type) {
		case *ast.IfStmt:
			bindings = append(bindings, ctx.ifStmt(s, c.Remainder(), usage))
			finalized = true
		default:
			// All other statements are translated one-by-one
			if c.HasNext() {
				bindings = append(bindings, ctx.stmt(s))
			} else {
				// The last statement is special: we propagate the usage and store "finalized"
				binding, fin := ctx.stmtInBlock(s, usage)
				bindings = append(bindings, binding)
				finalized = fin
			}
		}
	}
	// Crucially, we also arrive in this branch if the list of statements is empty.
	if !finalized {
		switch usage {
		case ExprValReturned:
			bindings = append(bindings, coq.NewAnon(coq.ReturnExpr{Value: coq.Tt}))
		case ExprValLoop:
			bindings = append(bindings, coq.NewAnon(coq.LoopContinue))
		case ExprValLocal:
			// Make sure the list of bindings is not empty -- translate empty blocks to unit
			if len(bindings) == 0 {
				bindings = append(bindings, coq.NewAnon(coq.ReturnExpr{Value: coq.Tt}))
			}
		default:
			panic("bad ExprValUsage")
		}
	}
	return coq.BlockExpr{Bindings: bindings}
}

// ifStmt has special support for an early-return "then" branch; to achieve that
// it is responsible for generating the `if` statement *together with* all the statements that
// follow it in the same block.
// It is also responsible for "finalizing" the expression according to the usage.
func (ctx Ctx) ifStmt(s *ast.IfStmt, remainder []ast.Stmt, usage ExprValUsage) coq.Binding {
	// TODO: be more careful about nested if statements; if the then branch has
	//  an if statement with early return, this is probably not handled correctly.
	//  We should conservatively disallow such returns until they're properly analyzed.
	if s.Init != nil {
		ctx.unsupported(s.Init, "if statement initializations")
		return coq.Binding{}
	}
	condExpr := ctx.expr(s.Cond)
	ife := coq.IfExpr{
		Cond: condExpr,
	}
	// Extract (possibly empty) block in Else
	var Else = &ast.BlockStmt{List: []ast.Stmt{}}
	if s.Else != nil {
		switch s := s.Else.(type) {
		case *ast.BlockStmt:
			Else = s
		case *ast.IfStmt:
			// This is an "else if"
			Else.List = []ast.Stmt{s}
		default:
			panic("if statement with unexpected kind of else branch")
		}
	}

	// Supported cases are:
	// - There is no code after the conditional -- then anything goes.
	// - The "then" branch always returns, and there is no "else" branch.
	// - Neither "then" nor "else" ever return.

	if len(remainder) == 0 {
		// The remainder is empty, so just propagate the usage to both branches.
		ife.Then = ctx.blockStmt(s.Body, usage)
		ife.Else = ctx.blockStmt(Else, usage)
		return coq.NewAnon(ife)
	}

	// There is code after the conditional -- merging control flow. Let us see what we can do.
	// Determine if we want to propagate our usage (i.e. the availability of control effects)
	// to the conditional or not.
	if ctx.endsWithReturn(s.Body) {
		ife.Then = ctx.blockStmt(s.Body, usage)
		// Put trailing code into "else". This is correct because "then" will always return.
		// "else" must be empty.
		if len(Else.List) > 0 {
			ctx.futureWork(s.Else, "early return in if with an else branch")
			return coq.Binding{}
		}
		// We can propagate the usage here since the return value of this part
		// will become the return value of the entire conditional (that's why we
		// put the remainder *inside* the conditional).
		ife.Else = ctx.stmts(remainder, usage)
		return coq.NewAnon(ife)
	}

	// No early return in "then" branch; translate this as a conditional in the middle of
	// a block (not propagating the usage). This will implicitly check that
	// the two branches do not rely on control effects.
	ife.Then = ctx.blockStmt(s.Body, ExprValLocal)
	ife.Else = ctx.blockStmt(Else, ExprValLocal)
	// And translate the remainder with our usage.
	tailExpr := ctx.stmts(remainder, usage)
	// Prepend the if-then-else before the tail.
	bindings := append([]coq.Binding{coq.NewAnon(ife)}, tailExpr.Bindings...)
	return coq.NewAnon(coq.BlockExpr{Bindings: bindings})
}

func (ctx Ctx) loopVar(s ast.Stmt) (ident *ast.Ident, init coq.Expr) {
	initAssign, ok := s.(*ast.AssignStmt)
	if !ok ||
		len(initAssign.Lhs) > 1 ||
		len(initAssign.Rhs) > 1 ||
		initAssign.Tok != token.DEFINE {
		ctx.unsupported(s, "loop initialization must be a single assignment")
		return nil, nil
	}
	lhs, ok := initAssign.Lhs[0].(*ast.Ident)
	if !ok {
		ctx.nope(s, "initialization must define an identifier")
	}
	rhs := initAssign.Rhs[0]
	return lhs, ctx.expr(rhs)
}

func (ctx Ctx) forStmt(s *ast.ForStmt) coq.ForLoopExpr {
	var init = coq.NewAnon(coq.Skip)
	var ident *ast.Ident
	if s.Init != nil {
		ident, _ = ctx.loopVar(s.Init)
		ctx.setPtrWrapped(ident)
		init = ctx.stmt(s.Init)
	}
	var cond coq.Expr = coq.True
	if s.Cond != nil {
		cond = ctx.expr(s.Cond)
	}
	post := coq.Skip
	if s.Post != nil {
		postBlock := ctx.stmt(s.Post)
		if len(postBlock.Names) > 0 {
			ctx.unsupported(s.Post, "post cannot bind names")
		}
		post = postBlock.Expr
	}

	body := ctx.blockStmt(s.Body, ExprValLoop)
	return coq.ForLoopExpr{
		Init: init,
		Cond: cond,
		Post: post,
		Body: body,
	}
}

func getIdentOrAnonymous(e ast.Expr) (ident string, ok bool) {
	if e == nil {
		return "_", true
	}
	return getIdent(e)
}

func (ctx Ctx) mapRangeStmt(s *ast.RangeStmt) coq.Expr {
	key, ok := getIdentOrAnonymous(s.Key)
	if !ok {
		ctx.nope(s.Key, "range with non-ident key")
		return nil
	}
	val, ok := getIdentOrAnonymous(s.Value)
	if !ok {
		ctx.nope(s.Value, "range with non-ident value")
		return nil
	}
	return coq.MapIterExpr{
		KeyIdent:   key,
		ValueIdent: val,
		Map:        ctx.expr(s.X),
		Body:       ctx.blockStmt(s.Body, ExprValLocal),
	}
}

func getIdentOrNil(e ast.Expr) *ast.Ident {
	if id, ok := e.(*ast.Ident); ok {
		return id
	}
	return nil
}

func (ctx Ctx) identBinder(id *ast.Ident) coq.Binder {
	if id == nil {
		return coq.Binder(nil)
	}
	e := coq.IdentExpr(id.Name)
	return &e
}

func (ctx Ctx) sliceRangeStmt(s *ast.RangeStmt) coq.Expr {
	key := getIdentOrNil(s.Key)
	val := getIdentOrNil(s.Value)
	return coq.SliceLoopExpr{
		Key:   ctx.identBinder(key),
		Val:   ctx.identBinder(val),
		Slice: ctx.expr(s.X),
		Ty:    ctx.coqTypeOfType(s.X, sliceElem(ctx.typeOf(s.X).Underlying())),
		Body:  ctx.blockStmt(s.Body, ExprValLocal),
	}
}

func (ctx Ctx) rangeStmt(s *ast.RangeStmt) coq.Expr {
	switch ctx.typeOf(s.X).Underlying().(type) {
	case *types.Map:
		return ctx.mapRangeStmt(s)
	case *types.Slice:
		return ctx.sliceRangeStmt(s)
	case *types.Chan:
		ctx.todo(s, "implement channel range for loop")
		return nil
	default:
		ctx.unsupported(s,
			"range over %v (only maps and slices are supported)",
			ctx.typeOf(s.X))
		return nil
	}
}

func (ctx Ctx) referenceTo(rhs ast.Expr) coq.Expr {
	return coq.RefExpr{
		X:  ctx.expr(rhs),
		Ty: ctx.coqTypeOfType(rhs, ctx.typeOf(rhs)),
	}
}

func (ctx Ctx) defineStmt(s *ast.AssignStmt) coq.Binding {
	if len(s.Rhs) > 1 {
		ctx.futureWork(s, "multiple defines (split them up)")
	}
	rhs := s.Rhs[0]
	// TODO: go only requires one of the variables being defined to be fresh;
	//  the rest are assigned. We should probably support re-assignment
	//  generally. The problem is re-assigning variables in a loop that were
	//  defined outside the loop, which in Go propagates to subsequent
	//  iterations, so we can just conservatively disallow assignments within
	//  loop bodies.

	var idents []*ast.Ident
	for _, lhsExpr := range s.Lhs {
		if ident, ok := lhsExpr.(*ast.Ident); ok {
			idents = append(idents, ident)
		} else {
			ctx.nope(lhsExpr, "defining a non-identifier")
		}
	}
	var names []string
	for _, ident := range idents {
		names = append(names, ident.Name)
	}
	// NOTE: this checks whether the identifier being defined is supposed to be
	// 	pointer wrapped, so to work correctly the caller must set this identInfo
	// 	before processing the defining expression.
	if len(idents) == 1 && ctx.isPtrWrapped(idents[0]) {
		return coq.Binding{Names: names, Expr: ctx.referenceTo(rhs)}
	} else {
		return coq.Binding{Names: names, Expr: ctx.exprSpecial(rhs, len(idents) == 2)}
	}
}

func (ctx Ctx) varSpec(s *ast.ValueSpec) coq.Binding {
	if len(s.Names) > 1 {
		ctx.unsupported(s, "multiple declarations in one block")
	}
	lhs := s.Names[0]
	ctx.setPtrWrapped(lhs)
	var rhs coq.Expr
	if len(s.Values) == 0 {
		ty := ctx.typeOf(lhs)
		rhs = coq.RefZeroExpr{Ty: ctx.coqTypeOfType(s, ty)}
	} else {
		rhs = ctx.referenceTo(s.Values[0])
	}
	return coq.Binding{
		Names: []string{lhs.Name},
		Expr:  rhs,
	}
}

// varDeclStmt translates declarations within functions
func (ctx Ctx) varDeclStmt(s *ast.DeclStmt) coq.Binding {
	decl, ok := s.Decl.(*ast.GenDecl)
	if !ok {
		ctx.noExample(s, "declaration that is not a GenDecl")
	}
	if decl.Tok != token.VAR {
		ctx.unsupported(s, "non-var declaration for %v", decl.Tok)
	}
	if len(decl.Specs) > 1 {
		ctx.unsupported(s, "multiple declarations in one var statement")
	}
	// guaranteed to be a *Ast.ValueSpec due to decl.Tok
	//
	// https://golang.org/pkg/go/ast/#GenDecl
	// TODO: handle TypeSpec
	return ctx.varSpec(decl.Specs[0].(*ast.ValueSpec))
}

// refExpr translates an expression which is a pointer in Go to a GooseLang
// expr for the pointer itself (whereas ordinarily it would be implicitly loaded)
//
// TODO: integrate this into the reference-of, store, and load code
//
//	note that we will no longer special-case when the reference is to a
//	basic value and will use generic type-based support in Coq,
//	hence on the Coq side we'll always have to reduce type-based loads and
//	stores when they end up loading single-word values.
func (ctx Ctx) refExpr(s ast.Expr) coq.Expr {
	switch s := s.(type) {
	case *ast.Ident:
		// this is the intended translation even if s is pointer-wrapped
		return coq.IdentExpr(s.Name)
	case *ast.SelectorExpr:
		ty := ctx.typeOf(s.X)
		info, ok := ctx.getStructInfo(ty)
		if !ok {
			ctx.unsupported(s,
				"reference to selector from non-struct type %v", ty)
		}
		fieldName := s.Sel.Name

		var structExpr coq.Expr
		if info.throughPointer {
			structExpr = ctx.expr(s.X)
		} else {
			structExpr = ctx.refExpr(s.X)
		}
		return coq.NewCallExpr(coq.GallinaIdent("struct.fieldRef"), coq.StructDesc(info.structCoqType),
			coq.GallinaString(fieldName), structExpr)
	// TODO: should move support for slice indexing here as well
	default:
		ctx.futureWork(s, "reference to other types of expressions")
		return nil
	}
}

func (ctx Ctx) pointerAssign(dst *ast.Ident, x coq.Expr) coq.Binding {
	ty := ctx.typeOf(dst)
	return coq.NewAnon(coq.StoreStmt{
		Dst: coq.IdentExpr(dst.Name),
		X:   x,
		Ty:  ctx.coqTypeOfType(dst, ty),
	})
}

func (ctx Ctx) assignFromTo(s ast.Node,
	lhs ast.Expr, rhs coq.Expr) coq.Binding {
	// assignments can mean various things
	switch lhs := lhs.(type) {
	case *ast.Ident:
		if lhs.Name == "_" {
			return coq.NewAnon(rhs)
		}
		if ctx.isPtrWrapped(lhs) {
			return ctx.pointerAssign(lhs, rhs)
		}
		ctx.unsupported(s, "variable %s is not assignable\n\t(declare it with 'var' to pointer-wrap in GooseLang and support re-assignment)", lhs.Name)
	case *ast.IndexExpr:
		targetTy := ctx.typeOf(lhs.X).Underlying()
		switch targetTy := targetTy.(type) {
		case *types.Slice:
			value := rhs
			return coq.NewAnon(coq.NewCallExpr(
				coq.GallinaIdent("SliceSet"),
				ctx.coqTypeOfType(lhs, targetTy.Elem()),
				ctx.expr(lhs.X),
				ctx.expr(lhs.Index),
				value))
		case *types.Map:
			value := rhs
			return coq.NewAnon(coq.NewCallExpr(
				coq.GallinaIdent("MapInsert"),
				ctx.expr(lhs.X),
				ctx.expr(lhs.Index),
				value))
		default:
			ctx.unsupported(s, "index update to unexpected target of type %v", targetTy)
		}
	case *ast.StarExpr:
		info, ok := ctx.getStructInfo(ctx.typeOf(lhs.X))
		if ok && info.throughPointer {
			return coq.NewAnon(coq.NewCallExpr(coq.GallinaIdent("struct.store"),
				coq.StructDesc(info.structCoqType),
				ctx.expr(lhs.X),
				rhs))
		}
		dstPtrTy, ok := ctx.typeOf(lhs.X).Underlying().(*types.Pointer)
		if !ok {
			ctx.unsupported(s,
				"could not identify element type of assignment through pointer")
		}
		return coq.NewAnon(coq.StoreStmt{
			Dst: ctx.expr(lhs.X),
			Ty:  ctx.coqTypeOfType(s, dstPtrTy.Elem()),
			X:   rhs,
		})
	case *ast.SelectorExpr:
		ty := ctx.typeOf(lhs.X)
		info, ok := ctx.getStructInfo(ty)
		var structExpr coq.Expr
		// TODO: this adjusts for pointer-wrapping in refExpr, but there should
		//  be a more systematic way to think about this (perhaps in terms of
		//  distinguishing between translating for lvalues and rvalue)
		if info.throughPointer {
			structExpr = ctx.expr(lhs.X)
		} else {
			structExpr = ctx.refExpr(lhs.X)
		}
		if ok {
			fieldName := lhs.Sel.Name
			return coq.NewAnon(coq.NewCallExpr(coq.GallinaIdent("struct.storeF"),
				coq.StructDesc(info.structCoqType),
				coq.GallinaString(fieldName),
				structExpr,
				rhs))
		}
		ctx.unsupported(s,
			"assigning to field of non-struct type %v", ty)
	default:
		ctx.unsupported(s, "assigning to complex expression")
	}
	return coq.Binding{}
}

func (ctx Ctx) multipleAssignStmt(s *ast.AssignStmt) coq.Binding {
	// Translates a, b, c = SomeCall(args)
	// into
	//
	// {
	//   ret1, ret2, ret3 := SomeCall(args)
	//   a = ret1
	//   b = ret2
	//   c = ret3
	// }
	//
	// Returns multiple bindings, since there are multiple statements

	if len(s.Rhs) > 1 {
		ctx.unsupported(s, "multiple assignments on right hand side")
	}
	rhs := ctx.expr(s.Rhs[0])

	if s.Tok != token.ASSIGN {
		// This should be invalid Go syntax anyway
		ctx.unsupported(s, "%v multiple assignment", s.Tok)
	}

	names := make([]string, len(s.Lhs))
	for i := 0; i < len(names); i += 1 {
		names[i] = fmt.Sprintf("%d_ret", i)
	}
	multipleRetBinding := coq.Binding{Names: names, Expr: rhs}

	coqStmts := make([]coq.Binding, len(s.Lhs)+1)
	coqStmts[0] = multipleRetBinding

	for i, name := range names {
		coqStmts[i+1] = ctx.assignFromTo(s, s.Lhs[i], coq.IdentExpr(name))
	}
	return coq.Binding{Names: make([]string, 0), Expr: coq.BlockExpr{Bindings: coqStmts}}
}

func (ctx Ctx) assignStmt(s *ast.AssignStmt) coq.Binding {
	if s.Tok == token.DEFINE {
		return ctx.defineStmt(s)
	}
	if len(s.Lhs) > 1 {
		return ctx.multipleAssignStmt(s)
	}
	lhs := s.Lhs[0]
	rhs := ctx.expr(s.Rhs[0])
	assignOps := map[token.Token]coq.BinOp{
		token.ADD_ASSIGN: coq.OpPlus,
		token.SUB_ASSIGN: coq.OpMinus,
		token.OR_ASSIGN:  coq.OpOr,
		token.AND_ASSIGN: coq.OpAnd,
		token.XOR_ASSIGN: coq.OpXor,
	}
	if op, ok := assignOps[s.Tok]; ok {
		rhs = coq.BinaryExpr{
			X:  ctx.expr(lhs),
			Op: op,
			Y:  rhs,
		}
	} else if s.Tok != token.ASSIGN {
		ctx.unsupported(s, "%v assignment", s.Tok)
	}
	return ctx.assignFromTo(s, lhs, rhs)
}

func (ctx Ctx) sendStmt(s *ast.SendStmt) coq.Expr {
	return coq.NewCallExpr(coq.GallinaIdent("Channel__Send"), ctx.coqTypeOfType(s.Value, ctx.typeOf(s.Value)), ctx.expr(s.Chan), ctx.expr(s.Value))
}

func (ctx Ctx) incDecStmt(stmt *ast.IncDecStmt) coq.Binding {
	ident := getIdentOrNil(stmt.X)
	if ident == nil {
		ctx.todo(stmt, "cannot inc/dec non-var")
		return coq.Binding{}
	}
	if !ctx.isPtrWrapped(ident) {
		// should also be able to support variables that are of pointer type
		ctx.todo(stmt, "can only inc/dec pointer-wrapped variables")
	}
	op := coq.OpPlus
	if stmt.Tok == token.DEC {
		op = coq.OpMinus
	}
	return ctx.pointerAssign(ident, coq.BinaryExpr{
		X:  ctx.expr(stmt.X),
		Op: op,
		Y:  coq.IntLiteral{Value: 1},
	})
}

func (ctx Ctx) spawnExpr(thread ast.Expr) coq.SpawnExpr {
	f, ok := thread.(*ast.FuncLit)
	if !ok {
		ctx.futureWork(thread,
			"only function literal spawns are supported")
		return coq.SpawnExpr{}
	}
	return coq.SpawnExpr{Body: ctx.blockStmt(f.Body, ExprValLocal)}
}

func (ctx Ctx) branchStmt(s *ast.BranchStmt) coq.Expr {
	if s.Tok == token.CONTINUE {
		return coq.LoopContinue
	}
	if s.Tok == token.BREAK {
		return coq.LoopBreak
	}
	ctx.noExample(s, "unexpected control flow %v in loop", s.Tok)
	return nil
}

// getSpawn returns a non-nil spawned thread if the expression is a go call
func (ctx Ctx) goStmt(e *ast.GoStmt) coq.Expr {
	if len(e.Call.Args) > 0 {
		ctx.todo(e, "go statement with parameters")
	}
	return ctx.spawnExpr(e.Call.Fun)
}

// This function also returns whether the expression has been "finalized",
// which means the usage has been taken care of. If it is not finalized,
// the caller is responsible for adding a trailing "return unit"/"continue".
func (ctx Ctx) stmtInBlock(s ast.Stmt, usage ExprValUsage) (coq.Binding, bool) {
	// First the case where the statement matches the usage, i.e. the necessary
	// control effect is actually available.
	switch usage {
	case ExprValReturned:
		s, ok := s.(*ast.ReturnStmt)
		if ok {
			return coq.NewAnon(ctx.returnExpr(s.Results)), true
		}
	case ExprValLoop:
		s, ok := s.(*ast.BranchStmt)
		if ok {
			return coq.NewAnon(ctx.branchStmt(s)), true
		}
	case ExprValLocal:
	}
	// Some statements can handle the usage themselves, and they make sure to finalize it, too
	switch s := s.(type) {
	case *ast.IfStmt:
		return ctx.ifStmt(s, []ast.Stmt{}, usage), true
	case *ast.BlockStmt:
		return coq.NewAnon(ctx.blockStmt(s, usage)), true
	}
	// For everything else, we generate the statement and possibly tell the caller
	// that this is not yet finalized.
	binding := coq.Binding{}
	switch s := s.(type) {
	case *ast.ReturnStmt:
		ctx.futureWork(s, "return in unsupported position")
	case *ast.BranchStmt:
		ctx.futureWork(s, "break/continue in unsupported position")
	case *ast.GoStmt:
		binding = coq.NewAnon(ctx.goStmt(s))
	case *ast.ExprStmt:
		binding = coq.NewAnon(ctx.expr(s.X))
	case *ast.AssignStmt:
		binding = ctx.assignStmt(s)
	case *ast.DeclStmt:
		binding = ctx.varDeclStmt(s)
	case *ast.IncDecStmt:
		binding = ctx.incDecStmt(s)
	case *ast.ForStmt:
		// note that this might be a nested loop,
		// in which case the loop var gets replaced by the inner loop.
		binding = coq.NewAnon(ctx.forStmt(s))
	case *ast.RangeStmt:
		binding = coq.NewAnon(ctx.rangeStmt(s))
	case *ast.SwitchStmt:
		ctx.todo(s, "check for switch statement")
	case *ast.TypeSwitchStmt:
		ctx.todo(s, "check for type switch statement")
	case *ast.SelectStmt:
		ctx.todo(s, "add support for select statement")
	case *ast.SendStmt:
		binding = coq.NewAnon(ctx.sendStmt(s))
	default:
		ctx.unsupported(s, "statement")
	}
	switch usage {
	case ExprValLocal:
		// Nothing to finalize.
		return binding, true
	default:
		// Finalization required
		return binding, false
	}
}

// Translate a single statement without usage (i.e., no control effects available)
func (ctx Ctx) stmt(s ast.Stmt) coq.Binding {
	// stmts takes care of finalization...
	binding, finalized := ctx.stmtInBlock(s, ExprValLocal)
	if !finalized {
		panic("ExprValLocal usage should always be finalized")
	}
	return binding
}

func (ctx Ctx) returnExpr(es []ast.Expr) coq.Expr {
	if len(es) == 0 {
		// named returns are not supported, so this must return unit
		return coq.ReturnExpr{Value: coq.UnitLiteral{}}
	}
	var exprs coq.TupleExpr
	for _, r := range es {
		exprs = append(exprs, ctx.expr(r))
	}
	return coq.ReturnExpr{Value: coq.NewTuple(exprs)}
}

// returnType converts an Ast.FuncType's Results to a Coq return type
func (ctx Ctx) returnType(results *ast.FieldList) coq.Type {
	if results == nil {
		return coq.TypeIdent("unitT")
	}
	rs := results.List
	for _, r := range rs {
		if len(r.Names) > 0 {
			ctx.unsupported(r, "named returned value")
			return coq.TypeIdent("<invalid>")
		}
	}
	var ts []coq.Type
	for _, r := range rs {
		if len(r.Names) > 0 {
			ctx.unsupported(r, "named returned value")
			return coq.TypeIdent("<invalid>")
		}
		ts = append(ts, ctx.coqType(r.Type))
	}
	return coq.NewTupleType(ts)
}

func (ctx Ctx) funcDecl(d *ast.FuncDecl) coq.FuncDecl {
	fd := coq.FuncDecl{Name: d.Name.Name, AddTypes: ctx.PkgConfig.TypeCheck,
		TypeParams: ctx.typeParamList(d.Type.TypeParams),
	}
	addSourceDoc(d.Doc, &fd.Comment)
	ctx.addSourceFile(d, &fd.Comment)

	if d.Recv != nil {
		if len(d.Recv.List) != 1 {
			ctx.nope(d, "function with multiple receivers")
		}
		rcvr := d.Recv.List[0]
		rcvrTy := rcvr.Type
		if star, ok := rcvrTy.(*ast.StarExpr); ok {
			rcvrTy = star.X
		}
		// For generic structs, get the type params
		if index_expr, ok := rcvrTy.(*ast.IndexExpr); ok {
			if type_param, ok := index_expr.Index.(*ast.Ident); ok {
				fd.TypeParams = append(fd.TypeParams, coq.TypeIdent(type_param.Name))
			}
			rcvrTy = index_expr.X
		}
		if index_list_expr, ok := rcvrTy.(*ast.IndexListExpr); ok {
			for _, typeParam := range index_list_expr.Indices {
				if ident, ok := typeParam.(*ast.Ident); ok {
					fd.TypeParams = append(fd.TypeParams, coq.TypeIdent(ident.Name))
				}
			}
			rcvrTy = index_list_expr.X
		}
		ident, ok := rcvrTy.(*ast.Ident)
		if !ok {
			ctx.unsupported(rcvr, "unexpected function receiver type: %s", ctx.printGo(rcvrTy))
		}
		fd.Name = coq.MethodName(ident.Name, d.Name.Name)
		fd.Args = append(fd.Args, ctx.field(rcvr))
	}

	fd.Args = append(fd.Args, ctx.paramList(d.Type.Params)...)
	fd.ReturnType = ctx.returnType(d.Type.Results)
	fd.Body = ctx.blockStmt(d.Body, ExprValReturned)
	ctx.dep.addName(fd.Name)
	return fd
}

func (ctx Ctx) constSpec(spec *ast.ValueSpec) coq.ConstDecl {
	ident := spec.Names[0]
	cd := coq.ConstDecl{
		Name:     ident.Name,
		AddTypes: ctx.PkgConfig.TypeCheck,
	}
	addSourceDoc(spec.Comment, &cd.Comment)
	if len(spec.Values) == 0 {
		ctx.unsupported(spec, "const with no value")
	}
	val := spec.Values[0]
	cd.Val = ctx.expr(val)
	if spec.Type == nil {
		cd.Type = ctx.coqTypeOfType(spec, ctx.typeOf(val))
	} else {
		cd.Type = ctx.coqType(spec.Type)
	}
	cd.Val = ctx.expr(spec.Values[0])
	return cd
}

func (ctx Ctx) constDecl(d *ast.GenDecl) []coq.Decl {
	var specs []coq.Decl
	for _, spec := range d.Specs {
		vs := spec.(*ast.ValueSpec)
		ctx.dep.addName(vs.Names[0].Name)
		specs = append(specs, ctx.constSpec(vs))
	}
	return specs
}

func (ctx Ctx) globalVarDecl(d *ast.GenDecl) []coq.Decl {
	// NOTE: this treats globals as constants, which is unsound but used for a
	// configurable Debug level in goose-nfsd. Configuration variables should
	// instead be treated as a non-deterministic constant, assuming they aren't
	// changed after startup.
	var specs []coq.Decl
	for _, spec := range d.Specs {
		vs := spec.(*ast.ValueSpec)
		ctx.dep.addName(vs.Names[0].Name)
		specs = append(specs, ctx.constSpec(vs))
	}
	return specs
}

func stringLitValue(lit *ast.BasicLit) string {
	if lit.Kind != token.STRING {
		panic("unexpected non-string literal")
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		panic("unexpected string literal value: " + err.Error())
	}
	return s
}

// TODO: put this in another file
var builtinImports = map[string]bool{
	"fmt":         true,
	"log":         true,
	"sync":        true,
	"sync/atomic": true,

	// TODO: minimize this list by instead adding trusted imports with the right
	// paths
	"github.com/goose-lang/goose/machine":            true,
	"github.com/goose-lang/goose/machine/async_disk": true,
	"github.com/goose-lang/goose/machine/disk":       true,
	"github.com/goose-lang/goose/machine/filesys":    true,
	"github.com/goose-lang/primitive":                true,
	"github.com/goose-lang/primitive/async_disk":     true,
	"github.com/goose-lang/primitive/disk":           true,
	"github.com/mit-pdos/gokv/grove_ffi":             true,
	"github.com/mit-pdos/gokv/time":                  true,
	"github.com/mit-pdos/vmvcc/cfmutex":              true,
}

var ffiMapping = map[string]string{
	"github.com/mit-pdos/gokv/grove_ffi":             "grove",
	"github.com/goose-lang/goose/machine/disk":       "disk",
	"github.com/goose-lang/goose/machine/async_disk": "async_disk",
	"github.com/goose-lang/primitive/disk":           "disk",
	"github.com/goose-lang/primitive/async_disk":     "async_disk",
}

var isTranslatedPreludeFile = map[string]bool{
	"github.com/goose-lang/goose/channel": true,
}

func (ctx Ctx) imports(d []ast.Spec) []coq.Decl {
	var decls []coq.Decl
	for _, s := range d {
		s := s.(*ast.ImportSpec)
		if s.Name != nil {
			ctx.unsupported(s, "renaming imports")
		}
		importPath := stringLitValue(s.Path)
		if !builtinImports[importPath] {
			// TODO: this uses the syntax of the Go import to determine the Coq
			// import, but Go packages can contain a different name than their
			// path. We can get this information by using the *types.Package
			// returned by Check (or the pkg.Types field from *packages.Package).
			pkgNameIndex := strings.LastIndex(importPath, "/") + 1
			pkgName := importPath[pkgNameIndex:]

			if strings.HasPrefix(pkgName, "trusted_") {
				decls = append(decls, coq.ImportDecl{Path: importPath, Trusted: true})
			} else {
				decls = append(decls, coq.ImportDecl{Path: importPath, Trusted: false})
			}
		}
	}
	return decls
}

func (ctx Ctx) exprInterface(cvs []coq.Decl, expr ast.Expr) []coq.Decl {
	switch f := expr.(type) {
	case *ast.UnaryExpr:
		if left, ok := f.X.(*ast.BinaryExpr); ok {
			if call, ok := left.X.(*ast.CallExpr); ok {
				cvs = ctx.callExprInterface(cvs, call)
			}
		}
	case *ast.BinaryExpr:
		if left, ok := f.X.(*ast.BinaryExpr); ok {
			if call, ok := left.X.(*ast.CallExpr); ok {
				cvs = ctx.callExprInterface(cvs, call)
			}
		}
		if right, ok := f.Y.(*ast.BinaryExpr); ok {
			if call, ok := right.X.(*ast.CallExpr); ok {
				cvs = ctx.callExprInterface(cvs, call)
			}
		}
	case *ast.CallExpr:
		cvs = ctx.callExprInterface(cvs, f)
	}
	return cvs
}

func (ctx Ctx) stmtInterface(cvs []coq.Decl, stmt ast.Stmt) []coq.Decl {
	switch f := stmt.(type) {
	case *ast.ReturnStmt:
		for _, result := range f.Results {
			cvs = ctx.exprInterface(cvs, result)
		}
		if len(f.Results) > 0 {
			if results, ok := f.Results[0].(*ast.BinaryExpr); ok {
				if call, ok := results.X.(*ast.CallExpr); ok {
					cvs = ctx.callExprInterface(cvs, call)
				}
			}
		}
	case *ast.IfStmt:
		if call, ok := f.Cond.(*ast.CallExpr); ok {
			cvs = ctx.callExprInterface(cvs, call)
		}
	case *ast.ExprStmt:
		if call, ok := f.X.(*ast.CallExpr); ok {
			cvs = ctx.callExprInterface(cvs, call)
		}
	case *ast.AssignStmt:
		if call, ok := f.Rhs[0].(*ast.CallExpr); ok {
			cvs = ctx.callExprInterface(cvs, call)
		}
	}
	return cvs
}

// TODO: this is a hack, should have a better scheme for putting
// interface/implementation types into the conversion name
func unqualifyName(name string) string {
	components := strings.Split(name, ".")
	// return strings.Join(components[1:], "")
	return components[len(components)-1]
}

func (ctx Ctx) callExprInterface(cvs []coq.Decl, r *ast.CallExpr) []coq.Decl {
	interfaceName := ""
	var methods []string
	if signature, ok := ctx.typeOf(r.Fun).(*types.Signature); ok {
		params := signature.Params()
		for j := 0; j < params.Len(); j++ {
			interfaceName = params.At(j).Type().String()
			interfaceName = unqualifyName(interfaceName)
			if v, ok := params.At(j).Type().Underlying().(*types.Interface); ok {
				for m := 0; m < v.NumMethods(); m++ {
					methods = append(methods, v.Method(m).Name())
				}
			}
		}
		for _, arg := range r.Args {
			structName := ctx.typeOf(arg).String()
			structName = unqualifyName(structName)
			if _, ok := ctx.typeOf(arg).Underlying().(*types.Struct); ok {
				cv := coq.StructToInterface{Struct: structName, Interface: interfaceName, Methods: methods}
				if len(cv.Coq(true)) > 1 && len(cv.MethodList()) > 0 {
					cvs = append(cvs, cv)
				}
			}
		}
	}
	return cvs
}

func (ctx Ctx) maybeDecls(d ast.Decl) []coq.Decl {
	switch d := d.(type) {
	case *ast.FuncDecl:
		var cvs []coq.Decl
		if !ctx.SkipInterfaces {
			if d.Body == nil {
				ctx.unsupported(d, "function declaration with no body")
			}
			for _, stmt := range d.Body.List {
				cvs = ctx.stmtInterface(cvs, stmt)
			}
		}
		fd := ctx.funcDecl(d)
		var results []coq.Decl
		if len(cvs) > 0 {
			results = append(cvs, fd)
		} else {
			results = []coq.Decl{fd}
		}
		return results
	case *ast.GenDecl:
		switch d.Tok {
		case token.IMPORT:
			return ctx.imports(d.Specs)
		case token.CONST:
			return ctx.constDecl(d)
		case token.VAR:
			return ctx.globalVarDecl(d)
		case token.TYPE:
			if len(d.Specs) > 1 {
				ctx.noExample(d, "multiple specs in a type decl")
			}
			spec := d.Specs[0].(*ast.TypeSpec)
			ctx.dep.addName(spec.Name.Name)
			ty := ctx.typeDecl(d.Doc, spec)
			return []coq.Decl{ty}
		default:
			ctx.nope(d, "unknown token type in decl")
		}
	case *ast.BadDecl:
		ctx.nope(d, "bad declaration in type-checked code")
	default:
		ctx.nope(d, "top-level decl")
	}
	return nil
}
