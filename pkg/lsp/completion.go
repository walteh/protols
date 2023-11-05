package lsp

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/linker"
	"github.com/bufbuild/protocompile/parser"
	"golang.org/x/tools/gopls/pkg/lsp/protocol"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func (c *Cache) GetCompletions(params *protocol.CompletionParams) (result *protocol.CompletionList, err error) {
	doc := params.TextDocument
	currentParseRes, err := c.FindParseResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	maybeCurrentLinkRes, err := c.FindResultByURI(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	mapper, err := c.GetMapper(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}
	posOffset, err := mapper.PositionOffset(params.Position)
	if err != nil {
		return nil, err
	}
	start, end, err := mapper.RangeOffsets(protocol.Range{
		Start: protocol.Position{
			Line:      params.Position.Line,
			Character: 0,
		},
		End: params.Position,
	})
	if err != nil {
		return nil, err
	}
	textPrecedingCursor := string(mapper.Content[start:end])

	latestAstValid, err := c.LatestDocumentContentsWellFormed(doc.URI.SpanURI())
	if err != nil {
		return nil, err
	}

	var searchTarget parser.Result
	if !latestAstValid {
		// The user is in the middle of editing the file and it's not valid yet,
		// so `currentParseRes` has a separate AST than `maybeCurrentLinkRes`.
		// Can't search for descriptors matching the new AST, only the old one.
		searchTarget = maybeCurrentLinkRes
	} else {
		searchTarget = currentParseRes
	}
	tokenAtOffset := searchTarget.AST().TokenAtOffset(posOffset)
	completions := []protocol.CompletionItem{}

	// Option name completion after "option "
	indexOfOptionKeyword := strings.LastIndex(textPrecedingCursor, "option ") // note the space; don't match 'optional'
	if indexOfOptionKeyword != -1 {
		// if the cursor is before the next semicolon, then we're in the middle of an option name
		if !strings.Contains(textPrecedingCursor[indexOfOptionKeyword:], ";") {
			// check if we have a partial option name on the line
			partialName := strings.TrimSpace(textPrecedingCursor[indexOfOptionKeyword+len("option "):])
			// find the current scope, then complete option names that would be valid here
			if maybeCurrentLinkRes != nil {
				// if we have a previous link result, use its option index
				path, found := findNarrowestEnclosingScope(currentParseRes, tokenAtOffset, params.Position)
				if found {
					c, err := completeOptionNames(path, partialName, maybeCurrentLinkRes)
					if err != nil {
						return nil, err
					}
					completions = append(completions, c...)
				}
			}
		}
	}

	// thisPackage := searchTarget.FileDescriptorProto().GetPackage()

	enc := semanticItems{
		options: semanticItemsOptions{
			skipComments: false,
		},
		parseRes: searchTarget,
	}
	computeSemanticTokens(c, &enc, ast.WithIntersection(tokenAtOffset))

	path, found := findNarrowestEnclosingScope(searchTarget, tokenAtOffset, params.Position)
	if found && maybeCurrentLinkRes != nil {
		desc, _, err := deepPathSearch(path, maybeCurrentLinkRes)
		if err != nil {
			return nil, err
		}
		switch desc := desc.(type) {
		case protoreflect.MessageDescriptor:
			// complete field names
			// filter out fields that are already present
			existingFieldNames := []string{}
			switch node := path[len(path)-1].(type) {
			case *ast.MessageLiteralNode:
				for _, elem := range node.Elements {
					name := string(elem.Name.Name.AsIdentifier())
					if fd := desc.Fields().ByName(protoreflect.Name(name)); fd != nil {
						existingFieldNames = append(existingFieldNames, name)
					}
				}
				for i, l := 0, desc.Fields().Len(); i < l; i++ {
					fld := desc.Fields().Get(i)
					if slices.Contains(existingFieldNames, string(fld.Name())) {
						continue
					}
					insertPos := protocol.Range{
						Start: params.Position,
						End:   params.Position,
					}
					completions = append(completions, fieldCompletion(fld, insertPos))
				}
			}
		}
	}

	return &protocol.CompletionList{
		Items: completions,
	}, nil
}

func editAddImport(parseRes parser.Result, path string) protocol.TextEdit {
	insertionPoint := parseRes.ImportInsertionPoint()
	text := fmt.Sprintf("import \"%s\";\n", path)
	return protocol.TextEdit{
		Range: protocol.Range{
			Start: protocol.Position{
				Line:      uint32(insertionPoint.Line - 1),
				Character: uint32(insertionPoint.Col - 1),
			},
			End: protocol.Position{
				Line:      uint32(insertionPoint.Line - 1),
				Character: uint32(insertionPoint.Col - 1),
			},
		},
		NewText: text,
	}
}

func completeWithinToken(posOffset int, mapper *protocol.Mapper, item semanticItem) (string, *protocol.Range, error) {
	startOffset, err := mapper.PositionOffset(protocol.Position{
		Line:      item.line,
		Character: item.start,
	})
	if err != nil {
		return "", nil, err
	}
	return string(mapper.Content[startOffset:posOffset]), &protocol.Range{
		Start: protocol.Position{
			Line:      item.line,
			Character: item.start,
		},
		End: protocol.Position{
			Line:      item.line,
			Character: item.start + item.len,
		},
	}, nil
}

func fieldCompletion(fld protoreflect.FieldDescriptor, rng protocol.Range) protocol.CompletionItem {
	name := string(fld.Name())
	var docs string
	if src := fld.ParentFile().SourceLocations().ByDescriptor(fld); len(src.Path) > 0 {
		docs = src.LeadingComments
	}

	compl := protocol.CompletionItem{
		Label:  name,
		Kind:   protocol.FieldCompletion,
		Detail: fieldTypeDetail(fld),
		Documentation: &protocol.Or_CompletionItem_documentation{
			Value: protocol.MarkupContent{
				Kind:  protocol.Markdown,
				Value: docs,
			},
		},
		Deprecated: fld.Options().(*descriptorpb.FieldOptions).GetDeprecated(),
	}
	switch fld.Cardinality() {
	case protoreflect.Repeated:
		compl.Detail = fmt.Sprintf("repeated %s", compl.Detail)
		compl.TextEdit = &protocol.TextEdit{
			Range:   rng,
			NewText: fmt.Sprintf("%s: [\n  ${0}\n]", name),
		}
		textFmt := protocol.SnippetTextFormat
		compl.InsertTextFormat = &textFmt
		insMode := protocol.AdjustIndentation
		compl.InsertTextMode = &insMode
	default:
		switch fld.Kind() {
		case protoreflect.MessageKind:
			msg := fld.Message()
			if !msg.IsMapEntry() {
				compl.TextEdit = &protocol.TextEdit{
					Range:   rng,
					NewText: fmt.Sprintf("%s: {\n  ${0}\n}", name),
				}
				textFmt := protocol.SnippetTextFormat
				compl.InsertTextFormat = &textFmt
				insMode := protocol.AdjustIndentation
				compl.InsertTextMode = &insMode
			}
		default:
			compl.TextEdit = &protocol.TextEdit{
				Range:   rng,
				NewText: fmt.Sprintf("%s: ", name),
			}
		}
	}
	return compl
}

func fieldTypeDetail(fld protoreflect.FieldDescriptor) string {
	switch fld.Kind() {
	case protoreflect.MessageKind:
		if fld.Message().IsMapEntry() {
			return fmt.Sprintf("map<%s, %s>", fld.MapKey().FullName(), fld.MapValue().FullName())
		}
		if fld.IsExtension() {
			fqn := fld.FullName()
			xn := fqn.Name()
			return string(fqn.Parent().Append(protoreflect.Name(fmt.Sprintf("(%s)", xn))))
		}
		return string(fld.Message().FullName())
	default:
		return fld.Kind().String()
	}
}

var fieldDescType = reflect.TypeOf((*protoreflect.FieldDescriptor)(nil)).Elem()
var adjustIndentationMode = protocol.AdjustIndentation
var snippetMode = protocol.SnippetTextFormat

func completeOptionNames(path []ast.Node, maybePartialName string, linkRes linker.Result) ([]protocol.CompletionItem, error) {
	switch path[len(path)-1].(type) {
	case *ast.MessageNode:
		return completeMessageOptionNames(path, maybePartialName, linkRes)
	case *ast.FieldNode:
		// return completeFieldOptionNames(path, maybePartialName, linkRes)
	}
	return nil, nil
}

func completeMessageOptionNames(path []ast.Node, maybePartialName string, linkRes linker.Result) ([]protocol.CompletionItem, error) {
	parts := strings.Split(maybePartialName, ".")
	if len(parts) == 1 && !strings.HasSuffix(maybePartialName, ")") {
		wantExtension := strings.HasPrefix(maybePartialName, "(")
		if wantExtension {
			maybePartialName = strings.TrimPrefix(maybePartialName, "(")
		}
		// search for message options
		candidates, err := linkRes.FindDescriptorsByPrefix(context.TODO(), maybePartialName, func(d protoreflect.Descriptor) bool {
			if fd, ok := d.(protoreflect.FieldDescriptor); ok {
				if wantExtension {
					return fd.IsExtension() && fd.ContainingMessage().FullName() == "google.protobuf.MessageOptions"
				} else {
					return !fd.IsExtension() && fd.ContainingMessage().FullName() == "google.protobuf.MessageOptions"
				}
			}
			return false
		})
		if err != nil {
			return nil, err
		}
		items := []protocol.CompletionItem{}
		for _, candidate := range candidates {
			fd := candidate.(protoreflect.FieldDescriptor)
			switch fd.Kind() {
			case protoreflect.MessageKind:
				if fd.IsExtension() && wantExtension {
					items = append(items, newExtensionFieldCompletionItem(fd, false))
				} else if !fd.IsExtension() && !wantExtension {
					items = append(items, newMessageFieldCompletionItem(fd))
				}
			default:
				if fd.IsExtension() && wantExtension {
					items = append(items, newExtensionNonMessageFieldCompletionItem(fd, false))
				} else if !fd.IsExtension() && !wantExtension {
					items = append(items, newNonMessageFieldCompletionItem(fd))
				}
			}
		}
		return items, nil
	} else if len(parts) > 1 {
		// walk the options path
		var currentContext protoreflect.MessageDescriptor
		for _, part := range parts[:len(parts)-1] {
			isExtension := strings.HasPrefix(part, "(") && strings.HasSuffix(part, ")")
			if isExtension {
				if currentContext == nil {
					targetName := strings.Trim(part, "()")
					fqn := linkRes.Package()
					if !strings.Contains(targetName, ".") {
						fqn = fqn.Append(protoreflect.Name(targetName))
					} else {
						fqn = protoreflect.FullName(targetName)
					}
					xt, err := linker.ResolverFromFile(linkRes).FindExtensionByName(fqn)
					if err != nil {
						return nil, err
					}
					currentContext = xt.TypeDescriptor().Message()
				} else {
					xt := currentContext.Extensions().ByName(protoreflect.Name(strings.Trim(part, "()")))
					if xt == nil {
						return nil, fmt.Errorf("no such extension %s", part)
					}
					currentContext = xt.Message()
				}
			} else if currentContext != nil {
				fd := currentContext.Fields().ByName(protoreflect.Name(part))
				if fd == nil {
					return nil, fmt.Errorf("no such field %s", part)
				}
				if fd.IsExtension() {
					currentContext = fd.ContainingMessage()
				} else {
					currentContext = fd.Message()
				}
			}
			if currentContext == nil {
				return nil, fmt.Errorf("no such extension %s", part)
			}
		}
		// now we have the context, filter by the last part
		lastPart := parts[len(parts)-1]
		isExtension := strings.HasPrefix(lastPart, "(")
		items := []protocol.CompletionItem{}
		if isExtension {
			exts := currentContext.Extensions()
			for i, l := 0, exts.Len(); i < l; i++ {
				ext := exts.Get(i)
				if !strings.Contains(string(ext.Name()), lastPart) {
					continue
				}
				switch ext.Kind() {
				case protoreflect.MessageKind:
					if ext.Message().IsMapEntry() {
						// if the field is actually a map, the completion should insert map syntax
						items = append(items, newMapFieldCompletionItem(ext))
					} else {
						items = append(items, newExtensionFieldCompletionItem(ext, true))
					}
				default:
					if strings.Contains(string(ext.Name()), lastPart) {
						items = append(items, newNonMessageFieldCompletionItem(ext))
					}
				}
			}
		} else {
			// match field names
			fields := currentContext.Fields()
			for i, l := 0, fields.Len(); i < l; i++ {
				fld := fields.Get(i)
				if !strings.Contains(string(fld.Name()), lastPart) {
					continue
				}
				switch fld.Kind() {
				case protoreflect.MessageKind:
					if fld.Message().IsMapEntry() {
						// if the field is actually a map, the completion should insert map syntax
						items = append(items, newMapFieldCompletionItem(fld))
					} else if fld.IsExtension() {
						items = append(items, newExtensionFieldCompletionItem(fld, false))
					} else {
						items = append(items, newMessageFieldCompletionItem(fld))
					}
				default:
					if strings.Contains(string(fld.Name()), lastPart) {
						items = append(items, newNonMessageFieldCompletionItem(fld))
					}
				}
			}
		}
		return items, nil
	}
	return nil, nil
}

func newMapFieldCompletionItem(fld protoreflect.FieldDescriptor) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.StructCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf("%s = {key: ${1:%s}, value: ${2:%s}};", fld.Name(), fieldTypeDetail(fld.MapKey()), fieldTypeDetail(fld.MapValue())),
	}
}

