// Copyright 2020-2023 Buf Technologies, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package format

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/kralicky/protocompile/ast"
)

type FileNodeInterface interface {
	GetSyntax() *ast.SyntaxNode
	GetEdition() *ast.EditionNode
	GetDecls() []*ast.FileElement
	GetEOF() *ast.RuneNode

	NodeInfo(node ast.Node) ast.NodeInfo
}

// formatter writes an *ast.FileNode as a .proto file.
type formatter struct {
	writer   io.Writer
	fileNode FileNodeInterface

	// Current level of indentation.
	indent int
	// The last character written to writer.
	lastWritten rune

	// The last node written. This must be updated from all functions
	// that write comments with a node. This flag informs how the next
	// node's leading comments and whitespace should be written.
	previousNode ast.Node

	// If true, a space will be written to the output unless the next character
	// written is a newline (don't wait errant trailing spaces).
	pendingSpace bool
	// If true, the formatter is in the middle of printing compact options.
	inCompactOptions bool

	// Track runes that open blocks/scopes and are expected to increase indention
	// level. For example, when runes "{" "[" "(" ")" are written, the pending
	// value is 2 (increment three times for "{" "[" "("; decrement once for ")").
	// If it's greater than zero at the end of a line, we call In() so that
	// subsequent lines are indented. If it's less than zero at the end of a line,
	// we call Out(). This minimizes the amount of explicit indent/unindent code
	// that is needed and makes it less error-prone.
	pendingIndent int
	// If true, an inline node/sequence is being written. We treat whitespace a
	// little differently for when blocks are printed inline vs. across multiple
	// lines. So this flag informs the logic that makes those whitespace decisions.
	inline bool

	// Records all errors that occur during the formatting process. Nearly any
	// non-nil error represents a bug in the implementation.
	err error
}

func (f *formatter) saveState(newWriter io.Writer) *formatter {
	return &formatter{
		writer:           newWriter,
		fileNode:         f.fileNode,
		indent:           f.indent,
		lastWritten:      f.lastWritten,
		previousNode:     f.previousNode,
		pendingSpace:     f.pendingSpace,
		inCompactOptions: f.inCompactOptions,
		pendingIndent:    f.pendingIndent,
		inline:           f.inline,
	}
}

func (f *formatter) mergeState(other *formatter, reader io.Reader) {
	io.Copy(f.writer, reader)
	f.indent = other.indent
	f.lastWritten = other.lastWritten
	f.previousNode = other.previousNode
	f.pendingSpace = other.pendingSpace
	f.inCompactOptions = other.inCompactOptions
	f.pendingIndent = other.pendingIndent
	f.inline = other.inline
}

// NewFormatter returns a new formatter for the given file.
func NewFormatter(
	writer io.Writer,
	fileNode FileNodeInterface,
) *formatter {
	return &formatter{
		writer:   writer,
		fileNode: fileNode,
	}
}

// Run runs the formatter and writes the file's content to the formatter's writer.
func (f *formatter) Run() error {
	f.writeFile()
	return f.err
}

// P prints a line to the generated output.
//
// This will emit a newline and proper indentation. If you do not
// want to emit a newline and want to write a raw string, use
// WriteString (which P calls).
//
// If strings.TrimSpace(elem) is empty, no indentation is produced.
func (f *formatter) P(elem string) {
	if len(strings.TrimSpace(elem)) > 0 {
		// We only want to write an indent if we're
		// writing a non-empty string (not just a newline).
		f.Indent(nil)
		f.WriteString(elem)
	}
	f.WriteString("\n")

	if f.pendingIndent > 0 {
		f.In()
	} else if f.pendingIndent < 0 {
		f.Out()
	}
	f.pendingIndent = 0
}

// Space adds a space to the generated output.
func (f *formatter) Space() {
	f.pendingSpace = true
}

// In increases the current level of indentation.
func (f *formatter) In() {
	f.indent++
}

// Out reduces the current level of indentation.
func (f *formatter) Out() {
	if f.indent <= 0 {
		// Unreachable.
		f.err = errors.Join(f.err,
			errors.New("internal error: attempted to decrement indentation at zero"),
		)
		return
	}
	f.indent--
}

// Indent writes the number of spaces associated
// with the current level of indentation.
func (f *formatter) Indent(nextNode ast.Node) {
	nextNode = ast.Unwrap(nextNode)

	// only indent at beginning of line
	if f.lastWritten != '\n' {
		return
	}
	indent := f.indent
	if rn, ok := nextNode.(*ast.RuneNode); ok && indent > 0 {
		if strings.ContainsRune("}])>", rn.Rune) {
			indent--
		}
	}
	f.WriteString(strings.Repeat("  ", indent))
}

// WriteString writes the given element to the generated output.
//
// This will not write indentation or newlines. Use P if you
// want to emit identation or newlines.
func (f *formatter) WriteString(elem string) {
	if f.pendingSpace {
		f.pendingSpace = false
		first, _ := utf8.DecodeRuneInString(elem)

		// We don't want "dangling spaces" before certain characters:
		// newlines, commas, and semicolons. Also, when writing
		// elements inline, we don't want spaces before close parens
		// and braces. Similarly, we don't want extra/doubled spaces
		// or dangling spaces after certain characters when printing
		// inline, like open parens/braces. So only print the space
		// if the previous and next character don't match above
		// conditions.

		prevBlockList := "\x00 \t\n"
		nextBlockList := "\n;,"
		if f.inline {
			prevBlockList = "\x00 \t\n<[{("
			nextBlockList = "\n;,)]}>"
		}

		if !strings.ContainsRune(prevBlockList, f.lastWritten) &&
			!strings.ContainsRune(nextBlockList, first) {
			if _, err := f.writer.Write([]byte{' '}); err != nil {
				f.err = errors.Join(f.err, err)
				return
			}
		}
	}
	if len(elem) == 0 {
		return
	}
	f.lastWritten, _ = utf8.DecodeLastRuneInString(elem)
	if _, err := f.writer.Write([]byte(elem)); err != nil {
		f.err = errors.Join(f.err, err)
	}
}

// SetPreviousNode sets the previously written node. This should
// be called in all of the comment writing functions.
func (f *formatter) SetPreviousNode(node ast.Node) {
	f.previousNode = node
}

// writeFile writes the file node.
func (f *formatter) writeFile() {
	f.writeFileHeader()
	f.writeFileTypes()
	if f.fileNode.GetEOF() != nil {
		info := f.fileNode.NodeInfo(f.fileNode.GetEOF())
		f.writeMultilineComments(info.LeadingComments())
	}
	if f.lastWritten != 0 && f.lastWritten != '\n' {
		// If anything was written, we always conclude with
		// a newline.
		f.P("")
	}
}

// writeFileHeader writes the header of a .proto file. This includes the syntax,
// package, imports, and options (in that order). The imports and options are
// sorted. All other file elements are handled by f.writeFileTypes.
//
// For example,
//
//	syntax = "proto3";
//
//	package acme.v1.weather;
//
//	import "acme/payment/v1/payment.proto";
//	import "google/type/datetime.proto";
//
//	option cc_enable_arenas = true;
//	option optimize_for = SPEED;
func (f *formatter) writeFileHeader() {
	var (
		packageNode *ast.PackageNode
		importNodes []*ast.ImportNode
		optionNodes []*ast.OptionNode
	)
	for _, fileElement := range f.fileNode.GetDecls() {
		switch node := fileElement.Unwrap().(type) {
		case *ast.PackageNode:
			packageNode = node
		case *ast.ImportNode:
			importNodes = append(importNodes, node)
		case *ast.OptionNode:
			optionNodes = append(optionNodes, node)
		default:
			continue
		}
	}
	if f.fileNode.GetSyntax() == nil && f.fileNode.GetEdition() == nil && packageNode == nil && importNodes == nil && optionNodes == nil {
		// There aren't any header values, so we can return early.
		return
	}
	if syntaxNode := f.fileNode.GetSyntax(); syntaxNode != nil {
		f.writeSyntax(syntaxNode)
	} else if editionNode := f.fileNode.GetEdition(); editionNode != nil {
		f.writeEdition(editionNode)
	}
	if packageNode != nil {
		f.writePackage(packageNode)
	}
	sort.Slice(importNodes, func(i, j int) bool {
		iName := importNodes[i].Name.AsString()
		jName := importNodes[j].Name.AsString()
		// sort by public > None > weak
		iOrder := importSortOrder(importNodes[i])
		jOrder := importSortOrder(importNodes[j])

		if iName < jName {
			return true
		}
		if iName > jName {
			return false
		}
		if iOrder > jOrder {
			return true
		}
		if iOrder < jOrder {
			return false
		}

		// put commented import first
		return !f.importHasComment(importNodes[j])
	})
	for i, importNode := range importNodes {
		if i == 0 && f.previousNode != nil && !f.leadingCommentsContainBlankLine(importNode) {
			f.P("")
		}

		// since the imports are sorted, this will skip write imports
		// if they have appear before and dont have comment
		if i > 0 && importNode.Name.AsString() == importNodes[i-1].Name.AsString() &&
			!f.importHasComment(importNode) {
			continue
		}

		f.writeImport(importNode, i > 0)
	}
	sort.Slice(optionNodes, func(i, j int) bool {
		// The default options (e.g. cc_enable_arenas) should always
		// be sorted above custom options (which are identified by a
		// leading '(').
		left := StringForOptionName(optionNodes[i].Name)
		right := StringForOptionName(optionNodes[j].Name)
		if strings.HasPrefix(left, "(") && !strings.HasPrefix(right, "(") {
			// Prefer the default option on the right.
			return false
		}
		if !strings.HasPrefix(left, "(") && strings.HasPrefix(right, "(") {
			// Prefer the default option on the left.
			return true
		}
		// Both options are custom, so we defer to the standard sorting.
		return left < right
	})

	if len(optionNodes) > 0 && f.previousNode != nil && !f.leadingCommentsContainBlankLine(optionNodes[0]) {
		f.P("")
	}
	f.writeFileOptions(optionNodes)
}

