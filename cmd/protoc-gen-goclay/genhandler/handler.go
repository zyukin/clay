package genhandler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/build"
	"go/format"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/go-openapi/spec"
	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
	"github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway/descriptor"
	"google.golang.org/genproto/googleapis/api/annotations"
)

type Generator struct {
	options options
	reg     *descriptor.Registry
	imports []descriptor.GoPackage // common imports
}

// New returns a new generator which generates handler wrappers.
func New(reg *descriptor.Registry, opts ...Option) *Generator {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	g := &Generator{
		options: o,
		reg:     reg,
	}
	g.imports = append(g.imports,
		g.newGoPackage("context"),
		g.newGoPackage("github.com/pkg/errors"),
		g.newGoPackage("github.com/utrack/clay/transport"),
	)
	return g
}

func (g *Generator) newGoPackage(pkgPath string) descriptor.GoPackage {
	gopkg := descriptor.GoPackage{
		Path: pkgPath,
		Name: path.Base(pkgPath),
	}
	if err := g.reg.ReserveGoPackageAlias(gopkg.Name, gopkg.Path); err != nil {
		for i := 0; ; i++ {
			alias := fmt.Sprintf("%s_%d", gopkg.Name, i)
			if err := g.reg.ReserveGoPackageAlias(alias, gopkg.Path); err != nil {
				continue
			}
			gopkg.Alias = alias
			break
		}
	}
	return gopkg
}

func (g *Generator) Generate(targets []*descriptor.File) ([]*plugin.CodeGeneratorResponse_File, error) {
	var files []*plugin.CodeGeneratorResponse_File
	for _, file := range targets {
		glog.V(1).Infof("Processing %s", file.GetName())

		if len(file.Services) == 0 {
			glog.V(0).Infof("%s: %v", file.GetName(), errNoTargetService)
			continue
		}
		descCode, err := g.getDescTemplate(g.options.SwaggerDef[file.GetName()], file)

		if err != nil {
			return nil, err
		}
		formatted, err := format.Source([]byte(descCode))
		if err != nil {
			glog.Errorf("%v: %s", err, annotateString(descCode))
			return nil, err
		}
		name := file.GetName()
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		output := fmt.Sprintf("%s.pb.goclay.go", base)

		files = append(files, &plugin.CodeGeneratorResponse_File{
			Name:    proto.String(output),
			Content: proto.String(string(formatted)),
		})
		glog.V(1).Infof("Will emit %s", output)

		if g.options.Impl {
			output := fmt.Sprintf("%s.pb.impl.go", base)
			output = filepath.Clean(output)

			if !g.options.Force && fileExists(output) {
				glog.V(0).Infof("Implementation will not be emitted: file '%s' already exists", output)
				continue
			}

			implCode, err := g.getImplTemplate(file)
			if err != nil {
				return nil, err
			}
			formatted, err := format.Source([]byte(implCode))
			if err != nil {
				glog.Errorf("%v: %s", err, annotateString(implCode))
				return nil, err
			}

			files = append(files, &plugin.CodeGeneratorResponse_File{
				Name:    proto.String(output),
				Content: proto.String(string(formatted)),
			})
			glog.V(1).Infof("Will emit %s", output)
		}
	}

	return files, nil
}

func (g *Generator) getDescTemplate(swagger *spec.Swagger, f *descriptor.File) (string, error) {
	pkgSeen := make(map[string]bool)
	var imports []descriptor.GoPackage
	for _, pkg := range g.imports {
		pkgSeen[pkg.Path] = true
		imports = append(imports, pkg)
	}

	for _, pkg := range []string{
		"fmt",
		"io/ioutil",
		"strings",
		"bytes",
		"net/http",

		"github.com/utrack/clay/transport/httpruntime",
		"github.com/utrack/clay/transport/swagger",
		"github.com/utrack/clay/transport",
		"github.com/grpc-ecosystem/grpc-gateway/runtime",
		"google.golang.org/grpc",
		"github.com/go-chi/chi",
		"github.com/go-openapi/spec",
	} {
		pkgSeen[pkg] = true
		imports = append(imports, g.newGoPackage(pkg))
	}

	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			pkg := m.RequestType.File.GoPkg
			// Add request type package to imports if needed
			if m.Options == nil || !proto.HasExtension(m.Options, annotations.E_Http) ||
				pkg == f.GoPkg || pkgSeen[pkg.Path] {
				continue
			}
			pkgSeen[pkg.Path] = true
			imports = append(imports, pkg)
		}
	}
	p := param{File: f, Imports: imports}
	if swagger != nil {
		b, err := swagger.MarshalJSON()
		var buf bytes.Buffer
		err = json.Indent(&buf, b, "", "    ")
		if err != nil {
			return "", err
		}
		p.SwaggerBuffer = buf.Bytes()
	}
	return applyDescTemplate(p)
}

func (g *Generator) getImplTemplate(f *descriptor.File) (string, error) {
	pkgSeen := make(map[string]bool)
	var imports []descriptor.GoPackage
	for _, pkg := range g.imports {
		pkgSeen[pkg.Path] = true
		imports = append(imports, pkg)
	}
	for _, pkg := range []string{
		"context",
	} {
		pkgSeen[pkg] = true
		imports = append(imports, g.newGoPackage(pkg))
	}
	p := param{
		File: f,
	}
	if g.options.ImplPath != g.options.DescPath {
		pkg := filepath.Join(getRootImportPath(f), g.options.DescPath)
		pkgSeen[pkg] = true
		gopkg := g.newGoPackage(pkg)
		imports = append(imports, gopkg)
		if gopkg.Alias != "" {
			p.DescPrefix = gopkg.Alias + "."
		} else {
			p.DescPrefix = gopkg.Name + "."
		}
	}
	for _, svc := range f.Services {
		for _, m := range svc.Methods {
			pkg := m.RequestType.File.GoPkg
			// Add request type package to imports if needed
			if m.Options == nil || !proto.HasExtension(m.Options, annotations.E_Http) ||
				pkg == f.GoPkg || pkgSeen[pkg.Path] {
				continue
			}
			pkgSeen[pkg.Path] = true
			imports = append(imports, pkg)
		}
	}
	p.Imports = imports

	return applyImplTemplate(p)
}

func annotateString(str string) string {
	strs := strings.Split(str, "\n")
	for pos := range strs {
		strs[pos] = fmt.Sprintf("%v: %v", pos, strs[pos])
	}
	return strings.Join(strs, "\n")
}

func fileExists(path string) bool {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		glog.V(0).Info(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		glog.V(0).Info(err)
	}
	if _, err := os.Stat(filepath.Join(dir, path)); err == nil {
		return true
	}
	return false
}

func getRootImportPath(file *descriptor.File) string {
	goImportPath := ""
	if file.GoPkg.Path != "." {
		goImportPath = file.GoPkg.Path
	}
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		glog.V(0).Info(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		glog.V(0).Info(err)
	}
	for _, gp := range strings.Split(build.Default.GOPATH, ":") {
		agp, _ := filepath.Abs(gp)
		agp, _ = filepath.EvalSymlinks(agp)
		if strings.HasPrefix(dir, agp) {
			currentPath := strings.TrimPrefix(dir, agp+"/src/")
			if strings.HasPrefix(goImportPath, currentPath) {
				return goImportPath
			} else if goImportPath != "" {
				return filepath.Join(currentPath, goImportPath)
			} else {
				return currentPath
			}
		}
	}
	return ""
}
