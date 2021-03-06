// Copyright 2016 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tplimpl

import (
	"regexp"
	"strings"

	"github.com/gohugoio/hugo/identity"

	template "github.com/gohugoio/hugo/tpl/internal/go_templates/htmltemplate"
	texttemplate "github.com/gohugoio/hugo/tpl/internal/go_templates/texttemplate"
	"github.com/gohugoio/hugo/tpl/internal/go_templates/texttemplate/parse"

	"github.com/gohugoio/hugo/common/maps"
	"github.com/gohugoio/hugo/tpl"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

type templateType int

const (
	templateUndefined templateType = iota
	templateShortcode
	templatePartial
)

type templateContext struct {
	visited          map[string]bool
	templateNotFound map[string]bool
	identityNotFound map[string]bool
	lookupFn         func(name string) *templateInfoTree

	// The last error encountered.
	err error

	typ templateType

	// Set when we're done checking for config header.
	configChecked bool

	// Contains some info about the template
	parseInfo *tpl.ParseInfo
	id        identity.Manager

	// Store away the return node in partials.
	returnNode *parse.CommandNode
}

func (c templateContext) getIfNotVisited(name string) *templateInfoTree {
	if c.visited[name] {
		return nil
	}
	c.visited[name] = true
	templ := c.lookupFn(name)
	if templ == nil {
		// This may be a inline template defined outside of this file
		// and not yet parsed. Unusual, but it happens.
		// Store the name to try again later.
		c.templateNotFound[name] = true
	}

	return templ
}

func newTemplateContext(
	id identity.Manager,
	info *tpl.ParseInfo,
	lookupFn func(name string) *templateInfoTree) *templateContext {

	return &templateContext{
		id:               id,
		parseInfo:        info,
		lookupFn:         lookupFn,
		visited:          make(map[string]bool),
		templateNotFound: make(map[string]bool),
		identityNotFound: make(map[string]bool),
	}
}

func createGetTemplateInfoTreeFor(getID func(name string) *templateInfoTree) func(nn string) *templateInfoTree {
	return func(nn string) *templateInfoTree {
		return getID(nn)
	}
}

func (t *templateHandler) applyTemplateTransformersToHMLTTemplate(typ templateType, templ *template.Template) (*templateContext, error) {
	id, info := t.createTemplateInfo(templ.Name())
	ti := &templateInfoTree{
		tree:  templ.Tree,
		templ: templ,
		typ:   typ,
		id:    id,
		info:  info,
	}
	t.templateInfoTree[templ.Name()] = ti
	getTemplateInfoTree := createGetTemplateInfoTreeFor(func(name string) *templateInfoTree {
		return t.templateInfoTree[name]
	})

	return applyTemplateTransformers(typ, ti, getTemplateInfoTree)
}

func (t *templateHandler) applyTemplateTransformersToTextTemplate(typ templateType, templ *texttemplate.Template) (*templateContext, error) {
	id, info := t.createTemplateInfo(templ.Name())
	ti := &templateInfoTree{
		tree:  templ.Tree,
		templ: templ,
		typ:   typ,
		id:    id,
		info:  info,
	}

	t.templateInfoTree[templ.Name()] = ti
	getTemplateInfoTree := createGetTemplateInfoTreeFor(func(name string) *templateInfoTree {
		return t.templateInfoTree[name]
	})

	return applyTemplateTransformers(typ, ti, getTemplateInfoTree)

}

type templateInfoTree struct {
	info  tpl.ParseInfo
	typ   templateType
	id    identity.Manager
	templ tpl.Template
	tree  *parse.Tree
}

func applyTemplateTransformers(
	typ templateType,
	templ *templateInfoTree,
	lookupFn func(name string) *templateInfoTree) (*templateContext, error) {

	if templ == nil {
		return nil, errors.New("expected template, but none provided")
	}

	c := newTemplateContext(templ.id, &templ.info, lookupFn)
	c.typ = typ

	_, err := c.applyTransformations(templ.tree.Root)

	if err == nil && c.returnNode != nil {
		// This is a partial with a return statement.
		c.parseInfo.HasReturn = true
		templ.tree.Root = c.wrapInPartialReturnWrapper(templ.tree.Root)
	}

	return c, err
}

const (
	partialReturnWrapperTempl = `{{ $_hugo_dot := $ }}{{ $ := .Arg }}{{ with .Arg }}{{ $_hugo_dot.Set ("PLACEHOLDER") }}{{ end }}`
)

var (
	partialReturnWrapper *parse.ListNode
)

func init() {
	templ, err := texttemplate.New("").Parse(partialReturnWrapperTempl)
	if err != nil {
		panic(err)
	}
	partialReturnWrapper = templ.Tree.Root

}

func (c *templateContext) wrapInPartialReturnWrapper(n *parse.ListNode) *parse.ListNode {
	wrapper := partialReturnWrapper.CopyList()
	withNode := wrapper.Nodes[2].(*parse.WithNode)
	retn := withNode.List.Nodes[0]
	setCmd := retn.(*parse.ActionNode).Pipe.Cmds[0]
	setPipe := setCmd.Args[1].(*parse.PipeNode)
	// Replace PLACEHOLDER with the real return value.
	// Note that this is a PipeNode, so it will be wrapped in parens.
	setPipe.Cmds = []*parse.CommandNode{c.returnNode}
	withNode.List.Nodes = append(n.Nodes, retn)

	return wrapper

}

// The truth logic in Go's template package is broken for certain values
// for the if and with keywords. This works around that problem by wrapping
// the node passed to if/with in a getif conditional.
// getif works slightly different than the Go built-in in that it also
// considers any IsZero methods on the values (as in time.Time).
// See https://github.com/gohugoio/hugo/issues/5738
// TODO(bep) get rid of this.
func (c *templateContext) wrapWithGetIf(p *parse.PipeNode) {
	if len(p.Cmds) == 0 {
		return
	}

	// getif will return an empty string if not evaluated as truthful,
	// which is when we need the value in the with clause.
	firstArg := parse.NewIdentifier("getif")
	secondArg := p.CopyPipe()
	newCmd := p.Cmds[0].Copy().(*parse.CommandNode)

	// secondArg is a PipeNode and will behave as it was wrapped in parens, e.g:
	// {{ getif (len .Params | eq 2) }}
	newCmd.Args = []parse.Node{firstArg, secondArg}

	p.Cmds = []*parse.CommandNode{newCmd}

}

// applyTransformations do 3 things:
// 1) Wraps every with and if pipe in getif
// 2) Parses partial return statement.
// 3) Tracks template (partial) dependencies and some other info.
func (c *templateContext) applyTransformations(n parse.Node) (bool, error) {
	switch x := n.(type) {
	case *parse.ListNode:
		if x != nil {
			c.applyTransformationsToNodes(x.Nodes...)
		}
	case *parse.ActionNode:
		c.applyTransformationsToNodes(x.Pipe)
	case *parse.IfNode:
		c.applyTransformationsToNodes(x.Pipe, x.List, x.ElseList)
		c.wrapWithGetIf(x.Pipe)
	case *parse.WithNode:
		c.applyTransformationsToNodes(x.Pipe, x.List, x.ElseList)
		c.wrapWithGetIf(x.Pipe)
	case *parse.RangeNode:
		c.applyTransformationsToNodes(x.Pipe, x.List, x.ElseList)
	case *parse.TemplateNode:
		subTempl := c.getIfNotVisited(x.Name)
		if subTempl != nil {
			c.applyTransformationsToNodes(subTempl.tree.Root)
		}
	case *parse.PipeNode:
		c.collectConfig(x)
		for i, cmd := range x.Cmds {
			keep, _ := c.applyTransformations(cmd)
			if !keep {
				x.Cmds = append(x.Cmds[:i], x.Cmds[i+1:]...)
			}
		}

	case *parse.CommandNode:
		c.collectPartialInfo(x)
		c.collectInner(x)
		keep := c.collectReturnNode(x)

		for _, elem := range x.Args {
			switch an := elem.(type) {
			case *parse.PipeNode:
				c.applyTransformations(an)
			}
		}
		return keep, c.err
	}

	return true, c.err
}

func (c *templateContext) applyTransformationsToNodes(nodes ...parse.Node) {
	for _, node := range nodes {
		c.applyTransformations(node)
	}
}

func (c *templateContext) hasIdent(idents []string, ident string) bool {
	for _, id := range idents {
		if id == ident {
			return true
		}
	}
	return false
}

// collectConfig collects and parses any leading template config variable declaration.
// This will be the first PipeNode in the template, and will be a variable declaration
// on the form:
//    {{ $_hugo_config:= `{ "version": 1 }` }}
func (c *templateContext) collectConfig(n *parse.PipeNode) {
	if c.typ != templateShortcode {
		return
	}
	if c.configChecked {
		return
	}
	c.configChecked = true

	if len(n.Decl) != 1 || len(n.Cmds) != 1 {
		// This cannot be a config declaration
		return
	}

	v := n.Decl[0]

	if len(v.Ident) == 0 || v.Ident[0] != "$_hugo_config" {
		return
	}

	cmd := n.Cmds[0]

	if len(cmd.Args) == 0 {
		return
	}

	if s, ok := cmd.Args[0].(*parse.StringNode); ok {
		errMsg := "failed to decode $_hugo_config in template"
		m, err := maps.ToStringMapE(s.Text)
		if err != nil {
			c.err = errors.Wrap(err, errMsg)
			return
		}
		if err := mapstructure.WeakDecode(m, &c.parseInfo.Config); err != nil {
			c.err = errors.Wrap(err, errMsg)
		}
	}
}

// collectInner determines if the given CommandNode represents a
// shortcode call to its .Inner.
func (c *templateContext) collectInner(n *parse.CommandNode) {
	if c.typ != templateShortcode {
		return
	}
	if c.parseInfo.IsInner || len(n.Args) == 0 {
		return
	}

	for _, arg := range n.Args {
		var idents []string
		switch nt := arg.(type) {
		case *parse.FieldNode:
			idents = nt.Ident
		case *parse.VariableNode:
			idents = nt.Ident
		}

		if c.hasIdent(idents, "Inner") {
			c.parseInfo.IsInner = true
			break
		}
	}

}

var partialRe = regexp.MustCompile(`^partial(Cached)?$|^partials\.Include(Cached)?$`)

func (c *templateContext) collectPartialInfo(x *parse.CommandNode) {
	if len(x.Args) < 2 {
		return
	}

	first := x.Args[0]
	var id string
	switch v := first.(type) {
	case *parse.IdentifierNode:
		id = v.Ident
	case *parse.ChainNode:
		id = v.String()
	}

	if partialRe.MatchString(id) {
		partialName := strings.Trim(x.Args[1].String(), "\"")
		if !strings.Contains(partialName, ".") {
			partialName += ".html"
		}
		partialName = "partials/" + partialName
		info := c.lookupFn(partialName)
		if info != nil {
			c.id.Add(info.id)
		} else {
			// Delay for later
			c.identityNotFound[partialName] = true
		}
	}
}

func (c *templateContext) collectReturnNode(n *parse.CommandNode) bool {
	if c.typ != templatePartial || c.returnNode != nil {
		return true
	}

	if len(n.Args) < 2 {
		return true
	}

	ident, ok := n.Args[0].(*parse.IdentifierNode)
	if !ok || ident.Ident != "return" {
		return true
	}

	c.returnNode = n
	// Remove the "return" identifiers
	c.returnNode.Args = c.returnNode.Args[1:]

	return false

}