// writeFileTypes writes the types defined in a .proto file. This includes the messages, enums,
// services, etc. All other elements are ignored since they are handled by f.writeFileHeader.
func (f *formatter) writeFileTypes() {
	for _, fileElement := range f.fileNode.GetDecls() {
		switch node := fileElement.Unwrap().(type) {
		case *ast.PackageNode, *ast.OptionNode, *ast.ImportNode, *ast.EmptyDeclNode:
			// These elements have already been written by f.writeFileHeader.
			continue
		default:
			if f.previousNode != nil && !f.leadingCommentsContainBlankLine(node) {
				f.P("")
			}
			f.writeNode(node)
		}
	}
}

// writeSyntax writes the syntax.
//
// For example,
//
//	syntax = "proto3";
func (f *formatter) writeSyntax(syntaxNode *ast.SyntaxNode) {
	f.writeStart(syntaxNode.Keyword)
	f.Space()
	f.writeInline(syntaxNode.Equals)
	f.Space()
	f.writeInline(syntaxNode.Syntax)
	f.writeLineEnd(syntaxNode.Semicolon)
}

// writeEdition writes the edition.
//
// For example,
//
//	edition = "2023";
func (f *formatter) writeEdition(editionNode *ast.EditionNode) {
	f.writeStart(editionNode.Keyword)
	f.Space()
	f.writeInline(editionNode.Equals)
	f.Space()
	f.writeInline(editionNode.Edition)
	f.writeLineEnd(editionNode.Semicolon)
}

// writePackage writes the package.
//
// For example,
//
//	package acme.weather.v1;
func (f *formatter) writePackage(packageNode *ast.PackageNode) {
	f.writeStart(packageNode.Keyword)
	f.Space()
	if packageNode.Name != nil {
		f.writeInline(packageNode.Name)
	}
	f.writeLineEnd(packageNode.Semicolon)
}

// writeImport writes an import statement.
//
// For example,
//
//	import "google/protobuf/descriptor.proto";
func (f *formatter) writeImport(importNode *ast.ImportNode, forceCompact bool) {
	f.writeStartMaybeCompact(importNode.Keyword, forceCompact)
	f.Space()
	// We don't want to write the "public" and "weak" nodes
	// if they aren't defined. One could be set, but never both.
	switch {
	case importNode.Public != nil:
		f.writeInline(importNode.Public)
		f.Space()
	case importNode.Weak != nil:
		f.writeInline(importNode.Weak)
		f.Space()
	}
	f.writeInline(importNode.Name)
	f.writeLineEnd(importNode.Semicolon)
}

type fileOptionNodesGroup []*ast.OptionNode

func (g fileOptionNodesGroup) GetElements() []*ast.OptionNode {
	return g
}

func (f *formatter) writeFileOptions(optionNodes []*ast.OptionNode) {
	columnFormatElements(f, fileOptionNodesGroup(optionNodes))
}

// writeFileOption writes a file option. This function is slightly
// different than f.writeOption because file options are sorted at
// the top of the file, and leading comments are adjusted accordingly.
func (f *formatter) writeFileOption(optionNode *ast.OptionNode, forceCompact bool) {
	f.writeStartMaybeCompact(optionNode.Keyword, forceCompact)
	f.Space()
	f.writeNode(optionNode.Name)
	f.Space()
	f.writeInline(optionNode.Equals)
	if node := optionNode.Val.GetCompoundStringLiteral(); node != nil {
		// Compound string literals are written across multiple lines
		// immediately after the '=', so we don't need a trailing
		// space in the option prefix.
		f.writeCompoundStringLiteralIndentEndInline(node)
		f.writeLineEnd(optionNode.Semicolon)
		return
	}
	f.Space()
	f.writeInline(optionNode.Val)
	f.writeLineEnd(optionNode.Semicolon)
}

// writeOption writes an option.
//
// For example,
//
//	option go_package = "github.com/foo/bar";
func (f *formatter) writeOption(optionNode *ast.OptionNode) {
	f.writeOptionPrefix(optionNode)
	if optionNode.Semicolon != nil {
		if node := optionNode.Val.GetCompoundStringLiteral(); node != nil {
			// Compound string literals are written across multiple lines
			// immediately after the '=', so we don't need a trailing
			// space in the option prefix.
			f.writeCompoundStringLiteralIndentEndInline(node)
			f.writeLineEnd(optionNode.Semicolon)
			return
		}
		f.writeInline(optionNode.Val)
		f.writeLineEnd(optionNode.Semicolon)
		return
	}

	if node := optionNode.Val.GetCompoundStringLiteral(); node != nil {
		f.writeCompoundStringLiteralIndent(node)
		return
	}
	f.writeInline(optionNode.Val)
}

// writeLastCompactOption writes a compact option but preserves its the
// trailing end comments. This is only used for the last compact option
// since it's the only time a trailing ',' will be omitted.
//
// For example,
//
//	[
//	  deprecated = true,
//	  json_name = "something" // Trailing comment on the last element.
//	]
func (f *formatter) writeLastCompactOption(optionNode *ast.OptionNode) {
	f.writeOptionPrefix(optionNode)
	f.writeLineEnd(optionNode.Val)
}

// writeOptionValue writes the option prefix, which makes up all of the
// option's definition, excluding the final token(s).
//
// For example,
//
//	deprecated =
func (f *formatter) writeOptionPrefix(optionNode *ast.OptionNode) {
	if optionNode.Keyword != nil {
		// Compact options don't have the keyword.
		f.writeStart(optionNode.Keyword)
		f.Space()
		f.writeNode(optionNode.Name)
	} else {
		f.writeStart(optionNode.Name)
	}
	f.Space()
	f.writeInline(optionNode.Equals)
	f.Space()
}

// writeOptionName writes an option name.
//
// For example,
//
//	go_package
//	(custom.thing)
//	(custom.thing).bridge.(another.thing)
func (f *formatter) writeOptionName(optionNameNode *ast.OptionNameNode) {
	if optionNameNode == nil {
		return
	}
	for i, part := range optionNameNode.Parts {
		if f.inCompactOptions && i == 0 {
			// The leading comments of the first token (either open rune or the
			// name) will have already been written, so we need to handle this
			// case specially.
			fieldReferenceNode := part.GetFieldRef()
			if fieldReferenceNode != nil {
				if fieldReferenceNode.Open != nil {
					f.writeNode(fieldReferenceNode.Open)
					if info := f.fileNode.NodeInfo(fieldReferenceNode.Open); info.TrailingComments().Len() > 0 {
						f.writeInlineComments(info.TrailingComments())
					}
					f.writeInline(fieldReferenceNode.Name)
				} else {
					f.writeNode(fieldReferenceNode.Name)
					if info := f.fileNode.NodeInfo(fieldReferenceNode.Name); info.TrailingComments().Len() > 0 {
						f.writeInlineComments(info.TrailingComments())
					}
				}
				if fieldReferenceNode.Close != nil {
					f.writeInline(fieldReferenceNode.Close)
				} else if fieldReferenceNode.Open != nil {
					// (extended syntax rule) fill in missing close paren automatically
					f.writeInline(&ast.RuneNode{Rune: ')'})
				}
				continue
			}
		}
		f.writeNode(part)
	}
}

// writeMessage writes the message node.
//
// For example,
//
//	message Foo {
//	  option deprecated = true;
//	  reserved 50 to 100;
//	  extensions 150 to 200;
//
//	  message Bar {
//	    string name = 1;
//	  }
//	  enum Baz {
//	    BAZ_UNSPECIFIED = 0;
//	  }
//	  extend Bar {
//	    string value = 2;
//	  }
//
//	  Bar bar = 1;
//	  Baz baz = 2;
//	}
func (f *formatter) writeMessage(messageNode *ast.MessageNode) {
	var elementWriterFunc func()
	if len(messageNode.Decls) != 0 {
		elementWriterFunc = func() {
			columnFormatElements(f, messageNode)
		}
	}
	f.writeStart(messageNode.Keyword)
	f.Space()
	f.writeInline(messageNode.Name)
	f.Space()
	f.writeCompositeTypeBody(
		messageNode.OpenBrace,
		messageNode.CloseBrace,
		messageNode.Semicolon,
		elementWriterFunc,
	)
}

// writeMessageLiteral writes a message literal.
//
// For example,
//
//	{
//	  foo: 1
//	  foo: 2
//	  foo: 3
//	  bar: <
//	    name:"abc"
//	    id:123
//	  >
//	}
func (f *formatter) writeMessageLiteral(messageLiteralNode *ast.MessageLiteralNode) {
	if f.maybeWriteCompactMessageLiteral(messageLiteralNode, false) {
		return
	}
	var elementWriterFunc func()
	if len(messageLiteralNode.Elements) > 0 {
		elementWriterFunc = func() {
			f.writeMessageLiteralElements(messageLiteralNode)
		}
	}
	f.writeCompositeValueBody(
		messageLiteralNode.Open,
		messageLiteralNode.Close,
		messageLiteralNode.Semicolon,
		elementWriterFunc,
	)
}

// writeMessageLiteral writes a message literal suitable for
// an element in an array literal.
func (f *formatter) writeMessageLiteralForArray(
	messageLiteralNode *ast.MessageLiteralNode,
	lastElement bool,
) {
	if f.maybeWriteCompactMessageLiteral(messageLiteralNode, true) {
		if lastElement {
			f.P("")
		}
		return
	}
	var elementWriterFunc func()
	if len(messageLiteralNode.Elements) > 0 {
		elementWriterFunc = func() {
			f.writeMessageLiteralElements(messageLiteralNode)
		}
	}
	closeWriter := f.writeBodyEndInline
	if lastElement {
		closeWriter = f.writeBodyEnd
	}
	f.writeBody(
		messageLiteralNode.Open,
		messageLiteralNode.Close,
		messageLiteralNode.Semicolon,
		elementWriterFunc,
		f.writeOpenBracePrefixForArray,
		closeWriter,
	)
}

func (f *formatter) messageLiteralShouldBeExpanded(messageLiteralNode *ast.MessageLiteralNode) bool {
	if len(messageLiteralNode.Elements) == 0 {
		return false
	}

	// if len(messageLiteralNode.Elements) == 0 || len(messageLiteralNode.Elements) > 1 ||
	// 	f.hasInteriorComments(messageLiteralNode.GetChildren()...) ||
	// 	messageLiteralHasNestedMessageOrArray(messageLiteralNode) {
	// 	return false
	// }
	// if the node is not currently formatted on a single line, then
	// preserve the existing formatting
	info := f.fileNode.NodeInfo(messageLiteralNode.Elements[0])
	whitespace := info.LeadingWhitespace()
	if strings.Contains(whitespace, "\n") {
		return true
	}
	return false
}