func newMessageFieldCompletionItem(fld protoreflect.FieldDescriptor) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.StructCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertText:       string(fld.Name()),
		CommitCharacters: []string{"."},
	}
}

func newExtensionFieldCompletionItem(fld protoreflect.FieldDescriptor, needsLeadingOpenParen bool) protocol.CompletionItem {
	var fmtStr string
	if needsLeadingOpenParen {
		fmtStr = "(%s)"
	} else {
		fmtStr = "%s)"
	}
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.ModuleCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertText:       fmt.Sprintf(fmtStr, fld.Name()),
		CommitCharacters: []string{"."},
	}
}

func newNonMessageFieldCompletionItem(fld protoreflect.FieldDescriptor) protocol.CompletionItem {
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.ValueCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf("%s = ${0};", fld.Name()),
	}
}

func newExtensionNonMessageFieldCompletionItem(fld protoreflect.FieldDescriptor, needsLeadingOpenParen bool) protocol.CompletionItem {
	var fmtStr string
	if needsLeadingOpenParen {
		fmtStr = "(%s) = ${0};"
	} else {
		fmtStr = "%s) = ${0};"
	}
	return protocol.CompletionItem{
		Label:            string(fld.Name()),
		Kind:             protocol.ValueCompletion,
		Detail:           fieldTypeDetail(fld),
		InsertTextFormat: &snippetMode,
		InsertText:       fmt.Sprintf(fmtStr, fld.Name()),
	}
}
