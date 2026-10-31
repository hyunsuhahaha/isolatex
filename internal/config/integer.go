package config

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Keep the public Go field types and yaml.v3's strict decoder, but reject
// coercion of integer settings. Durations retain their duration-string syntax.
// Resolve aliases and merges too, with a bounded recursion depth for cycles.
type integerNodeKey struct {
	node *yaml.Node
	typ  reflect.Type
}

func validateIntegerNodes(node *yaml.Node, typ reflect.Type, path string, depth int, seen map[integerNodeKey]bool) error {
	if depth > 100 {
		return fmt.Errorf("%s: YAML nesting exceeds 100", path)
	}
	// Aliased merge graphs can reuse one subtree many times. Visit each
	// (node, schema type) once so validation stays bounded by the input graph.
	key := integerNodeKey{node, typ}
	if seen[key] {
		return nil
	}
	seen[key] = true
	if node.Kind == yaml.DocumentNode {
		for _, child := range node.Content {
			if err := validateIntegerNodes(child, typ, path, depth+1, seen); err != nil {
				return err
			}
		}
		return nil
	}
	if node.Kind == yaml.AliasNode {
		return validateIntegerNodes(node.Alias, typ, path, depth+1, seen)
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(time.Duration(0)) {
		return nil
	}
	switch typ.Kind() {
	case reflect.Int, reflect.Int64:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return fmt.Errorf("%s at line %d must be a YAML integer", path, node.Line)
		}
	case reflect.Struct, reflect.Map:
		if node.Kind != yaml.MappingNode {
			return nil
		} // Main decoder reports shape errors.
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				merged := []*yaml.Node{value}
				if value.Kind == yaml.SequenceNode {
					merged = value.Content
				}
				for _, item := range merged {
					if err := validateIntegerNodes(item, typ, path, depth+1, seen); err != nil {
						return err
					}
				}
				continue
			}
			var childType reflect.Type
			if typ.Kind() == reflect.Map {
				childType = typ.Elem()
			} else {
				for j := 0; j < typ.NumField(); j++ {
					field := typ.Field(j)
					if strings.Split(field.Tag.Get("yaml"), ",")[0] == key.Value {
						childType = field.Type
						break
					}
				}
			}
			if childType != nil {
				if err := validateIntegerNodes(value, childType, path+"."+key.Value, depth+1, seen); err != nil {
					return err
				}
			}
		}
	case reflect.Slice:
		for _, child := range node.Content {
			if err := validateIntegerNodes(child, typ.Elem(), path, depth+1, seen); err != nil {
				return err
			}
		}
	}
	return nil
}