func (f *formatter) arrayLiteralShouldBeExpanded(arrayLiteralNode *ast.ArrayLiteralNode) bool {
	if len(arrayLiteralNode.Elements) == 0 {
		return false
	}

	info := f.fileNode.NodeInfo(arrayLiteralNode.Elements[0])
	whitespace := info.LeadingWhitespace()
	if strings.Contains(whitespace, "\n") {
		return true
	}
	return false
}

func (f *formatter) maybeWriteCompactMessageLiteral(
	messageLiteralNode *ast.MessageLiteralNode,
	inArrayLiteral bool,
) bool {
	if f.messageLiteralShouldBeExpanded(messageLiteralNode) {
		return false
	}

	if inArrayLiteral {
		f.Indent(messageLiteralNode.Open)
	}
	f.writeInline(messageLiteralNode.Open)
	for i, fieldNode := range messageLiteralNode.Elements {
		f.writeInline(fieldNode.Name)
		if fieldNode.Sep != nil {
			f.writeInline(fieldNode.Sep)
		} else {
			// fill in missing ':' automatically
			f.writeInline(&ast.RuneNode{Rune: ':'})
		}
		f.Space()
		f.writeInline(fieldNode.Val)
		if fieldNode.Semicolon == nil && i < len(messageLiteralNode.Elements)-1 {
			fieldNode.Semicolon = &ast.RuneNode{Rune: ','}
		}
		if fieldNode.Semicolon != nil && i < len(messageLiteralNode.Elements)-1 {
			f.writeInline(fieldNode.Semicolon)
			f.Space()
		}
	}
	f.writeInline(messageLiteralNode.Close)
	return true
}

func messageLiteralHasNestedMessageOrArray(messageLiteralNode *ast.MessageLiteralNode) bool {
	for _, elem := range messageLiteralNode.Elements {
		switch elem.Val.Unwrap().(type) {
		case *ast.ArrayLiteralNode, *ast.MessageLiteralNode:
			return true
		}
	}
	return false
}

func messageLiteralHasNestedArray(messageLiteralNode *ast.MessageLiteralNode) bool {
	for _, elem := range messageLiteralNode.Elements {
		if v := elem.Val.GetArrayLiteral(); v != nil {
			if len(v.Elements) == 0 {
				// (extended syntax rule) empty array literals don't need to be expanded
				continue
			}
			return true
		}
	}
	return false
}

func arrayLiteralHasNestedMessageOrArray(arrayLiteralNode *ast.ArrayLiteralNode) bool {
	for _, elem := range arrayLiteralNode.FilterValues() {
		switch elem.Unwrap().(type) {
		case *ast.ArrayLiteralNode, *ast.MessageLiteralNode:
			return true
		}
	}
	return false
}

// writeMessageLiteralElements writes the message literal's elements.
//
// For example,
//
//	foo: 1
//	foo: 2
func (f *formatter) writeMessageLiteralElements(messageLiteralNode *ast.MessageLiteralNode) {
	canColumnFormat := true
	// for _, elem := range messageLiteralNode.Seps {
	// 	if elem != nil {
	// 		canColumnFormat = false
	// 		break
	// 	}
	// }
	// if canColumnFormat {
	// 	LOOP:
	// 	for _, elem := range messageLiteralNode.Elements {
	// 		switch elem.Val.(type) {
	// 		case *ast.CompoundStringLiteralNode, *ast.MessageLiteralNode:
	// 			canColumnFormat = false
	// 			break LOOP
	// 		}
	// 	}
	// }

	// make open/close brackets consistent within the message literal

	if canColumnFormat {
		columnFormatElements(f, messageLiteralNode)
		return
	}
	for _, elem := range messageLiteralNode.Elements {
		if sep := elem.Semicolon; sep != nil {
			f.writeMessageFieldWithSeparator(elem)
			f.writeLineEnd(sep)
			continue
		}
		f.writeNode(elem)
	}
}

// writeMessageField writes the message field node, and concludes the
// line without leaving room for a trailing separator in the parent
// message literal.
func (f *formatter) writeMessageField(messageFieldNode *ast.MessageFieldNode) {
	f.writeMessageFieldPrefix(messageFieldNode)
	f.Space()
	if compoundStringLiteral := messageFieldNode.Val.GetCompoundStringLiteral(); compoundStringLiteral != nil {
		f.writeCompoundStringLiteralIndent(compoundStringLiteral)
		return
	}
	f.writeLineEnd(messageFieldNode.Val)
}

// writeMessageFieldWithSeparator writes the message field node,
// but leaves room for a trailing separator in the parent message
// literal.
func (f *formatter) writeMessageFieldWithSeparator(messageFieldNode *ast.MessageFieldNode) {
	f.writeMessageFieldPrefix(messageFieldNode)
	f.Space()
	if compoundStringLiteral := messageFieldNode.Val.GetCompoundStringLiteral(); compoundStringLiteral != nil {
		f.writeCompoundStringLiteralIndentEndInline(compoundStringLiteral)
		return
	}
	f.writeInline(messageFieldNode.Val)
}

// writeMessageFieldPrefix writes the message field node as a single line.
//
// For example,
//
//	foo:"bar"
func (f *formatter) writeMessageFieldPrefix(messageFieldNode *ast.MessageFieldNode, maybeNodeWriter ...func(ast.Node)) {
	// The comments need to be written as a multiline comment above
	// the message field name.
	//
	// Note that this is different than how field reference nodes are
	// normally formatted in-line (i.e. as option name components).
	fieldReferenceNode := messageFieldNode.Name
	if fieldReferenceNode.Open != nil {
		f.writeStart(fieldReferenceNode.Open, maybeNodeWriter...)
		if fieldReferenceNode.UrlPrefix != nil {
			f.writeInline(fieldReferenceNode.UrlPrefix)
		}
		if fieldReferenceNode.Slash != nil {
			f.writeInline(fieldReferenceNode.Slash)
		}
		f.writeInline(fieldReferenceNode.Name)
	} else {
		f.writeStart(fieldReferenceNode.Name, maybeNodeWriter...)
	}
	if fieldReferenceNode.Close != nil {
		f.writeInline(fieldReferenceNode.Close)
	} else if fieldReferenceNode.Open != nil {
		// (extended syntax rule) fill in missing close paren automatically
		f.writeInline(&ast.RuneNode{Rune: ')'})
	}
	if messageFieldNode.Sep != nil {
		f.writeInline(messageFieldNode.Sep)
	}
}

// writeEnum writes the enum node.
//
// For example,
//
//	enum Foo {
//	  option deprecated = true;
//	  reserved 1 to 5;
//
//	  FOO_UNSPECIFIED = 0;
//	}
func (f *formatter) writeEnum(enumNode *ast.EnumNode) {
	var elementWriterFunc func()
	if len(enumNode.Decls) > 0 {
		elementWriterFunc = func() {
			columnFormatElements(f, enumNode)
		}
	}
	f.writeStart(enumNode.Keyword)
	f.Space()
	f.writeInline(enumNode.Name)
	f.Space()
	f.writeCompositeTypeBody(
		enumNode.OpenBrace,
		enumNode.CloseBrace,
		enumNode.Semicolon,
		elementWriterFunc,
	)
}

// writeEnumValue writes the enum value as a single line. If the enum has
// compact options, it will be written across multiple lines.
//
// For example,
//
//	FOO_UNSPECIFIED = 1 [
//	  deprecated = true
//	];
func (f *formatter) writeEnumValue(enumValueNode *ast.EnumValueNode) {
	f.writeStart(enumValueNode.Name)
	f.Space()
	f.writeInline(enumValueNode.Equals)
	f.Space()
	f.writeInline(enumValueNode.Number)
	if enumValueNode.Options != nil {
		f.Space()
		f.writeNode(enumValueNode.Options)
	}
	if enumValueNode.Semicolon != nil && enumValueNode.Semicolon.Rune != ';' {
		enumValueNode.Semicolon.Rune = ';'
	}
	f.writeLineEnd(enumValueNode.Semicolon)
}

// writeField writes the field node as a single line. If the field has
// compact options, it will be written across multiple lines.
//
// For example,
//
//	repeated string name = 1 [
//	  deprecated = true,
//	  json_name = "name"
//	];
func (f *formatter) writeField(fieldNode *ast.FieldNode) {
	// We need to handle the comments for the field label specially since
	// a label might not be defined, but it has the leading comments attached
	// to it.
	if fieldNode.Label != nil {
		f.writeStart(fieldNode.Label)
		f.Space()
		f.writeInline(fieldNode.FieldType)
	} else {
		// If a label was not written, the multiline comments will be
		// attached to the type.
		if compoundIdentNode := fieldNode.GetFieldType().GetCompoundIdent(); compoundIdentNode != nil {
			f.writeCompountIdentForFieldName(compoundIdentNode)
		} else {
			f.writeStart(fieldNode.FieldType)
		}
	}
	if fieldNode.Name != nil {
		f.Space()
		f.writeInline(fieldNode.Name)
	}
	if fieldNode.Equals != nil {
		f.Space()
		f.writeInline(fieldNode.Equals)
	}
	if fieldNode.Tag != nil {
		f.Space()
		f.writeInline(fieldNode.Tag)
	}
	if fieldNode.Options != nil {
		f.Space()
		f.writeNode(fieldNode.Options)
	}
	f.writeLineEnd(fieldNode.Semicolon)
}

// writeMapField writes a map field (e.g. 'map<string, string> pairs = 1;').
func (f *formatter) writeMapField(mapFieldNode *ast.MapFieldNode) {
	f.writeNode(mapFieldNode.MapType)
	f.Space()
	f.writeInline(mapFieldNode.Name)
	f.Space()
	f.writeInline(mapFieldNode.Equals)
	f.Space()
	f.writeInline(mapFieldNode.Tag)
	if mapFieldNode.Options != nil {
		f.Space()
		f.writeNode(mapFieldNode.Options)
	}
	f.writeLineEnd(mapFieldNode.Semicolon)
}

