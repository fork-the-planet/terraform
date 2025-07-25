// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package genconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/tfdiags"
)

type Resource struct {
	// HCL Body of the resource, which is the attributes and blocks
	// that are part of the resource.
	Body []byte

	// Import is the HCL code for the import block. This is only
	// generated for list resource results.
	Import  []byte
	Addr    addrs.AbsResourceInstance
	Results []*Resource
}

func (r *Resource) String() string {
	var buf strings.Builder
	switch r.Addr.Resource.Resource.Mode {
	case addrs.ListResourceMode:
		last := len(r.Results) - 1
		// sort the results by their keys so the output is consistent
		for idx, managed := range r.Results {
			if managed.Body != nil {
				buf.WriteString(managed.String())
				buf.WriteString("\n")
			}
			if managed.Import != nil {
				buf.WriteString(string(managed.Import))
				buf.WriteString("\n")
			}
			if idx != last {
				buf.WriteString("\n")
			}
		}
	case addrs.ManagedResourceMode:
		buf.WriteString(fmt.Sprintf("resource %q %q {\n", r.Addr.Resource.Resource.Type, r.Addr.Resource.Resource.Name))
		buf.Write(r.Body)
		buf.WriteString("}")
	default:
		panic(fmt.Errorf("unsupported resource mode %s", r.Addr.Resource.Resource.Mode))
	}

	// The output better be valid HCL which can be parsed and formatted.
	formatted := hclwrite.Format([]byte(buf.String()))
	return string(formatted)
}

// GenerateResourceContents generates HCL configuration code for the provided
// resource and state value.
//
// If you want to generate actual valid Terraform code you should follow this
// call up with a call to WrapResourceContents, which will place a Terraform
// resource header around the attributes and blocks returned by this function.
func GenerateResourceContents(addr addrs.AbsResourceInstance,
	schema *configschema.Block,
	pc addrs.LocalProviderConfig,
	stateVal cty.Value) (*Resource, tfdiags.Diagnostics) {
	var buf strings.Builder

	var diags tfdiags.Diagnostics

	if pc.LocalName != addr.Resource.Resource.ImpliedProvider() || pc.Alias != "" {
		buf.WriteString(strings.Repeat(" ", 2))
		buf.WriteString(fmt.Sprintf("provider = %s\n", pc.StringCompact()))
	}

	if stateVal.RawEquals(cty.NilVal) {
		diags = diags.Append(writeConfigAttributes(addr, &buf, schema.Attributes, 2))
		diags = diags.Append(writeConfigBlocks(addr, &buf, schema.BlockTypes, 2))
	} else {
		diags = diags.Append(writeConfigAttributesFromExisting(addr, &buf, stateVal, schema.Attributes, 2, optionalOrRequiredProcessor))
		diags = diags.Append(writeConfigBlocksFromExisting(addr, &buf, stateVal, schema.BlockTypes, 2))
	}

	// The output better be valid HCL which can be parsed and formatted.
	formatted := hclwrite.Format([]byte(buf.String()))
	return &Resource{
		Body: formatted,
		Addr: addr,
	}, diags
}

