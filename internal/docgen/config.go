// SPDX-FileCopyrightText: Dennis Klein <d.klein@gsi.de>
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/dennisklein/brig/internal/bytesize"
	"github.com/dennisklein/brig/internal/config"
)

// goDocs holds the doc comments of a Go package's declarations.
type goDocs struct {
	types  map[string]string
	funcs  map[string]string
	fields map[string]map[string]string // by type, then field
}

// parseGoDocs reads the doc comments of the package in dir, below the source
// tree's root.
func parseGoDocs(repo, dir string) (*goDocs, error) {
	entries, err := os.ReadDir(filepath.Join(repo, filepath.FromSlash(dir)))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var parsed []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(repo, filepath.FromSlash(dir), e.Name()), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		parsed = append(parsed, f)
	}
	d := &goDocs{types: map[string]string{}, funcs: map[string]string{}, fields: map[string]map[string]string{}}
	for _, f := range parsed {
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					d.funcs[decl.Name.Name] = decl.Doc.Text()
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					doc := ts.Doc
					if doc == nil {
						doc = decl.Doc
					}
					d.types[ts.Name.Name] = doc.Text()
					st, ok := ts.Type.(*ast.StructType)
					if !ok {
						continue
					}
					d.fields[ts.Name.Name] = map[string]string{}
					for _, fld := range st.Fields.List {
						for _, name := range fld.Names {
							d.fields[ts.Name.Name][name.Name] = fld.Doc.Text()
						}
					}
				}
			}
		}
	}
	return d, nil
}

