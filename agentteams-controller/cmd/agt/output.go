package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
)

// printTable renders rows as an aligned text table (similar to kubectl get).
func printTable(headers []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(headers, "\t"))
	for _, row := range rows {
		fmt.Fprintln(w, strings.Join(row, "\t"))
	}
	w.Flush()
}

// KeyValue is a label-value pair for detail output.
type KeyValue struct {
	Key   string
	Value string
}

// printDetail renders a single resource in "Key: Value" format.
func printDetail(fields []KeyValue) {
	maxKey := 0
	for _, f := range fields {
		if len(f.Key) > maxKey {
			maxKey = len(f.Key)
		}
	}
	for _, f := range fields {
		if f.Value != "" {
			fmt.Printf("%-*s  %s\n", maxKey+1, f.Key+":", f.Value)
		}
	}
}

// printJSON outputs v as indented JSON.
func printJSON(v interface{}) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: marshal JSON: %v\n", err)
		return
	}
	fmt.Println(string(data))
}

// or returns fallback if s is empty.
func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