// writeMapType writes a map type (e.g. 'map<string, string>').
func (f *formatter) writeMapType(mapTypeNode *ast.MapTypeNode) {
	f.writeStart(mapTypeNode.Keyword)
	f.writeInline(mapTypeNode.OpenAngle)
	f.writeInline(mapTypeNode.KeyType)
	f.writeInline(mapTypeNode.Comma)
	f.Space()
	f.writeInline(mapTypeNode.ValueType)
	f.writeInline(mapTypeNode.CloseAngle)
	if vs := mapTypeNode.Semicolon; vs != nil {
		info := f.fileNode.NodeInfo(vs)
		if info.TrailingComments().Len() > 0 {
			f.writeInlineComments(info.TrailingComments())
		}
	}
}

// writeFieldReference writes a field reference (e.g. '(foo.bar)').
func (f *formatter) writeFieldReference(fieldReferenceNode *ast.FieldReferenceNode) {
	if fieldReferenceNode.Open != nil {
		f.writeInline(fieldReferenceNode.Open)
	}
	if fieldReferenceNode.UrlPrefix != nil {
		f.writeInline(fieldReferenceNode.UrlPrefix)
	}
	if fieldReferenceNode.Slash != nil {
		f.writeInline(fieldReferenceNode.Slash)
	}
	f.writeInline(fieldReferenceNode.Name)
	if fieldReferenceNode.Close != nil {
		f.writeInline(fieldReferenceNode.Close)
	} else if fieldReferenceNode.Open != nil {
		// (extended syntax rule) fill in missing close paren automatically
		f.writeInline(&ast.RuneNode{Rune: ')'})
	}
}

// writeExtend writes the extend node.
//
// For example,
//
//	extend google.protobuf.FieldOptions {
//	  bool redacted = 33333;
//	}
func (f *formatter) writeExtend(extendNode *ast.ExtendNode) {
	var elementWriterFunc func()
	if len(extendNode.Decls) > 0 {
		elementWriterFunc = func() {
			columnFormatElements(f, extendNode)
		}
	}
	f.writeStart(extendNode.Keyword)
	if extendNode.Extendee != nil {
		f.Space()
		f.writeInline(extendNode.Extendee)
	}
	f.Space()
	f.writeCompositeTypeBody(
		extendNode.OpenBrace,
		extendNode.CloseBrace,
		extendNode.Semicolon,
		elementWriterFunc,
	)
}

// writeService writes the service node.
//
// For example,
//
//	service FooService {
//	  option deprecated = true;
//
//	  rpc Foo(FooRequest) returns (FooResponse) {};
func (f *formatter) writeService(serviceNode *ast.ServiceNode) {
	var elementWriterFunc func()
	if len(serviceNode.Decls) > 0 {
		elementWriterFunc = func() {
			for _, decl := range serviceNode.Decls {
				f.writeNode(decl)
			}
		}
	}
	f.writeStart(serviceNode.Keyword)
	f.Space()
	f.writeInline(serviceNode.Name)
	f.Space()
	f.writeCompositeTypeBody(
		serviceNode.OpenBrace,
		serviceNode.CloseBrace,
		serviceNode.Semicolon,
		elementWriterFunc,
	)
}

// writeRPC writes the RPC node. RPCs are formatted in
// the following order:
//
// For example,
//
//	rpc Foo(FooRequest) returns (FooResponse) {
//	  option deprecated = true;
//	};
func (f *formatter) writeRPC(rpcNode *ast.RPCNode) {
	var elementWriterFunc func()
	if len(rpcNode.Decls) > 0 {
		elementWriterFunc = func() {
			for _, decl := range rpcNode.Decls {
				f.writeNode(decl)
			}
		}
	}
	f.writeStart(rpcNode.Keyword)
	f.Space()
	f.writeInline(rpcNode.Name)
	f.writeInline(rpcNode.Input)
	f.Space()
	f.writeInline(rpcNode.Returns)
	f.Space()
	f.writeInline(rpcNode.Output)
	if len(rpcNode.Decls) == 0 {
		// This RPC doesn't have any elements, so we prefer the
		// ';' form.
		//
		//  rpc Ping(PingRequest) returns (PingResponse);
		//
		f.writeLineEnd(&ast.RuneNode{Rune: ';'})
		return
	}
	f.Space()
	f.writeCompositeTypeBody(
		rpcNode.OpenBrace,
		rpcNode.CloseBrace,
		rpcNode.Semicolon,
		elementWriterFunc,
	)
}

// writeRPCType writes the RPC type node (e.g. (stream foo.Bar)).
func (f *formatter) writeRPCType(rpcTypeNode *ast.RPCTypeNode) {
	f.writeInline(rpcTypeNode.OpenParen)
	if rpcTypeNode.Stream != nil {
		f.writeInline(rpcTypeNode.Stream)
		f.Space()
	}
	f.writeInline(rpcTypeNode.MessageType)
	f.writeInline(rpcTypeNode.CloseParen)
}

// writeOneOf writes the oneof node.
//
// For example,
//
//	oneof foo {
//	  option deprecated = true;
//
//	  string name = 1;
//	  int number = 2;
//	}
func (f *formatter) writeOneOf(oneOfNode *ast.OneofNode) {
	var elementWriterFunc func()
	if len(oneOfNode.Decls) > 0 {
		elementWriterFunc = func() {
			columnFormatElements(f, oneOfNode)
		}
	}
	f.writeStart(oneOfNode.Keyword)
	f.Space()
	f.writeInline(oneOfNode.Name)
	f.Space()
	f.writeCompositeTypeBody(
		oneOfNode.OpenBrace,
		oneOfNode.CloseBrace,
		oneOfNode.Semicolon,
		elementWriterFunc,
	)
}

// writeGroup writes the group node.
//
// For example,
//
//	optional group Key = 4 [
//	  deprecated = true,
//	  json_name = "key"
//	] {
//	  optional uint64 id = 1;
//	  optional string name = 2;
//	}
func (f *formatter) writeGroup(groupNode *ast.GroupNode) {
	var elementWriterFunc func()
	if len(groupNode.Decls) > 0 {
		elementWriterFunc = func() {
			for _, decl := range groupNode.Decls {
				f.writeNode(decl)
			}
		}
	}
	// We need to handle the comments for the group label specially since
	// a label might not be defined, but it has the leading comments attached
	// to it.
	if groupNode.Label != nil {
		f.writeStart(groupNode.Label)
		f.Space()
		f.writeInline(groupNode.Keyword)
	} else {
		// If a label was not written, the multiline comments will be
		// attached to the keyword.
		f.writeStart(groupNode.Keyword)
	}
	f.Space()
	f.writeInline(groupNode.Name)
	f.Space()
	f.writeInline(groupNode.Equals)
	f.Space()
	f.writeInline(groupNode.Tag)
	if groupNode.Options != nil {
		f.Space()
		f.writeNode(groupNode.Options)
	}
	f.Space()
	f.writeCompositeTypeBody(
		groupNode.OpenBrace,
		groupNode.CloseBrace,
		groupNode.Semicolon,
		elementWriterFunc,
	)
}

// writeExtensionRange writes the extension range node.
//
// For example,
//
//	extensions 5-10, 100 to max [
//	  deprecated = true
//	];
func (f *formatter) writeExtensionRange(extensionRangeNode *ast.ExtensionRangeNode) {
	f.writeStart(extensionRangeNode.Keyword)
	f.Space()
	for _, elem := range extensionRangeNode.Elements {
		if comma := elem.GetComma(); comma == nil {
			// don't write spaces before commas
			f.Space()
		}
		f.writeInline(elem)
	}
	if extensionRangeNode.Options != nil {
		f.Space()
		f.writeNode(extensionRangeNode.Options)
	}
	f.writeLineEnd(extensionRangeNode.Semicolon)
}

// writeReserved writes a reserved node.
//
// For example,
//
//	reserved 5-10, 100 to max;
func (f *formatter) writeReserved(reservedNode *ast.ReservedNode) {
	f.writeStart(reservedNode.Keyword)
	for _, elem := range reservedNode.Elements {
		if comma := elem.GetComma(); comma == nil {
			// don't write spaces before commas
			f.Space()
		}
		f.writeInline(elem)
	}
	f.writeLineEnd(reservedNode.Semicolon)
}

// writeRange writes the given range node (e.g. '1 to max').
func (f *formatter) writeRange(rangeNode *ast.RangeNode) {
	f.writeInline(rangeNode.StartVal)
	if rangeNode.To != nil {
		f.Space()
		f.writeInline(rangeNode.To)
	}
	// Either EndVal or Max will be set, but never both.
	switch {
	case rangeNode.EndVal != nil:
		f.Space()
		f.writeInline(rangeNode.EndVal)
	case rangeNode.Max != nil:
		f.Space()
		f.writeInline(rangeNode.Max)
	}
}

func (f *formatter) compactOptionsShouldBeExpanded(compactOptionsNode *ast.CompactOptionsNode) bool {
	if len(compactOptionsNode.Options) == 0 {
		return false
	}
	info := f.fileNode.NodeInfo(compactOptionsNode.Options[0])
	if strings.Contains(info.LeadingWhitespace(), "\n") {
		return true
	}
	if hasInteriorComments(f, compactOptionsNode.Options...) {
		return true
	}

	return false
	// if len(compactOptionsNode.Options) > 1 {
	// 	return true
	// }
	// if len(compactOptionsNode.Options) == 1 {
	// 	if f.hasInteriorComments(compactOptionsNode.OpenBracket, compactOptionsNode.Options[0].Name) {
	// 		return true
	// 	}
	// 	switch op := compactOptionsNode.Options[0].Val.(type) {
	// 	case *ast.MessageLiteralNode:
	// 		if messageLiteralHasNestedMessageOrArray(op) {
	// 			return true
	// 		}
	// 	case *ast.ArrayLiteralNode:
	// 		if arrayLiteralHasNestedMessageOrArray(op) {
	// 			return true
	// 		}
	// 	}
	// }
	// return false
}