func GenerateListResourceContents(addr addrs.AbsResourceInstance,
	schema *configschema.Block,
	idSchema *configschema.Object,
	pc addrs.LocalProviderConfig,
	stateVal cty.Value,
) (*Resource, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	if !stateVal.CanIterateElements() {
		diags = diags.Append(
			hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Invalid resource instance value",
				Detail:   fmt.Sprintf("Resource instance %s has nil or non-iterable value", addr),
			})
		return nil, diags
	}

	ret := make([]*Resource, stateVal.LengthInt())
	iter := stateVal.ElementIterator()
	for idx := 0; iter.Next(); idx++ {
		// Generate a unique resource name for each instance in the list.
		resAddr := addrs.AbsResourceInstance{
			Module: addr.Module,
			Resource: addrs.ResourceInstance{
				Resource: addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: addr.Resource.Resource.Type,
					Name: fmt.Sprintf("%s_%d", addr.Resource.Resource.Name, idx),
				},
				Key: addr.Resource.Key,
			},
		}
		ls := &Resource{Addr: resAddr}
		ret[idx] = ls

		_, val := iter.Element()
		// we still need to generate the resource block even if the state is not given,
		// so that the import block can reference it.
		stateVal := cty.NilVal
		if val.Type().HasAttribute("state") {
			stateVal = val.GetAttr("state")
		}
		content, gDiags := GenerateResourceContents(resAddr, schema, pc, stateVal)
		if gDiags.HasErrors() {
			diags = diags.Append(gDiags)
			continue
		}
		ls.Body = content.Body

		idVal := val.GetAttr("identity")
		importContent, gDiags := generateImportBlock(resAddr, idSchema, pc, idVal)
		if gDiags.HasErrors() {
			diags = diags.Append(gDiags)
			continue
		}
		ls.Import = bytes.TrimSpace(hclwrite.Format([]byte(importContent)))
	}

	return &Resource{
		Results: ret,
		Addr:    addr,
	}, diags
}

func generateImportBlock(addr addrs.AbsResourceInstance, idSchema *configschema.Object, pc addrs.LocalProviderConfig, identity cty.Value) (string, tfdiags.Diagnostics) {
	var buf strings.Builder
	var diags tfdiags.Diagnostics

	buf.WriteString("\n")
	buf.WriteString("import {\n")
	buf.WriteString(fmt.Sprintf("  to = %s\n", addr.String()))
	buf.WriteString(fmt.Sprintf("  provider = %s\n", pc.StringCompact()))
	buf.WriteString("  identity = {\n")
	diags = diags.Append(writeConfigAttributesFromExisting(addr, &buf, identity, idSchema.Attributes, 2, allowAllAttributesProcessor))
	buf.WriteString(strings.Repeat(" ", 2))
	buf.WriteString("}\n}\n")

	formatted := hclwrite.Format([]byte(buf.String()))
	return string(formatted), diags
}

func writeConfigAttributes(addr addrs.AbsResourceInstance, buf *strings.Builder, attrs map[string]*configschema.Attribute, indent int) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if len(attrs) == 0 {
		return diags
	}

	// Get a list of sorted attribute names so the output will be consistent between runs.
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		attrS := attrs[name]
		if attrS.NestedType != nil {
			diags = diags.Append(writeConfigNestedTypeAttribute(addr, buf, name, attrS, indent))
			continue
		}
		if attrS.Required {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = ", name))
			tok := hclwrite.TokensForValue(attrS.EmptyValue())
			if _, err := tok.WriteTo(buf); err != nil {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagWarning,
					Summary:  "Skipped part of config generation",
					Detail:   fmt.Sprintf("Could not create attribute %s in %s when generating import configuration. The plan will likely report the missing attribute as being deleted.", name, addr),
					Extra:    err,
				})
				continue
			}
			writeAttrTypeConstraint(buf, attrS)
		} else if attrS.Optional {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = ", name))
			tok := hclwrite.TokensForValue(attrS.EmptyValue())
			if _, err := tok.WriteTo(buf); err != nil {
				diags = diags.Append(&hcl.Diagnostic{
					Severity: hcl.DiagWarning,
					Summary:  "Skipped part of config generation",
					Detail:   fmt.Sprintf("Could not create attribute %s in %s when generating import configuration. The plan will likely report the missing attribute as being deleted.", name, addr),
					Extra:    err,
				})
				continue
			}
			writeAttrTypeConstraint(buf, attrS)
		}
	}
	return diags
}

func optionalOrRequiredProcessor(attr *configschema.Attribute) bool {
	// Exclude computed-only attributes
	return attr.Optional || attr.Required
}

func allowAllAttributesProcessor(attr *configschema.Attribute) bool {
	return true
}

