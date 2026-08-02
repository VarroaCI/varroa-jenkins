package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// printDescribe prints a structured key: value description of v to w.
// Maps are sorted by key, indented recursively; scalar arrays are inlined;
// object arrays are shown as indented blocks.
func printDescribe(w io.Writer, v any) error {
	printValue(w, v, 0)
	return nil
}

func printValue(w io.Writer, v any, indent int) {
	prefix := strings.Repeat("  ", indent)

	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			switch inner := val[k].(type) {
			case map[string]any:
				fmt.Fprintf(w, "%s%s:\n", prefix, k)
				printValue(w, inner, indent+1)
			case []any:
				if len(inner) == 0 {
					fmt.Fprintf(w, "%s%s:\t[]\n", prefix, k)
					continue
				}
				// Check if it's an array of objects
				if _, ok := inner[0].(map[string]any); ok {
					fmt.Fprintf(w, "%s%s:\n", prefix, k)
					for _, item := range inner {
						printValue(w, item, indent+1)
						_, _ = fmt.Fprintln(w)
					}
				} else {
					// Scalar array — inline
					parts := make([]string, len(inner))
					for i, item := range inner {
						parts[i] = fmt.Sprintf("%v", item)
					}
					fmt.Fprintf(w, "%s%s:\t[%s]\n", prefix, k, strings.Join(parts, ", "))
				}
			default:
				fmt.Fprintf(w, "%s%s:\t%v\n", prefix, k, inner)
			}
		}
	default:
		fmt.Fprintf(w, "%s%v\n", prefix, val)
	}
}