// writeCompactOptions writes a compact options node.
//
// For example,
//
//	[
//	  deprecated = true,
//	  json_name = "something"
//	]
func (f *formatter) writeCompactOptions(compactOptionsNode *ast.CompactOptionsNode) {
	f.inCompactOptions = true
	defer func() {
		f.inCompactOptions = false
	}()
	if !f.compactOptionsShouldBeExpanded(compactOptionsNode) && !hasInteriorComments(f, compactOptionsNode.Options...) {
		// If there's only a single compact scalar option without comments, we can write it
		// in-line. For example:
		//
		//  [deprecated = true]
		//
		// However, this does not include the case when the '[' has trailing comments,
		// or the option name has leading comments. In those cases, we write the option
		// across multiple lines. For example:
		//
		//  [
		//    // This type is deprecated.
		//    deprecated = true
		//  ]
		//
		if len(compactOptionsNode.Options) > 0 {
			f.writeInline(compactOptionsNode.OpenBracket)
			for i, optionNode := range compactOptionsNode.Options {
				if optionNode.Name == nil && optionNode.Equals == nil && optionNode.Val == nil && optionNode.Semicolon != nil {
					// leading comma, ignore
					continue
				}
				f.writeInline(optionNode.Name)
				f.Space()
				if optionNode.Equals != nil {
					f.writeInline(optionNode.Equals)
				}
				if node := optionNode.Val.GetCompoundStringLiteral(); node != nil {
					// If there's only a single compact option, the value needs to
					// write its comments (if any) in a way that preserves the closing ']'.
					f.writeCompoundStringLiteralNoIndentEndInline(node)
					f.writeInline(compactOptionsNode.CloseBracket)
					return
				}
				f.Space()
				f.writeInline(optionNode.Val)

				if optionNode.Semicolon != nil && i < len(compactOptionsNode.Options)-1 {
					if optionNode.Semicolon.Rune != ',' {
						optionNode.Semicolon.Rune = ','
					}
					f.writeInline(optionNode.Semicolon)
					f.Space()
				}
			}
			f.writeInline(compactOptionsNode.CloseBracket)
		}
		return
	}
	var elementWriterFunc func()
	if len(compactOptionsNode.Options) > 0 {
		elementWriterFunc = func() {
			columnFormatElements(f, compactOptionsNode)
			// for i, opt := range compactOptionsNode.Options {
			// 	if i == len(compactOptionsNode.Options)-1 {
			// 		// The last element won't have a trailing comma.
			// 		f.writeLastCompactOption(opt)
			// 		return
			// 	}
			// 	f.writeNode(opt)
			// 	f.writeLineEnd(compactOptionsNode.Commas[i])
			// }
		}
	}
	f.writeCompositeValueBody(
		compactOptionsNode.OpenBracket,
		compactOptionsNode.CloseBracket,
		compactOptionsNode.Semicolon,
		elementWriterFunc,
	)
}

func hasInteriorComments[T ast.Node](f *formatter, nodes ...T) bool {
	for i, n := range nodes {
		// interior comments mean we ignore leading comments on first
		// token and trailing comments on the last one
		info := f.fileNode.NodeInfo(n)
		if i > 0 && info.LeadingComments().Len() > 0 {
			return true
		}
		if i < len(nodes)-1 && info.TrailingComments().Len() > 0 {
			return true
		}
	}
	return false
}

// writeArrayLiteral writes an array literal across multiple lines.
//
// For example,
//
//	[
//	  "foo",
//	  "bar"
//	]
func (f *formatter) writeArrayLiteral(arrayLiteralNode *ast.ArrayLiteralNode) {
	inline := !f.arrayLiteralShouldBeExpanded(arrayLiteralNode)
	var elementWriterFunc func()
	if len(arrayLiteralNode.Elements) > 0 {
		elementWriterFunc = func() {
			values, commas := arrayLiteralNode.Split() // FIXME
			for i := 0; i < len(values); i++ {
				lastElement := i == len(values)-1
				if _, ok := values[i].Unwrap().(ast.TerminalNode); !ok {
					if inline {
						f.writeCompositeValueForArrayLiteral(values[i], false)
					} else {
						f.writeCompositeValueForArrayLiteral(values[i], lastElement)
					}
					if !lastElement {
						if inline {
							f.writeInline(commas[i])
							f.Space()
						} else {
							f.writeLineEnd(commas[i])
						}
					}
					continue
				}
				if lastElement {
					// The last element won't have a trailing comma.
					if inline {
						f.writeBodyEndInline(values[i], nil, true)
					} else {
						f.writeLineElement(values[i])
					}
					return
				}
				f.writeStartMaybeCompact(values[i], inline)
				if inline {
					f.writeInline(commas[i])
					f.Space()
				} else {
					f.writeLineEnd(commas[i])
				}
			}
		}
	}
	var maybeOpenBraceWriterFunc []func(ast.Node)
	if inline {
		maybeOpenBraceWriterFunc = append(maybeOpenBraceWriterFunc, f.writeOpenBracePrefixInline)
	}
	f.writeCompositeValueBody(
		arrayLiteralNode.OpenBracket,
		arrayLiteralNode.CloseBracket,
		arrayLiteralNode.Semicolon,
		elementWriterFunc,
		maybeOpenBraceWriterFunc...,
	)
}

// writeCompositeForArrayLiteral writes the composite node in a way that's suitable
// for array literals. In general, signed integers and compound strings should have their
// comments written in-line because they are one of many components in a single line.
//
// However, each of these composite types occupy a single line in an array literal,
// so they need their comments to be formatted like a standalone node.
//
// For example,
//
//	option (value) = /* In-line comment for '-42' */ -42;
//
//	option (thing) = {
//	  values: [
//	    // Leading comment on -42.
//	    -42, // Trailing comment on -42.
//	  ]
//	}
//
// The lastElement boolean is used to signal whether or not the composite value
// should be written as the last element (i.e. it doesn't have a trailing comma).
func (f *formatter) writeCompositeValueForArrayLiteral(
	compositeNode ast.Node,
	lastElement bool,
) {
	switch node := ast.Unwrap(compositeNode).(type) {
	case *ast.CompoundStringLiteralNode:
		f.writeCompoundStringLiteralForArray(node, lastElement)
	case *ast.NegativeIntLiteralNode:
		f.writeNegativeIntLiteralForArray(node, lastElement)
	case *ast.SignedFloatLiteralNode:
		f.writeSignedFloatLiteralForArray(node, lastElement)
	case *ast.MessageLiteralNode:
		f.writeMessageLiteralForArray(node, lastElement)
	default:
		panic(fmt.Errorf("unexpected array value node %T", node))
	}
}

// writeCompositeTypeBody writes the body of a composite type, e.g. message, enum, extend, oneof, etc.
func (f *formatter) writeCompositeTypeBody(
	openBrace *ast.RuneNode,
	closeBrace *ast.RuneNode,
	semicolon *ast.RuneNode,
	elementWriterFunc func(),
) {
	f.writeBody(
		openBrace,
		closeBrace,
		semicolon,
		elementWriterFunc,
		f.writeOpenBracePrefix,
		f.writeBodyEnd,
	)
}

// writeCompositeValueBody writes the body of a composite value, e.g. compact options,
// array literal, etc. We need to handle the ']' different than composite types because
// there could be more tokens following the final ']'.
func (f *formatter) writeCompositeValueBody(
	openBrace *ast.RuneNode,
	closeBrace *ast.RuneNode,
	semicolon *ast.RuneNode,
	elementWriterFunc func(),
	maybeOpenBraceWriterFunc ...func(ast.Node),
) {
	if len(maybeOpenBraceWriterFunc) == 0 {
		maybeOpenBraceWriterFunc = append(maybeOpenBraceWriterFunc, f.writeOpenBracePrefix)
	}
	f.writeBody(
		openBrace,
		closeBrace,
		semicolon,
		elementWriterFunc,
		maybeOpenBraceWriterFunc[0],
		f.writeBodyEndInline,
	)
}

// writeBody writes the body of a type or value, e.g. message, enum, compact options, etc.
// The elementWriterFunc is used to write the declarations within the composite type (e.g.
// fields in a message). The openBraceWriterFunc and closeBraceWriterFunc functions are used
// to customize how the '{' and '} nodes are written, respectively.
func (f *formatter) writeBody(
	openBrace *ast.RuneNode,
	closeBrace *ast.RuneNode,
	semicolon *ast.RuneNode,
	elementWriterFunc func(),
	openBraceWriterFunc func(ast.Node),
	closeBraceWriterFunc func(ast.Node, *ast.RuneNode, bool),
) {
	if openBrace != nil && closeBrace != nil {
		if elementWriterFunc == nil && !hasInteriorComments(f, openBrace, closeBrace) {
			// completely empty body
			f.writeInline(openBrace)
			closeBraceWriterFunc(closeBrace, semicolon, true)
			return
		}
	}

	if openBrace != nil {
		openBraceWriterFunc(openBrace)
	}
	if elementWriterFunc != nil {
		elementWriterFunc()
	}
	if closeBrace != nil {
		closeBraceWriterFunc(closeBrace, semicolon, false)
	}
}

// writeOpenBracePrefix writes the open brace with its leading comments in-line.
// This is used for nearly every use case of f.writeBody, excluding the instances
// in array literals.
func (f *formatter) writeOpenBracePrefix(openBrace ast.Node) {
	defer f.SetPreviousNode(openBrace)
	info := f.fileNode.NodeInfo(openBrace)
	if info.LeadingComments().Len() > 0 {
		f.writeInlineComments(info.LeadingComments())
		if info.LeadingWhitespace() != "" {
			f.Space()
		}
	}
	f.writeNode(openBrace)
	if info.TrailingComments().Len() > 0 {
		f.writeTrailingEndComments(info.TrailingComments())
	} else {
		f.P("")
	}
}

func (f *formatter) writeOpenBracePrefixInline(openBrace ast.Node) {
	defer f.SetPreviousNode(openBrace)
	info := f.fileNode.NodeInfo(openBrace)
	if info.LeadingComments().Len() > 0 {
		f.writeInlineComments(info.LeadingComments())
		if info.LeadingWhitespace() != "" {
			f.Space()
		}
	}
	f.writeNode(openBrace)
	if info.TrailingComments().Len() > 0 {
		f.writeInlineComments(info.TrailingComments())
	}
}