func writeConfigAttributesFromExisting(addr addrs.AbsResourceInstance, buf *strings.Builder, stateVal cty.Value, attrs map[string]*configschema.Attribute, indent int, processAttr func(*configschema.Attribute) bool) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	if len(attrs) == 0 {
		return diags
	}

	// Sort attribute names so the output will be consistent between runs.
	for _, name := range slices.Sorted(maps.Keys(attrs)) {
		attrS := attrs[name]
		if attrS.NestedType != nil {
			writeConfigNestedTypeAttributeFromExisting(addr, buf, name, attrS, stateVal, indent)
			continue
		}

		if processAttr != nil && processAttr(attrS) {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = ", name))

			var val cty.Value
			if !stateVal.IsNull() && stateVal.Type().HasAttribute(name) {
				val = stateVal.GetAttr(name)
			} else {
				val = attrS.EmptyValue()
			}
			if val.Type() == cty.String {
				// Before we inspect the string, take off any marks.
				unmarked, marks := val.Unmark()

				// SHAMELESS HACK: If we have "" for an optional value, assume
				// it is actually null, due to the legacy SDK.
				if !unmarked.IsNull() && attrS.Optional && len(unmarked.AsString()) == 0 {
					unmarked = attrS.EmptyValue()
				}

				// Before we carry on, add the marks back.
				val = unmarked.WithMarks(marks)
			}
			if attrS.Sensitive || val.IsMarked() {
				buf.WriteString("null # sensitive")
			} else {
				// If the value is a string storing a JSON value we want to represent it in a terraform native way
				// and encapsulate it in `jsonencode` as it is the idiomatic representation
				if val.IsKnown() && !val.IsNull() && val.Type() == cty.String && json.Valid([]byte(val.AsString())) {
					var ctyValue ctyjson.SimpleJSONValue
					err := ctyValue.UnmarshalJSON([]byte(val.AsString()))
					if err != nil {
						diags = diags.Append(&hcl.Diagnostic{
							Severity: hcl.DiagWarning,
							Summary:  "Failed to parse JSON",
							Detail:   fmt.Sprintf("Could not parse JSON value of attribute %s in %s when generating import configuration. The plan will likely report the missing attribute as being deleted. This is most likely a bug in Terraform, please report it.", name, addr),
							Extra:    err,
						})
						continue
					}

					// Lone deserializable primitive types are valid json, but should be treated as strings
					if ctyValue.Type().IsPrimitiveType() {
						if d := writeTokens(val, buf); d != nil {
							diags = diags.Append(d)
							continue
						}
					} else {
						buf.WriteString("jsonencode(")

						if d := writeTokens(ctyValue.Value, buf); d != nil {
							diags = diags.Append(d)
							continue
						}

						buf.WriteString(")")
					}
				} else {
					if d := writeTokens(val, buf); d != nil {
						diags = diags.Append(d)
						continue
					}
				}
			}

			buf.WriteString("\n")
		}
	}
	return diags
}

func writeTokens(val cty.Value, buf *strings.Builder) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	tok := hclwrite.TokensForValue(val)
	if _, err := tok.WriteTo(buf); err != nil {
		return diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagWarning,
			Summary:  "Skipped part of config generation",
			Detail:   "Could not create attribute in import configuration. The plan will likely report the missing attribute as being deleted.",
			Extra:    err,
		})
	}
	return diags
}

func writeConfigBlocks(addr addrs.AbsResourceInstance, buf *strings.Builder, blocks map[string]*configschema.NestedBlock, indent int) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if len(blocks) == 0 {
		return diags
	}

	// Get a list of sorted block names so the output will be consistent between runs.
	for _, name := range slices.Sorted(maps.Keys(blocks)) {
		blockS := blocks[name]
		diags = diags.Append(writeConfigNestedBlock(addr, buf, name, blockS, indent))
	}
	return diags
}

