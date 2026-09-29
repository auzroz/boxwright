// Package bootstrap turns an indented outline of a storage hierarchy into
// Homebox locations.
//
// It exists because the product cannot cold-start itself: with no containers
// in Homebox, every recommendation is trivially "make a new box", and the
// placement engine has nothing to score. Creating a few dozen containers by hand in
// a web UI is the kind of chore that stops a project being used.
package bootstrap

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Node is one location in the outline, with the metadata to write on it.
type Node struct {
	Name     string
	Depth    int
	Children []*Node

	// Metadata, all optional. Absent means "not recorded", which the placement
	// engine treats as unknown rather than false -- so omitting a flag is a
	// meaningful choice, not laziness.
	CapacityUnits *int
	Access        string
	HeavySafe     *bool
	FragileSafe   *bool
}

// Leaf reports whether this node holds items rather than other locations.
// Only leaves get metadata: a row or a room is not somewhere you put a drill.
func (n *Node) Leaf() bool { return len(n.Children) == 0 }

// Parse reads an indented outline:
//
//	Garage
//	  California
//	    Fresno      access=easy heavySafe=true capacity=8
//	    Modesto
//
// Indentation is two spaces per level. Blank lines and # comments are ignored.
// Keys after the name set metadata on that location.
func Parse(r io.Reader) ([]*Node, error) {
	var roots []*Node
	// stack[d] is the most recent node seen at depth d.
	var stack []*Node

	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		raw := sc.Text()
		if i := strings.Index(raw, "#"); i >= 0 {
			raw = raw[:i]
		}
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if strings.Contains(raw, "\t") {
			return nil, fmt.Errorf("line %d: use spaces, not tabs, so depth is unambiguous", line)
		}

		indent := len(raw) - len(strings.TrimLeft(raw, " "))
		if indent%2 != 0 {
			return nil, fmt.Errorf("line %d: indent %d is not a multiple of 2", line, indent)
		}
		depth := indent / 2
		if depth > len(stack) {
			return nil, fmt.Errorf("line %d: %q jumps from depth %d to %d; indent one level at a time",
				line, strings.TrimSpace(raw), len(stack), depth)
		}

		node, err := parseNode(strings.TrimSpace(raw), depth)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}

		stack = stack[:depth]
		if depth == 0 {
			roots = append(roots, node)
		} else {
			parent := stack[depth-1]
			parent.Children = append(parent.Children, node)
		}
		stack = append(stack, node)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("outline is empty")
	}
	return roots, nil
}

// parseNode splits "Fresno access=easy heavySafe=true" into a name and metadata.
func parseNode(s string, depth int) (*Node, error) {
	fields := strings.Fields(s)
	n := &Node{Depth: depth}
	nameParts := []string{}
	for _, f := range fields {
		k, v, isKV := strings.Cut(f, "=")
		if !isKV {
			if len(n.Access) > 0 || n.CapacityUnits != nil || n.HeavySafe != nil || n.FragileSafe != nil {
				return nil, fmt.Errorf("%q: name words must come before key=value settings", f)
			}
			nameParts = append(nameParts, f)
			continue
		}
		switch k {
		case "access":
			switch v {
			case "easy", "normal", "deep":
				n.Access = v
			default:
				return nil, fmt.Errorf("access=%q must be easy, normal or deep", v)
			}
		case "capacity":
			i, err := strconv.Atoi(v)
			if err != nil || i <= 0 {
				return nil, fmt.Errorf("capacity=%q must be a positive whole number", v)
			}
			n.CapacityUnits = &i
		case "heavySafe", "fragileSafe":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("%s=%q must be true or false", k, v)
			}
			if k == "heavySafe" {
				n.HeavySafe = &b
			} else {
				n.FragileSafe = &b
			}
		default:
			return nil, fmt.Errorf("unknown setting %q (want access, capacity, heavySafe, fragileSafe)", k)
		}
	}
	if len(nameParts) == 0 {
		return nil, fmt.Errorf("missing a location name")
	}
	n.Name = strings.Join(nameParts, " ")
	return n, nil
}

// Walk visits every node depth-first, parents before children.
func Walk(nodes []*Node, fn func(n *Node, parent *Node)) {
	var rec func([]*Node, *Node)
	rec = func(ns []*Node, parent *Node) {
		for _, n := range ns {
			fn(n, parent)
			rec(n.Children, n)
		}
	}
	rec(nodes, nil)
}