// writeOpenBracePrefixForArray writes the open brace with its leading comments
// on multiple lines. This is only used for message literals in arrays.
func (f *formatter) writeOpenBracePrefixForArray(openBrace ast.Node) {
	defer f.SetPreviousNode(openBrace)
	info := f.fileNode.NodeInfo(openBrace)
	if info.LeadingComments().Len() > 0 {
		f.writeMultilineComments(info.LeadingComments())
	}
	f.Indent(openBrace)
	f.writeNode(openBrace)
	if info.TrailingComments().Len() > 0 {
		f.writeTrailingEndComments(info.TrailingComments())
	} else {
		f.P("")
	}
}

// writeCompoundIdent writes a compound identifier (e.g. '.com.foo.Bar').
func (f *formatter) writeCompoundIdent(compoundIdentNode *ast.CompoundIdentNode) {
	for _, node := range compoundIdentNode.Components {
		f.writeInline(node)
	}
}

// writeCompountIdentForFieldName writes a compound identifier, but handles comments
// specially for field names.
//
// For example,
//
//	message Foo {
//	  // These are comments attached to bar.
//	  bar.v1.Bar bar = 1;
//	}
func (f *formatter) writeCompountIdentForFieldName(compoundIdentNode *ast.CompoundIdentNode, maybeNodeWriter ...func(ast.Node)) {
	leadingDot, ok := compoundIdentNode.Components[0].Unwrap().(*ast.RuneNode)
	if ok {
		f.writeStart(leadingDot, maybeNodeWriter...)
	}
	orderedNodes := compoundIdentNode.Components
	for i, node := range orderedNodes {
		if i == 0 && leadingDot == nil {
			f.writeStart(node, maybeNodeWriter...)
			continue
		}
		f.writeInline(node)
	}
}

// writeCompoundStringLiteral writes a compound string literal value.
//
// For example,
//
//	"one,"
//	"two,"
//	"three"
func (f *formatter) writeCompoundStringLiteral(
	compoundStringLiteralNode *ast.CompoundStringLiteralNode,
	needsIndent bool,
	hasTrailingPunctuation bool,
) {
	f.P("")
	if needsIndent {
		f.In()
	}
	for i, child := range compoundStringLiteralNode.Elements {
		if hasTrailingPunctuation && i == len(compoundStringLiteralNode.Elements)-1 {
			// inline because there may be a subsequent comma or punctuation from enclosing element
			f.writeStart(child)
			break
		}
		f.writeLineElement(child)
	}
	if needsIndent {
		f.Out()
	}
}

func (f *formatter) writeCompoundStringLiteralIndent(
	compoundStringLiteralNode *ast.CompoundStringLiteralNode,
) {
	f.writeCompoundStringLiteral(compoundStringLiteralNode, true, false)
}

func (f *formatter) writeCompoundStringLiteralIndentEndInline(
	compoundStringLiteralNode *ast.CompoundStringLiteralNode,
) {
	f.writeCompoundStringLiteral(compoundStringLiteralNode, true, true)
}

func (f *formatter) writeCompoundStringLiteralNoIndentEndInline(
	compoundStringLiteralNode *ast.CompoundStringLiteralNode,
) {
	f.writeCompoundStringLiteral(compoundStringLiteralNode, false, true)
}

// writeCompoundStringLiteralForArray writes a compound string literal value,
// but writes its comments suitable for an element in an array literal.
//
// The lastElement boolean is used to signal whether or not the value should
// be written as the last element (i.e. it doesn't have a trailing comma).
func (f *formatter) writeCompoundStringLiteralForArray(
	compoundStringLiteralNode *ast.CompoundStringLiteralNode,
	lastElement bool,
) {
	for i, child := range compoundStringLiteralNode.Elements {
		if !lastElement && i == len(compoundStringLiteralNode.Elements)-1 {
			f.writeStart(child)
			return
		}
		f.writeLineElement(child)
	}
}

// writeFloatLiteral writes a float literal value (e.g. '42.2').
func (f *formatter) writeFloatLiteral(floatLiteralNode *ast.FloatLiteralNode) {
	f.WriteString(floatLiteralNode.Raw)
}

// writeSignedFloatLiteral writes a signed float literal value (e.g. '-42.2').
func (f *formatter) writeSignedFloatLiteral(signedFloatLiteralNode *ast.SignedFloatLiteralNode) {
	f.writeInline(signedFloatLiteralNode.Sign)
	f.writeInline(signedFloatLiteralNode.Float)
}

// writeSignedFloatLiteralForArray writes a signed float literal value, but writes
// its comments suitable for an element in an array literal.
//
// The lastElement boolean is used to signal whether or not the value should
// be written as the last element (i.e. it doesn't have a trailing comma).
func (f *formatter) writeSignedFloatLiteralForArray(
	signedFloatLiteralNode *ast.SignedFloatLiteralNode,
	lastElement bool,
) {
	f.writeStart(signedFloatLiteralNode.Sign)
	if lastElement {
		f.writeLineEnd(signedFloatLiteralNode.Float)
		return
	}
	f.writeInline(signedFloatLiteralNode.Float)
}

// writeSpecialFloatLiteral writes a special float literal value (e.g. "nan" or "inf").
func (f *formatter) writeSpecialFloatLiteral(specialFloatLiteralNode *ast.SpecialFloatLiteralNode) {
	f.WriteString(specialFloatLiteralNode.GetKeyword().GetVal())
}

// writeStringLiteral writes a string literal value (e.g. "foo").
// Note that the raw string is written as-is so that it preserves
// the quote style used in the original source.
func (f *formatter) writeStringLiteral(stringLiteralNode *ast.StringLiteralNode) {
	info := f.fileNode.NodeInfo(stringLiteralNode)
	rawText := info.RawText()
	if len(rawText) > 1 && rawText[0] == '\'' && rawText[len(rawText)-1] == '\'' {
		// convert single quotes to double quotes
		b := []rune(rawText)
		b[0] = '"'
		b[len(b)-1] = '"'
		rawText = string(b)
	}
	f.WriteString(rawText)
}

// writeUintLiteral writes a uint literal (e.g. '42').
func (f *formatter) writeUintLiteral(uintLiteralNode *ast.UintLiteralNode) {
	if uintLiteralNode.Raw != "" {
		f.WriteString(uintLiteralNode.Raw)
		return
	}
	f.WriteString(strconv.FormatUint(uint64(uintLiteralNode.Val), 10))
}

// writeNegativeIntLiteral writes a negative int literal (e.g. '-42').
func (f *formatter) writeNegativeIntLiteral(negativeIntLiteralNode *ast.NegativeIntLiteralNode) {
	f.writeInline(negativeIntLiteralNode.Minus)
	f.writeInline(negativeIntLiteralNode.Uint)
}

// writeNegativeIntLiteralForArray writes a negative int literal value, but writes
// its comments suitable for an element in an array literal.
//
// The lastElement boolean is used to signal whether or not the value should
// be written as the last element (i.e. it doesn't have a trailing comma).
func (f *formatter) writeNegativeIntLiteralForArray(
	negativeIntLiteralNode *ast.NegativeIntLiteralNode,
	lastElement bool,
) {
	f.writeStart(negativeIntLiteralNode.Minus)
	if lastElement {
		f.writeLineEnd(negativeIntLiteralNode.Uint)
		return
	}
	f.writeInline(negativeIntLiteralNode.Uint)
}

// writeIdent writes an identifier (e.g. 'foo').
func (f *formatter) writeIdent(identNode *ast.IdentNode) {
	f.WriteString(identNode.Val)
}

// writeKeyword writes a keyword (e.g. 'syntax').
func (f *formatter) writeKeyword(keywordNode *ast.IdentNode) {
	f.WriteString(keywordNode.Val)
}

// writeRune writes a rune (e.g. '=').
func (f *formatter) writeRune(runeNode *ast.RuneNode) {
	if strings.ContainsRune("{[(<", runeNode.Rune) {
		f.pendingIndent++
	} else if strings.ContainsRune("}])>", runeNode.Rune) {
		f.pendingIndent--
	}
	f.WriteString(string(runeNode.Rune))
}

// writeNode writes the node by dispatching to a function tailored to its concrete type.
//
// Comments are handled in each respective write function so that it can determine whether
// to write the comments in-line or not.
func (f *formatter) writeNode(node ast.Node) {
	node = ast.Unwrap(node)

	switch element := node.(type) {
	case *ast.ArrayLiteralNode:
		f.writeArrayLiteral(element)
	case *ast.CompactOptionsNode:
		f.writeCompactOptions(element)
	case *ast.CompoundIdentNode:
		f.writeCompoundIdent(element)
	case *ast.CompoundStringLiteralNode:
		f.writeCompoundStringLiteralIndentEndInline(element)
	case *ast.EnumNode:
		f.writeEnum(element)
	case *ast.EnumValueNode:
		f.writeEnumValue(element)
	case *ast.ExtendNode:
		f.writeExtend(element)
	case *ast.ExtensionRangeNode:
		f.writeExtensionRange(element)
	case *ast.FieldNode:
		f.writeField(element)
	case *ast.FieldReferenceNode:
		f.writeFieldReference(element)
	case *ast.FloatLiteralNode:
		f.writeFloatLiteral(element)
	case *ast.GroupNode:
		f.writeGroup(element)
	case *ast.IdentNode:
		f.writeIdent(element)
	case *ast.ImportNode:
		f.writeImport(element, false)
	case *ast.MapFieldNode:
		f.writeMapField(element)
	case *ast.MapTypeNode:
		f.writeMapType(element)
	case *ast.MessageNode:
		f.writeMessage(element)
	case *ast.MessageFieldNode:
		f.writeMessageField(element)
	case *ast.MessageLiteralNode:
		f.writeMessageLiteral(element)
	case *ast.NegativeIntLiteralNode:
		f.writeNegativeIntLiteral(element)
	case *ast.OneofNode:
		f.writeOneOf(element)
	case *ast.OptionNode:
		f.writeOption(element)
	case *ast.OptionNameNode:
		f.writeOptionName(element)
	case *ast.PackageNode:
		f.writePackage(element)
	case *ast.RangeNode:
		f.writeRange(element)
	case *ast.ReservedNode:
		f.writeReserved(element)
	case *ast.RPCNode:
		f.writeRPC(element)
	case *ast.RPCTypeNode:
		f.writeRPCType(element)
	case *ast.RuneNode:
		f.writeRune(element)
	case *ast.ServiceNode:
		f.writeService(element)
	case *ast.SignedFloatLiteralNode:
		f.writeSignedFloatLiteral(element)
	case *ast.SpecialFloatLiteralNode:
		f.writeSpecialFloatLiteral(element)
	case *ast.StringLiteralNode:
		f.writeStringLiteral(element)
	case *ast.SyntaxNode:
		f.writeSyntax(element)
	case *ast.EditionNode:
		f.writeEdition(element)
	case *ast.UintLiteralNode:
		f.writeUintLiteral(element)
	case *ast.EmptyDeclNode:
		// Nothing to do here.
	default:
		f.err = errors.Join(f.err, fmt.Errorf("unexpected node: %T", node))
	}
}