func writeConfigNestedBlock(addr addrs.AbsResourceInstance, buf *strings.Builder, name string, schema *configschema.NestedBlock, indent int) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	switch schema.Nesting {
	case configschema.NestingSingle, configschema.NestingGroup:
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(fmt.Sprintf("%s {", name))
		writeBlockTypeConstraint(buf, schema)
		diags = diags.Append(writeConfigAttributes(addr, buf, schema.Attributes, indent+2))
		diags = diags.Append(writeConfigBlocks(addr, buf, schema.BlockTypes, indent+2))
		buf.WriteString("}\n")
		return diags
	case configschema.NestingList, configschema.NestingSet:
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(fmt.Sprintf("%s {", name))
		writeBlockTypeConstraint(buf, schema)
		diags = diags.Append(writeConfigAttributes(addr, buf, schema.Attributes, indent+2))
		diags = diags.Append(writeConfigBlocks(addr, buf, schema.BlockTypes, indent+2))
		buf.WriteString("}\n")
		return diags
	case configschema.NestingMap:
		buf.WriteString(strings.Repeat(" ", indent))
		// we use an arbitrary placeholder key (block label) "key"
		buf.WriteString(fmt.Sprintf("%s \"key\" {", name))
		writeBlockTypeConstraint(buf, schema)
		diags = diags.Append(writeConfigAttributes(addr, buf, schema.Attributes, indent+2))
		diags = diags.Append(writeConfigBlocks(addr, buf, schema.BlockTypes, indent+2))
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString("}\n")
		return diags
	default:
		// This should not happen, the above should be exhaustive.
		panic(fmt.Errorf("unsupported NestingMode %s", schema.Nesting.String()))
	}
}

func writeConfigNestedTypeAttribute(addr addrs.AbsResourceInstance, buf *strings.Builder, name string, schema *configschema.Attribute, indent int) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	buf.WriteString(strings.Repeat(" ", indent))
	buf.WriteString(fmt.Sprintf("%s = ", name))

	switch schema.NestedType.Nesting {
	case configschema.NestingSingle:
		buf.WriteString("{")
		writeAttrTypeConstraint(buf, schema)
		diags = diags.Append(writeConfigAttributes(addr, buf, schema.NestedType.Attributes, indent+2))
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString("}\n")
		return diags
	case configschema.NestingList, configschema.NestingSet:
		buf.WriteString("[{")
		writeAttrTypeConstraint(buf, schema)
		diags = diags.Append(writeConfigAttributes(addr, buf, schema.NestedType.Attributes, indent+2))
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString("}]\n")
		return diags
	case configschema.NestingMap:
		buf.WriteString("{")
		writeAttrTypeConstraint(buf, schema)
		buf.WriteString(strings.Repeat(" ", indent+2))
		// we use an arbitrary placeholder key "key"
		buf.WriteString("key = {\n")
		diags = diags.Append(writeConfigAttributes(addr, buf, schema.NestedType.Attributes, indent+4))
		buf.WriteString(strings.Repeat(" ", indent+2))
		buf.WriteString("}\n")
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString("}\n")
		return diags
	default:
		// This should not happen, the above should be exhaustive.
		panic(fmt.Errorf("unsupported NestingMode %s", schema.NestedType.Nesting.String()))
	}
}

func writeConfigBlocksFromExisting(addr addrs.AbsResourceInstance, buf *strings.Builder, stateVal cty.Value, blocks map[string]*configschema.NestedBlock, indent int) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics

	if len(blocks) == 0 {
		return diags
	}

	// Sort block names so the output will be consistent between runs.
	for _, name := range slices.Sorted(maps.Keys(blocks)) {
		blockS := blocks[name]
		// This shouldn't happen in real usage; state always has all values (set
		// to null as needed), but it protects against panics in tests (and any
		// really weird and unlikely cases).
		if !stateVal.Type().HasAttribute(name) {
			continue
		}
		blockVal := stateVal.GetAttr(name)
		diags = diags.Append(writeConfigNestedBlockFromExisting(addr, buf, name, blockS, blockVal, indent))
	}

	return diags
}

