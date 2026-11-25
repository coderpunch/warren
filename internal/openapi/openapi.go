// Package openapi gives the tool list that warren sends to Open WebUI.
//
// Open WebUI makes the address of each tool call from the path keys of the
// OpenAPI document of a terminal server, with no check
// (docs/protocol.md, section 4.3). A sandbox is hostile, thus warren never
// sends the document of a sandbox. Warren sends its own document. The
// document of a sandbox can only remove tools from that document.
//
// The file open-terminal.json is the OpenAPI document of Open Terminal
// 0.18.1 (MIT license, https://github.com/open-webui/open-terminal). It is
// the output of GET /openapi.json of that version with the default
// settings. The file LICENSE.open-terminal is the license notice of Open
// Terminal.
package openapi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

//go:embed open-terminal.json
var embedded []byte

// toolPath is the form of a path key that warren permits. A key that does
// not start with "/", or that has other characters, can change the host of
// a tool call.
var toolPath = regexp.MustCompile(`^(/[A-Za-z0-9._~{}-]+)+/?$`)

// SafePath reports whether a path key is safe in a document for Open WebUI.
func SafePath(key string) bool {
	if !toolPath.MatchString(key) {
		return false
	}
	// A tool call must not go to the management API.
	if key == "/api/v1" || strings.HasPrefix(key, "/api/v1/") {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// methods are the keys of a path item that are operations.
var methods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// Document is a tool list that an administrator trusts.
type Document struct {
	doc   map[string]json.RawMessage
	paths map[string]map[string]json.RawMessage
	full  []byte
}

// Default returns the document that is part of warren.
func Default() *Document {
	d, err := Parse(embedded)
	if err != nil {
		panic("the embedded OpenAPI document is not valid: " + err.Error())
	}
	return d
}

// Parse reads a trusted document. It removes the parts that can send a tool
// call to a different address, also when an administrator supplied the file.
func Parse(data []byte) (*Document, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, errors.New("the document is not a JSON object")
	}
	delete(doc, "servers")
	var paths map[string]map[string]json.RawMessage
	if raw, ok := doc["paths"]; ok {
		if err := json.Unmarshal(raw, &paths); err != nil {
			return nil, fmt.Errorf("paths: %w", err)
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("the document has no paths")
	}
	for key, item := range paths {
		if !SafePath(key) {
			return nil, fmt.Errorf("the path key %q is not permitted", key)
		}
		delete(item, "servers")
		delete(item, "$ref")
		for name, rawOp := range item {
			if !methods[name] {
				continue
			}
			var op map[string]json.RawMessage
			if err := json.Unmarshal(rawOp, &op); err != nil || op == nil {
				return nil, fmt.Errorf("%s %s is not an operation object", name, key)
			}
			delete(op, "servers")
			item[name], _ = json.Marshal(op)
		}
	}
	d := &Document{doc: doc, paths: paths}
	d.full = d.render(nil)
	return d, nil
}

// Full returns the complete document.
func (d *Document) Full() []byte { return d.full }

// Operations is a set of operations: path key, then method in lower case.
type Operations map[string]map[string]bool

// Filtered returns the document with only the operations that are in the
// set. Use it to remove the tools that a sandbox does not have.
func (d *Document) Filtered(have Operations) []byte {
	return d.render(have)
}

func (d *Document) render(have Operations) []byte {
	paths := make(map[string]map[string]json.RawMessage, len(d.paths))
	for key, item := range d.paths {
		out := make(map[string]json.RawMessage, len(item))
		operations := 0
		for name, raw := range item {
			if methods[name] {
				if have != nil && !have[key][name] {
					continue
				}
				operations++
			}
			out[name] = raw
		}
		if operations > 0 {
			paths[key] = out
		}
	}
	doc := make(map[string]json.RawMessage, len(d.doc))
	for k, v := range d.doc {
		doc[k] = v
	}
	doc["paths"], _ = json.Marshal(paths)
	out, _ := json.Marshal(doc)
	return out
}

// Has reports whether the document has one or more operations of the set.
// A sandbox must not remove all tools: Open WebUI stops a chat turn when
// the tool list is empty.
func (d *Document) Has(have Operations) bool {
	for key, item := range d.paths {
		for name := range item {
			if methods[name] && have[key][name] {
				return true
			}
		}
	}
	return false
}

// OperationsOf reads the operations from the document of a sandbox. Warren
// uses only the path keys and the method names of that document, and only to
// compare them with the trusted document. It returns false if the data is
// not an OpenAPI document.
func OperationsOf(data []byte) (Operations, bool) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || doc.Paths == nil {
		return nil, false
	}
	ops := make(Operations, len(doc.Paths))
	for key, item := range doc.Paths {
		for name := range item {
			if methods[name] {
				if ops[key] == nil {
					ops[key] = map[string]bool{}
				}
				ops[key][name] = true
			}
		}
	}
	return ops, true
}