// paragraphs reflows a doc comment into paragraphs of one line each.
func paragraphs(doc string) []string {
	var out []string
	for _, p := range strings.Split(strings.TrimSpace(doc), "\n\n") {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// describe turns a doc comment of the form "Name is the size of x" or "Name
// allows y" into a description of what Name is: "The size of x", "Allows y".
func describe(name, doc string) string {
	d := strings.Join(paragraphs(doc), " ")
	d = strings.TrimPrefix(d, name+" ")
	d = strings.TrimPrefix(strings.TrimPrefix(d, "is "), "are ")
	if d == "" {
		return d
	}
	return strings.ToUpper(d[:1]) + d[1:]
}

// firstSentenceDropped returns the doc comment without its first sentence.
func firstSentenceDropped(doc string) []string {
	ps := paragraphs(doc)
	if len(ps) == 0 {
		return nil
	}
	if _, rest, ok := strings.Cut(ps[0], ". "); ok {
		ps[0] = rest
	} else {
		ps = ps[1:]
	}
	return ps
}

// configKey is a key of config.yaml.
type configKey struct {
	path, typ, def, doc string
}

var (
	sizeType     = reflect.TypeFor[bytesize.Size]()
	durationType = reflect.TypeFor[config.Duration]()
)

// typeName names the kind of value that a field of type t holds.
func typeName(t reflect.Type) (string, error) {
	switch t {
	case sizeType:
		return "size", nil
	case durationType:
		return "duration", nil
	}
	switch t.Kind() {
	case reflect.Int:
		return "integer", nil
	case reflect.String:
		return "string", nil
	case reflect.Bool:
		return "boolean", nil
	case reflect.Slice:
		switch t.Elem().Kind() {
		case reflect.String:
			return "list of strings", nil
		case reflect.Uint16:
			return "list of port numbers", nil
		}
	}
	return "", fmt.Errorf("config.yaml has a value of type %s, which docgen does not describe", t)
}

// defaultText shows the default value v of a key.
func defaultText(v reflect.Value) string {
	switch x := v.Interface().(type) {
	case bytesize.Size:
		return code(x.String())
	case config.Duration:
		if x == 0 {
			return code("0")
		}
		return code(fmt.Sprint(x))
	case string:
		return code(x)
	case int:
		return code(fmt.Sprint(x))
	case bool:
		return code(fmt.Sprint(x))
	}
	if v.Kind() == reflect.Slice && v.Len() == 0 {
		return code("[]")
	}
	return code(fmt.Sprint(v.Interface()))
}

// minimums are the least values that config.Validate accepts for some keys.
var minimums = map[string]string{
	"defaults.cpus":      fmt.Sprint(config.MinCPUs),
	"defaults.memory":    config.MinMemory.String(),
	"defaults.root_disk": config.MinRootDisk.String(),
	"defaults.data_disk": config.MinDataDisk.String(),
}

// section is a table of keys with an introduction.
type section struct {
	name string
	doc  []string
	keys []configKey
}

// keyDocs returns the keys of the struct type t, whose values are v, below
// prefix.
func keyDocs(docs *goDocs, t reflect.Type, v reflect.Value, prefix string) ([]configKey, error) {
	var out []configKey
	for i := range t.NumField() {
		f := t.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		typ, err := typeName(f.Type)
		if err != nil {
			return nil, fmt.Errorf("%s%s: %w", prefix, tag, err)
		}
		k := configKey{path: prefix + tag, typ: typ, def: defaultText(v.Field(i))}
		k.doc = describe(f.Name, docs.fields[t.Name()][f.Name])
		if k.doc == "" {
			return nil, fmt.Errorf("%s has no doc comment", k.path)
		}
		if m, ok := minimums[k.path]; ok {
			k.doc += fmt.Sprintf(" At least %s.", m)
		}
		out = append(out, k)
	}
	return out, nil
}

// configPages writes the reference of config.yaml and of the network
// profiles from the structs and doc comments of package config.
func configPages(repo string) (files, error) {
	cfgDocs, err := parseGoDocs(repo, "internal/config")
	if err != nil {
		return nil, err
	}
	sizeDocs, err := parseGoDocs(repo, "internal/bytesize")
	if err != nil {
		return nil, err
	}
	builtin := config.Builtin()
	ct := reflect.TypeFor[config.Config]()
	cv := reflect.ValueOf(*builtin)

	var sections []section
	for i := range ct.NumField() {
		f := ct.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		st, sv := f.Type, cv.Field(i)
		prefix := tag + "."
		if st.Kind() == reflect.Map {
			st, sv, prefix = st.Elem(), reflect.New(st.Elem()).Elem(), tag+".NAME."
		}
		keys, err := keyDocs(cfgDocs, st, sv, prefix)
		if err != nil {
			return nil, err
		}
		sections = append(sections, section{name: tag, doc: paragraphs(strings.Replace(cfgDocs.types[st.Name()], "NetworkProfile ", "A network profile ", 1)), keys: keys})
	}

	var b strings.Builder
	b.WriteString(generatedMark)
	b.WriteString("\n# Configuration\n\n")
	b.WriteString("brig reads its settings from `$XDG_CONFIG_HOME/brig/config.yaml`, usually `~/.config/brig/config.yaml`. ")
	b.WriteString("[config.example.yaml](../../config.example.yaml) in the source tree sets every key.\n")
	for _, p := range firstSentenceDropped(cfgDocs.funcs["Load"]) {
		b.WriteString("\n" + escape(p) + "\n")
	}
	b.WriteString("\nAll keys are optional. A key that you leave out keeps its built-in default, shown below.\n")

	b.WriteString("\n## Value types\n\n")
	fmt.Fprintf(&b, "- **size**: %s\n", inline(describe("Parse reads", sizeDocs.funcs["Parse"])))
	fmt.Fprintf(&b, "- **duration**: %s\n", inline(describe("Duration", cfgDocs.types["Duration"])))
	b.WriteString("- **list of port numbers**: TCP ports from 1 to 65535, written as a YAML list such as `[11434, 8080]`.\n")

	for _, s := range sections {
		fmt.Fprintf(&b, "\n## %s\n\n", s.name)
		for _, p := range s.doc {
			b.WriteString(escape(p) + "\n\n")
		}
		if s.name == "network_profiles" {
			b.WriteString("[Network profiles](network-profiles.md) lists the built-in profiles and what the keys mean in practice. `NAME` is the name of a profile.\n\n")
		}
		b.WriteString("| Key | Type | Default | Description |\n| --- | --- | --- | --- |\n")
		for _, k := range s.keys {
			fmt.Fprintf(&b, "| <a id=\"%s\"></a>%s | %s | %s | %s |\n", k.path, code(k.path), k.typ, k.def, cell(k.doc))
		}
	}
	out := files{referenceDir + "/config.md": b.String()}

	profiles, err := profilesPage(cfgDocs, builtin, sections)
	if err != nil {
		return nil, err
	}
	out[referenceDir+"/network-profiles.md"] = profiles
	return out, nil
}

// profilesPage describes the built-in network profiles and the keys of a
// profile.
func profilesPage(docs *goDocs, builtin *config.Config, sections []section) (string, error) {
	var keys []configKey
	for _, s := range sections {
		if s.name == "network_profiles" {
			keys = s.keys
		}
	}
	if keys == nil {
		return "", fmt.Errorf("config.yaml has no network_profiles")
	}
	var b strings.Builder
	b.WriteString(generatedMark)
	b.WriteString("\n# Network profiles\n\n")
	for _, p := range paragraphs(strings.Replace(docs.types["NetworkProfile"], "NetworkProfile ", "A network profile ", 1)) {
		b.WriteString(escape(p) + "\n\n")
	}
	b.WriteString("A VM has one profile, [fence 2](glossary.md#fence-2). ")
	b.WriteString("`brig create --profile` and `brig update --profile` pick it, and [`defaults.network_profile`](config.md#defaults.network_profile) in `config.yaml` picks the one for VMs that do not say. ")
	b.WriteString("A change applies when the VM next starts.\n")

	b.WriteString("\n## Built-in profiles\n\n")
	names := builtin.ProfileNames()
	b.WriteString("| Profile | Allows |")
	for _, k := range keys {
		b.WriteString(" " + code(strings.TrimPrefix(k.path, "network_profiles.NAME.")) + " |")
	}
	b.WriteString("\n| --- | ---")
	b.WriteString(strings.Repeat(" | ---", len(keys)))
	b.WriteString(" |\n")
	for _, name := range names {
		p := builtin.NetworkProfiles[name]
		v := reflect.ValueOf(p)
		fmt.Fprintf(&b, "| %s | %s |", code(name), cell(p.Allows()))
		for i := range keys {
			b.WriteString(" " + defaultText(v.Field(i)) + " |")
		}
		b.WriteString("\n")
	}
	b.WriteString("\nA profile in `config.yaml` with the name of a built-in one replaces it as a whole.\n")

	b.WriteString("\n## Keys of a profile\n")
	for _, k := range keys {
		name := strings.TrimPrefix(k.path, "network_profiles.NAME.")
		fmt.Fprintf(&b, "\n### %s\n\nType: %s. Default: %s.\n\n%s\n", name, k.typ, k.def, inline(k.doc))
	}
	b.WriteString("\n## Your own profile\n\nAdd it to `network_profiles` in `config.yaml`. This one allows the internet and a model server on the host:\n\n")
	b.WriteString(fence("yaml", "network_profiles:", "  ollama:", "    internet: true", "    host_ports: [11434]"))
	return b.String(), nil
}