func writeConfigNestedTypeAttributeFromExisting(addr addrs.AbsResourceInstance, buf *strings.Builder, name string, schema *configschema.Attribute, stateVal cty.Value, indent int) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	processor := optionalOrRequiredProcessor

	switch schema.NestedType.Nesting {
	case configschema.NestingSingle:
		if schema.Sensitive || stateVal.IsMarked() {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = {} # sensitive\n", name))
			return diags
		}

		// This shouldn't happen in real usage; state always has all values (set
		// to null as needed), but it protects against panics in tests (and any
		// really weird and unlikely cases).
		if !stateVal.Type().HasAttribute(name) {
			return diags
		}
		nestedVal := stateVal.GetAttr(name)

		if nestedVal.IsNull() {
			// There is a difference between a null object, and an object with
			// no attributes.
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = null\n", name))
			return diags
		}

		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(fmt.Sprintf("%s = {\n", name))
		diags = diags.Append(writeConfigAttributesFromExisting(addr, buf, nestedVal, schema.NestedType.Attributes, indent+2, processor))
		buf.WriteString("}\n")
		return diags

	case configschema.NestingList, configschema.NestingSet:

		if schema.Sensitive || stateVal.IsMarked() {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = [] # sensitive\n", name))
			return diags
		}

		listVals := ctyCollectionValues(stateVal.GetAttr(name))
		if listVals == nil {
			// There is a difference between an empty list and a null list
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = null\n", name))
			return diags
		}

		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(fmt.Sprintf("%s = [\n", name))
		for i := range listVals {
			buf.WriteString(strings.Repeat(" ", indent+2))

			// The entire element is marked.
			if listVals[i].IsMarked() {
				buf.WriteString("{}, # sensitive\n")
				continue
			}

			buf.WriteString("{\n")
			diags = diags.Append(writeConfigAttributesFromExisting(addr, buf, listVals[i], schema.NestedType.Attributes, indent+4, processor))
			buf.WriteString(strings.Repeat(" ", indent+2))
			buf.WriteString("},\n")
		}
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString("]\n")
		return diags

	case configschema.NestingMap:
		if schema.Sensitive || stateVal.IsMarked() {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = {} # sensitive\n", name))
			return diags
		}

		attr := stateVal.GetAttr(name)
		if attr.IsNull() {
			// There is a difference between an empty map and a null map.
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s = null\n", name))
			return diags
		}

		vals := attr.AsValueMap()

		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(fmt.Sprintf("%s = {\n", name))
		for _, key := range slices.Sorted(maps.Keys(vals)) {
			buf.WriteString(strings.Repeat(" ", indent+2))
			buf.WriteString(fmt.Sprintf("%s = {", hclEscapeString(key)))

			// This entire value is marked
			if vals[key].IsMarked() {
				buf.WriteString("} # sensitive\n")
				continue
			}

			buf.WriteString("\n")
			diags = diags.Append(writeConfigAttributesFromExisting(addr, buf, vals[key], schema.NestedType.Attributes, indent+4, processor))
			buf.WriteString(strings.Repeat(" ", indent+2))
			buf.WriteString("}\n")
		}
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString("}\n")
		return diags

	default:
		// This should not happen, the above should be exhaustive.
		panic(fmt.Errorf("unsupported NestingMode %s", schema.NestedType.Nesting.String()))
	}
}