// writeStart writes the node across as the start of a line.
// Start nodes have their leading comments written across
// multiple lines, but their trailing comments must be written
// in-line to preserve the line structure.
//
// For example,
//
//	// Leading comment on 'message'.
//	// Spread across multiple lines.
//	message /* This is a trailing comment on 'message' */ Foo {}
//
// Newlines are preserved, so that any logical grouping of elements
// is maintained in the formatted result.
//
// For example,
//
//	// Type represents a set of different types.
//	enum Type {
//	  // Unspecified is the naming convention for default enum values.
//	  TYPE_UNSPECIFIED = 0;
//
//	  // The following elements are the real values.
//	  TYPE_ONE = 1;
//	  TYPE_TWO = 2;
//	}
//
// Start nodes are always indented according to the formatter's
// current level of indentation (e.g. nested messages, fields, etc).
//
// Note that this is one of the most complex component of the formatter - it
// controls how each node should be separated from one another and preserves
// newlines in the original source.
func (f *formatter) writeStart(node ast.Node, maybeNodeWriter ...func(ast.Node)) {
	f.writeStartMaybeCompact(node, false, maybeNodeWriter...)
}

func (f *formatter) writeStartMaybeCompact(node ast.Node, forceCompact bool, maybeNodeWriter ...func(ast.Node)) {
	node = ast.Unwrap(node)

	nodeWriter := f.writeNode
	if len(maybeNodeWriter) > 0 {
		nodeWriter = maybeNodeWriter[0]
	}

	defer f.SetPreviousNode(node)
	info := f.fileNode.NodeInfo(node)
	var (
		nodeNewlineCount = newlineCount(info.LeadingWhitespace())
		compact          = forceCompact || isOpenBrace(f.previousNode)
	)
	if length := info.LeadingComments().Len(); length > 0 {
		// If leading comments are defined, the whitespace we care about
		// is attached to the first comment.
		f.writeMultilineCommentsMaybeCompact(info.LeadingComments(), forceCompact)
		if !forceCompact && nodeNewlineCount > 1 {
			// At this point, we're looking at the lines between
			// a comment and the node its attached to.
			//
			// If the last comment is a standard comment, a single newline
			// character is sufficient to warrant a separation of the
			// two.
			//
			// If the last comment is a C-style comment, multiple newline
			// characters are required because C-style comments don't consume
			// a newline.
			f.P("")
		}
	} else if !compact && nodeNewlineCount > 1 {
		// If the previous node is an open brace, this is the first element
		// in the body of a composite type, so we don't want to write a
		// newline. This makes it so that trailing newlines are removed.
		//
		// For example,
		//
		//  message Foo {
		//
		//    string bar = 1;
		//  }
		//
		// Is formatted into the following:
		//
		//  message Foo {
		//    string bar = 1;
		//  }
		f.P("")
	}
	f.Indent(node)
	nodeWriter(node)
	if info.TrailingComments().Len() > 0 {
		f.writeInlineComments(info.TrailingComments())
	}
}

// writeInline writes the node and its surrounding comments in-line.
//
// This is useful for writing individual nodes like keywords, runes,
// string literals, etc.
//
// For example,
//
//	// This is a leading comment on the syntax keyword.
//	syntax = /* This is a leading comment on 'proto3' */" proto3";
func (f *formatter) writeInline(node ast.Node) {
	node = ast.Unwrap(node)

	f.inline = true
	defer func() {
		f.inline = false
	}()
	if _, ok := node.(ast.TerminalNode); !ok {
		// We only want to write comments for terminal nodes.
		// Otherwise comments accessible from CompositeNodes
		// will be written twice.
		f.writeNode(node)
		return
	}
	defer f.SetPreviousNode(node)
	info := f.fileNode.NodeInfo(node)
	if info.LeadingComments().Len() > 0 {
		f.writeInlineComments(info.LeadingComments())
		if info.LeadingWhitespace() != "" {
			f.Space()
		}
	}
	f.writeNode(node)
	f.writeInlineComments(info.TrailingComments())
}

// writeBodyEnd writes the node as the end of a body.
// Leading comments are written above the token across
// multiple lines, whereas the trailing comments are
// written in-line and preserve their format.
//
// Body end nodes are always indented according to the
// formatter's current level of indentation (e.g. nested
// messages).
//
// This is useful for writing a node that concludes a
// composite node: ']', '}', '>', etc.
//
// For example,
//
//	message Foo {
//	  string bar = 1;
//	  // Leading comment on '}'.
//	} // Trailing comment on '}.
func (f *formatter) writeBodyEnd(node ast.Node, semicolon *ast.RuneNode, leadingEndline bool) {
	node = ast.Unwrap(node)

	if _, ok := node.(ast.TerminalNode); !ok {
		// We only want to write comments for terminal nodes.
		// Otherwise comments accessible from CompositeNodes
		// will be written twice.
		f.writeNode(node)
		if f.lastWritten != '\n' {
			f.P("")
		}
		return
	}
	defer f.SetPreviousNode(node)
	info := f.fileNode.NodeInfo(node)
	if leadingEndline {
		if info.LeadingComments().Len() > 0 {
			f.writeInlineComments(info.LeadingComments())
			if info.LeadingWhitespace() != "" {
				f.Space()
			}
		}
	} else {
		f.writeMultilineComments(info.LeadingComments())
		f.Indent(node)
	}
	f.writeNode(node)

	var trailingComments ast.Comments
	if info.TrailingComments().Len() > 0 {
		trailingComments = info.TrailingComments()
	} else if semicolon != nil && semicolon.Virtual {
		f.Space() // always insert a space here, since there would always be "leading whitespace"
		trailingComments = f.fileNode.NodeInfo(semicolon).TrailingComments()
	}
	f.writeTrailingEndComments(trailingComments)
}

func (f *formatter) writeLineElement(node ast.Node) {
	f.writeBodyEnd(node, nil, false)
}

// writeBodyEndInline writes the node as the end of a body.
// Leading comments are written above the token across
// multiple lines, whereas the trailing comments are
// written in-line and adapt their comment style if they
// exist.
//
// Body end nodes are always indented according to the
// formatter's current level of indentation (e.g. nested
// messages).
//
// This is useful for writing a node that concludes either
// compact options or an array literal.
//
// This is behaviorally similar to f.writeStart, but it ignores
// the preceding newline logic because these body ends should
// always be compact.
//
// For example,
//
//	message Foo {
//	  string bar = 1 [
//	    deprecated = true
//
//	    // Leading comment on ']'.
//	  ] /* Trailing comment on ']' */ ;
//	}
func (f *formatter) writeBodyEndInline(node ast.Node, semicolon *ast.RuneNode, leadingInline bool) {
	node = ast.Unwrap(node)

	if _, ok := node.(ast.TerminalNode); !ok {
		// We only want to write comments for terminal nodes.
		// Otherwise comments accessible from CompositeNodes
		// will be written twice.
		f.writeNode(node)
		return
	}
	defer f.SetPreviousNode(node)
	info := f.fileNode.NodeInfo(node)
	if leadingInline {
		if info.LeadingComments().Len() > 0 {
			f.writeInlineComments(info.LeadingComments())
			if info.LeadingWhitespace() != "" {
				f.Space()
			}
		}
	} else {
		f.writeMultilineComments(info.LeadingComments())
		f.Indent(node)
	}
	f.writeNode(node)

	var trailingComments ast.Comments
	if info.TrailingComments().Len() > 0 {
		trailingComments = info.TrailingComments()
	} else if semicolon != nil && semicolon.Virtual {
		trailingComments = f.fileNode.NodeInfo(semicolon).TrailingComments()
	}
	if trailingComments.Len() > 0 {
		f.writeInlineComments(trailingComments)
	}
}

// writeLineEnd writes the node so that it ends a line.
//
// This is useful for writing individual nodes like ';' and other
// tokens that conclude the end of a single line. In this case, we
// don't want to transform the trailing comment's from '//' to C-style
// because it's not necessary.
//
// For example,
//
//	// This is a leading comment on the syntax keyword.
//	syntax = " proto3" /* This is a leading comment on the ';'; // This is a trailing comment on the ';'.
func (f *formatter) writeLineEnd(node ast.Node) {
	node = ast.Unwrap(node)
	if _, ok := node.(ast.TerminalNode); !ok {
		// We only want to write comments for terminal nodes.
		// Otherwise comments accessible from CompositeNodes
		// will be written twice.
		f.writeNode(node)
		if f.lastWritten != '\n' {
			f.P("")
		}
		return
	}
	defer f.SetPreviousNode(node)
	info := f.fileNode.NodeInfo(node)
	if info.LeadingComments().Len() > 0 {
		f.writeInlineComments(info.LeadingComments())
		if info.LeadingWhitespace() != "" {
			f.Space()
		}
	}
	f.writeNode(node)
	f.Space()
	f.writeTrailingEndComments(info.TrailingComments())
}

// writeMultilineComments writes the given comments as a newline-delimited block.
// This is useful for both the beginning of a type (e.g. message, field, etc), as
// well as the trailing comments attached to the beginning of a body block (e.g.
// '{', '[', '<', etc).
//
// For example,
//
//	// This is a comment spread across
//	// multiple lines.
//	message Foo {}
func (f *formatter) writeMultilineComments(comments ast.Comments) {
	f.writeMultilineCommentsMaybeCompact(comments, false)
}

func (f *formatter) writeMultilineCommentsMaybeCompact(comments ast.Comments, forceCompact bool) {
	compact := forceCompact || isOpenBrace(f.previousNode)
	for i := 0; i < comments.Len(); i++ {
		comment := comments.Index(i)
		if !compact && newlineCount(comment.LeadingWhitespace()) > 1 {
			// Newlines between blocks of comments should be preserved.
			//
			// For example,
			//
			//  // This is a license header
			//  // spread across multiple lines.
			//
			//  // Package pet.v1 defines a PetStore API.
			//  package pet.v1;
			//
			f.P("")
		}
		compact = false
		f.writeComment(comment.RawText())
		f.WriteString("\n")
	}
}

// writeInlineComments writes the given comments in-line. Standard comments are
// transformed to C-style comments so that we can safely write the comment in-line.
//
// Nearly all of these comments will already be C-style comments. The only cases we're
// preventing are when the type is defined across multiple lines.
//
// For example, given the following:
//
//	extend . google. // in-line comment
//	 protobuf .
//	  ExtensionRangeOptions {
//	   optional string label = 20000;
//	  }
//
// The formatted result is shown below:
//
//	extend .google.protobuf./* in-line comment */ExtensionRangeOptions {
//	  optional string label = 20000;
//	}
func (f *formatter) writeInlineComments(comments ast.Comments) {
	for i := 0; i < comments.Len(); i++ {
		comment := comments.Index(i)
		if comment.IsVirtual() {
			continue
		}
		if i > 0 || comment.LeadingWhitespace() != "" || f.lastWritten == ';' || f.lastWritten == '}' {
			f.Space()
		}
		text := comment.RawText()
		if strings.HasPrefix(text, "//") {
			text = strings.TrimSpace(strings.TrimPrefix(text, "//"))
			text = "/* " + text + " */"
		} else {
			// no multi-line comments
			lines := strings.Split(text, "\n")
			for i := range lines {
				lines[i] = strings.TrimSpace(lines[i])
			}
			text = strings.Join(lines, " ")
		}
		f.WriteString(text)
	}
}

// writeTrailingEndComments writes the given comments at the end of a line and
// preserves the comment style. This is useful or writing comments attached to
// things like ';' and other tokens that conclude a type definition on a single
// line.
//
// If there is a newline between this trailing comment and the previous node, the
// comments are written immediately underneath the node on a newline.
//
// For example,
//
//	enum Type {
//	  TYPE_UNSPECIFIED = 0;
//	}
//	// This comment is attached to the '}'
//	// So is this one.
func (f *formatter) writeTrailingEndComments(comments ast.Comments) {
	for i := 0; i < comments.Len(); i++ {
		comment := comments.Index(i)
		if lws := comment.LeadingWhitespace(); len(lws) > 0 {
			if strings.Contains(lws, "\n") {
				f.P("")
			} else if i > 0 {
				f.Space()
			}
		}
		f.writeComment(comment.RawText())
	}
	f.P("")
}

func (f *formatter) writeComment(comment string) {
	if strings.HasPrefix(comment, "/*") && newlineCount(comment) > 0 {
		lines := strings.Split(comment, "\n")
		// find minimum indent, so we can make all other lines relative to that
		minIndent := -1 // sentinel that means unset
		// start at 1 because line at index zero starts with "/*", not whitespace
		var prefix string
		for i := 1; i < len(lines); i++ {
			indent, ok := computeIndent(lines[i])
			if ok && (minIndent == -1 || indent < minIndent) {
				minIndent = indent
			}
			if i > 1 && len(prefix) == 0 {
				// no shared prefix
				continue
			}
			line := strings.TrimSpace(lines[i])
			if line == "*/" {
				continue
			}
			var linePrefix string
			if len(line) > 0 && isCommentPrefix(line[0]) {
				linePrefix = line[:1]
			}
			if i == 1 {
				prefix = linePrefix
			} else if linePrefix != prefix {
				// they do not share prefix
				prefix = ""
			}
		}
		if minIndent < 0 {
			// This shouldn't be necessary.
			// But we do it just in case, to avoid possible panic
			minIndent = 0
		}
		for i, line := range lines {
			trimmedLine := strings.TrimSpace(line)
			if trimmedLine == "" || trimmedLine == "*/" || len(prefix) > 0 {
				line = trimmedLine
			} else {
				// we only trim space from the right; for the left,
				// we unindent based on indentation found above.
				line = unindent(line, minIndent)
				line = strings.TrimRightFunc(line, unicode.IsSpace)
			}
			// If we have a block comment with no prefix, we'll format
			// like so:

			/*
			   This is a multi-line comment example.
			   It has no comment prefix on each line.
			*/

			// But if there IS a prefix, "|" for example, we'll left-align
			// the prefix symbol under the asterisk of the comment start
			// like this:

			/*
			 | This comment has a prefix before each line.
			 | Usually the prefix is asterisk, but it's a
			 | pipe in this example.
			*/

			// Finally, if the comment prefix is an asterisk, we'll left-align
			// the comment end so its asterisk also aligns, like so:

			/*
			 * This comment has a prefix before each line.
			 * Usually the prefix is asterisk, which is the
			 * case in this example.
			 */

			if i > 0 && line != "*/" {
				if len(prefix) == 0 {
					line = "   " + line
				} else {
					line = " " + line
				}
			}
			if line == "*/" && prefix == "*" {
				// align the comment end with the other asterisks
				line = " " + line
			}

			if i != len(lines)-1 {
				f.P(line)
			} else {
				// for last line, we don't use P because we don't
				// want to print a trailing newline
				f.Indent(nil)
				f.WriteString(line)
			}
		}
	} else {
		f.Indent(nil)
		f.WriteString(strings.TrimSpace(comment))
	}
}

func isCommentPrefix(ch byte) bool {
	r := rune(ch)
	// A multi-line comment prefix is *usually* an asterisk, like in the following
	/*
	 * Foo
	 * Bar
	 * Baz
	 */
	// But we'll allow other prefixes. But if it's a letter or number, it's not a prefix.
	return !unicode.IsLetter(r) && !unicode.IsNumber(r)
}

func unindent(s string, unindent int) string {
	pos := 0
	for i, r := range s {
		if pos == unindent {
			return s[i:]
		}
		if pos > unindent {
			// removing tab-stop unindented too far, so we
			// add back some spaces to compensate
			return strings.Repeat(" ", pos-unindent) + s[i:]
		}

		switch r {
		case ' ':
			pos++
		case '\t':
			// jump to next tab stop
			pos += 8 - (pos % 8)
		default:
			return s[i:]
		}
	}
	// nothing but whitespace...
	return ""
}

func computeIndent(s string) (int, bool) {
	if strings.TrimSpace(s) == "*/" {
		return 0, false
	}
	indent := 0
	for _, r := range s {
		switch r {
		case ' ':
			indent++
		case '\t':
			// jump to next tab stop
			indent += 8 - (indent % 8)
		default:
			return indent, true
		}
	}
	// if we get here, line is nothing but whitespace
	return 0, false
}

func (f *formatter) leadingCommentsContainBlankLine(n ast.Node) bool {
	info := f.fileNode.NodeInfo(n)
	comments := info.LeadingComments()
	for i := 0; i < comments.Len(); i++ {
		if newlineCount(comments.Index(i).LeadingWhitespace()) > 1 {
			return true
		}
	}
	return newlineCount(info.LeadingWhitespace()) > 1
}

func (f *formatter) importHasComment(importNode *ast.ImportNode) bool {
	if f.nodeHasComment(importNode) {
		return true
	}
	if importNode == nil {
		return false
	}

	return f.nodeHasComment(importNode.Keyword) ||
		f.nodeHasComment(importNode.Name) ||
		f.nodeHasComment(importNode.Semicolon) ||
		f.nodeHasComment(importNode.Public) ||
		f.nodeHasComment(importNode.Weak)
}

func (f *formatter) nodeHasComment(node ast.Node) bool {
	node = ast.Unwrap(node)

	// when node != nil, node's value could be nil, see: https://go.dev/doc/faq#nil_error
	if ast.IsNil(node) {
		return false
	}

	nodeinfo := f.fileNode.NodeInfo(node)
	return nodeinfo.LeadingComments().Len() > 0 ||
		nodeinfo.TrailingComments().Len() > 0
}

// importSortOrder maps import types to a sort order number, so it can be compared and sorted.
// `import`=3, `import public`=2, `import weak`=1
func importSortOrder(node *ast.ImportNode) int {
	switch {
	case node.Public != nil:
		return 2
	case node.Weak != nil:
		return 1
	default:
		return 3
	}
}

// StringForOptionName returns the string representation of the given option name node.
// This is used for sorting file-level options.
func StringForOptionName(optionNameNode *ast.OptionNameNode) string {
	if optionNameNode == nil {
		return ""
	}
	var result string
	for j, part := range optionNameNode.FilterFieldReferences() {
		if j > 0 {
			// Add a dot between each of the parts.
			result += "."
		}
		result += StringForFieldReference(part)
	}
	return result
}

// StringForFieldReference returns the string representation of the given field reference.
// This is used for sorting file-level options.
func StringForFieldReference(fieldReference *ast.FieldReferenceNode) string {
	var result string
	if fieldReference.Open != nil {
		result += "("
	}
	result += string(fieldReference.Name.AsIdentifier())
	if fieldReference.Close != nil {
		result += ")"
	} else if fieldReference.Open != nil {
		// (extended syntax rule) fill in missing close paren automatically
		result += ")"
	}
	return result
}

// isOpenBrace returns true if the given node represents one of the
// possible open brace tokens, namely '{', '[', or '<'.
func isOpenBrace(node ast.Node) bool {
	node = ast.Unwrap(node)

	if node == nil {
		return false
	}
	runeNode, ok := node.(*ast.RuneNode)
	if !ok {
		return false
	}
	return runeNode.Rune == '{' || runeNode.Rune == '[' || runeNode.Rune == '<'
}

// newlineCount returns the number of newlines in the given value.
// This is useful for determining whether or not we should preserve
// the newline between nodes.
//
// The newlines don't need to be adjacent to each other - all of the
// tokens between them are other whitespace characters, so we can
// safely ignore them.
func newlineCount(value string) int {
	return strings.Count(value, "\n")
}