func writeConfigNestedBlockFromExisting(addr addrs.AbsResourceInstance, buf *strings.Builder, name string, schema *configschema.NestedBlock, stateVal cty.Value, indent int) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	processAttr := optionalOrRequiredProcessor

	switch schema.Nesting {
	case configschema.NestingSingle, configschema.NestingGroup:
		if stateVal.IsNull() {
			return diags
		}
		buf.WriteString(strings.Repeat(" ", indent))
		buf.WriteString(fmt.Sprintf("%s {", name))

		// If the entire value is marked, don't print any nested attributes
		if stateVal.IsMarked() {
			buf.WriteString("} # sensitive\n")
			return diags
		}
		buf.WriteString("\n")
		diags = diags.Append(writeConfigAttributesFromExisting(addr, buf, stateVal, schema.Attributes, indent+2, processAttr))
		diags = diags.Append(writeConfigBlocksFromExisting(addr, buf, stateVal, schema.BlockTypes, indent+2))
		buf.WriteString("}\n")
		return diags
	case configschema.NestingList, configschema.NestingSet:
		if stateVal.IsMarked() {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s {} # sensitive\n", name))
			return diags
		}
		listVals := ctyCollectionValues(stateVal)
		for i := range listVals {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s {\n", name))
			diags = diags.Append(writeConfigAttributesFromExisting(addr, buf, listVals[i], schema.Attributes, indent+2, processAttr))
			diags = diags.Append(writeConfigBlocksFromExisting(addr, buf, listVals[i], schema.BlockTypes, indent+2))
			buf.WriteString("}\n")
		}
		return diags
	case configschema.NestingMap:
		// If the entire value is marked, don't print any nested attributes
		if stateVal.IsMarked() {
			buf.WriteString(fmt.Sprintf("%s {} # sensitive\n", name))
			return diags
		}

		vals := stateVal.AsValueMap()
		for _, key := range slices.Sorted(maps.Keys(vals)) {
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString(fmt.Sprintf("%s %q {", name, key))
			// This entire map element is marked
			if vals[key].IsMarked() {
				buf.WriteString("} # sensitive\n")
				return diags
			}
			buf.WriteString("\n")
			diags = diags.Append(writeConfigAttributesFromExisting(addr, buf, vals[key], schema.Attributes, indent+2, processAttr))
			diags = diags.Append(writeConfigBlocksFromExisting(addr, buf, vals[key], schema.BlockTypes, indent+2))
			buf.WriteString(strings.Repeat(" ", indent))
			buf.WriteString("}\n")
		}
		return diags
	default:
		// This should not happen, the above should be exhaustive.
		panic(fmt.Errorf("unsupported NestingMode %s", schema.Nesting.String()))
	}
}

func writeAttrTypeConstraint(buf *strings.Builder, schema *configschema.Attribute) {
	if schema.Required {
		buf.WriteString(" # REQUIRED ")
	} else {
		buf.WriteString(" # OPTIONAL ")
	}

	if schema.NestedType != nil {
		buf.WriteString(fmt.Sprintf("%s\n", schema.NestedType.ImpliedType().FriendlyName()))
	} else {
		buf.WriteString(fmt.Sprintf("%s\n", schema.Type.FriendlyName()))
	}
}

func writeBlockTypeConstraint(buf *strings.Builder, schema *configschema.NestedBlock) {
	if schema.MinItems > 0 {
		buf.WriteString(" # REQUIRED block\n")
	} else {
		buf.WriteString(" # OPTIONAL block\n")
	}
}

// copied from command/format/diff
func ctyCollectionValues(val cty.Value) []cty.Value {
	if !val.IsKnown() || val.IsNull() {
		return nil
	}

	var len int
	if val.IsMarked() {
		val, _ = val.Unmark()
		len = val.LengthInt()
	} else {
		len = val.LengthInt()
	}

	ret := make([]cty.Value, 0, len)
	for it := val.ElementIterator(); it.Next(); {
		_, value := it.Element()
		ret = append(ret, value)
	}

	return ret
}

// hclEscapeString formats the input string into a format that is safe for
// rendering within HCL.
//
// Note, this function doesn't actually do a very good job of this currently. We
// need to expose some internal functions from HCL in a future version and call
// them from here. For now, just use "%q" formatting.
//
// Note, the similar function in jsonformat/computed/renderers/map.go is doing
// something similar.
func hclEscapeString(str string) string {
	// TODO: Replace this with more complete HCL logic instead of the simple
	// go workaround.
	if !hclsyntax.ValidIdentifier(str) {
		return fmt.Sprintf("%q", str)
	}
	return str
}
