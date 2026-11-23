// Package routing implements the label-matcher routing tree.
package routing

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"kilnalert/internal/model"
	"kilnalert/internal/timewin"
)

// Node is a compiled routing node.
type Node struct {
	Spec     *model.RouteNode
	Match    func(map[string]string) bool
	Children []*Node
	Parent   *Node
	Depth    int
}

// Tree is a compiled routing tree with a flat index.
type Tree struct {
	Root  *Node
	ByID  map[string]*Node
	Order []*Node // pre-order traversal, for stable iteration
}

// CompileMatchers compiles a matcher set into a predicate. Regular
// expressions are fully anchored.
func CompileMatchers(ms model.MatchAll) (func(map[string]string) bool, error) {
	type compiled struct {
		spec model.Matcher
		re   *regexp.Regexp
	}
	cs := make([]compiled, 0, len(ms))
	for _, m := range ms {
		c := compiled{spec: m}
		switch m.Op {
		case "=", "!=":
		case "=~", "!~":
			re, err := regexp.Compile("^(?:" + m.Value + ")$")
			if err != nil {
				return nil, fmt.Errorf("匹配条件 %s%s%q: %w", m.Name, m.Op, m.Value, err)
			}
			c.re = re
		default:
			return nil, fmt.Errorf("匹配条件 %s: 未知运算符 %q", m.Name, m.Op)
		}
		cs = append(cs, c)
	}
	return func(labels map[string]string) bool {
		for _, c := range cs {
			v, ok := labels[c.spec.Name]
			switch c.spec.Op {
			case "=":
				if !ok || v != c.spec.Value {
					return false
				}
			case "!=":
				if ok && v == c.spec.Value {
					return false
				}
			case "=~":
				if !ok || !c.re.MatchString(v) {
					return false
				}
			case "!~":
				if ok && c.re.MatchString(v) {
					return false
				}
			}
		}
		return true
	}, nil
}

// Matches evaluates a matcher set against labels.
func Matches(ms model.MatchAll, labels map[string]string) bool {
	f, err := CompileMatchers(ms)
	if err != nil {
		return false
	}
	return f(labels)
}

// Validate checks a route spec for the problems that would make routing
// ambiguous.
func Validate(root *model.RouteNode) error {
	if root == nil {
		return fmt.Errorf("路由树为空")
	}
	ids := map[string]bool{}
	var walk func(n *model.RouteNode, parent string) error
	walk = func(n *model.RouteNode, parent string) error {
		if n.ID == "" {
			return fmt.Errorf("路由节点 %q 缺少 id", n.Name)
		}
		if ids[n.ID] {
			return fmt.Errorf("路由节点 id 重复 %q", n.ID)
		}
		ids[n.ID] = true
		if n.Parent != "" && n.Parent != parent {
			return fmt.Errorf("节点 %q 声明父节点 %q，实际在 %q 下", n.ID, n.Parent, parent)
		}
		if _, err := CompileMatchers(n.Match); err != nil {
			return fmt.Errorf("节点 %q: %w", n.ID, err)
		}
		for _, r := range n.InhibitRules {
			if _, err := CompileMatchers(r.Source); err != nil {
				return fmt.Errorf("抑制规则 %q 源条件: %w", r.ID, err)
			}
			if _, err := CompileMatchers(r.Target); err != nil {
				return fmt.Errorf("抑制规则 %q 目标条件: %w", r.ID, err)
			}
		}
		for _, s := range n.Silences {
			if _, err := CompileMatchers(s.Match); err != nil {
				return fmt.Errorf("静默 %q: %w", s.ID, err)
			}
			if !s.Ends.After(s.Starts) {
				return fmt.Errorf("静默 %q 结束早于开始", s.ID)
			}
		}
		seen := map[string]bool{}
		for _, c := range n.Children {
			if seen[c.ID] {
				return fmt.Errorf("节点 %q 重复列出子节点 %q", n.ID, c.ID)
			}
			seen[c.ID] = true
			if err := walk(c, n.ID); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, "")
}

// Compile validates and compiles a routing tree.
func Compile(root *model.RouteNode) (*Tree, error) {
	if err := Validate(root); err != nil {
		return nil, err
	}
	t := &Tree{ByID: map[string]*Node{}}
	var build func(spec *model.RouteNode, parent *Node) (*Node, error)
	build = func(spec *model.RouteNode, parent *Node) (*Node, error) {
		m, err := CompileMatchers(spec.Match)
		if err != nil {
			return nil, err
		}
		n := &Node{Spec: spec, Match: m, Parent: parent}
		if parent != nil {
			n.Depth = parent.Depth + 1
		}
		t.ByID[spec.ID] = n
		t.Order = append(t.Order, n)
		for _, cs := range spec.Children {
			c, err := build(cs, n)
			if err != nil {
				return nil, err
			}
			n.Children = append(n.Children, c)
		}
		return n, nil
	}
	r, err := build(root, nil)
	if err != nil {
		return nil, err
	}
	t.Root = r
	return t, nil
}

// Match is one node an alert is routed to.
type Match struct {
	Node   *Node
	Key    model.GroupKey
	Values []string
}

// Route returns the terminal nodes an alert is routed to at time at.
//
// Matching is top-down and deepest-match wins, as in Alertmanager: a node that
// matches and has a matching descendant delivers the alert to that descendant
// rather than to itself. A node with ContinueSiblings also lets the walk try
// the following siblings (and their subtrees). A node whose time windows
// exclude at is skipped along with its subtree, but its siblings are tried.
func (t *Tree) Route(labels map[string]string, at time.Time) []Match {
	var out []Match
	var visit func(parent *Node)
	visit = func(parent *Node) {
		for _, child := range parent.Children {
			if !timewin.Active(child.Spec.TimeWindows, at) {
				continue
			}
			if !child.Match(labels) {
				continue
			}
			before := len(out)
			visit(child)
			if len(out) == before {
				out = append(out, Match{Node: child, Key: MakeKey(child.Spec, labels), Values: groupValues(child.Spec, labels)})
			}
			if !child.Spec.ContinueSiblings {
				return
			}
		}
	}
	visit(t.Root)
	return out
}

// MakeKey builds the group key for a node and label set.
func MakeKey(spec *model.RouteNode, labels map[string]string) model.GroupKey {
	return model.GroupKey{NodeID: spec.ID, LabelSet: append([]string(nil), spec.GroupBy...), Values: groupValues(spec, labels)}
}

func groupValues(spec *model.RouteNode, labels map[string]string) []string {
	vals := make([]string, len(spec.GroupBy))
	for i, name := range spec.GroupBy {
		vals[i] = labels[name]
	}
	return vals
}

// Path renders the node's lineage for display.
func (n *Node) Path() string {
	var parts []string
	for x := n; x != nil; x = x.Parent {
		parts = append(parts, x.Spec.Name)
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, " / ")
}

// DescribeMatch renders a node's match conditions for display.
func DescribeMatch(ms model.MatchAll) string {
	if len(ms) == 0 {
		return "全部"
	}
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, fmt.Sprintf("%s%s%q", m.Name, m.Op, m.Value))
	}
	sort.Strings(parts)
	return strings.Join(parts, " 且 ")
}

// Describe is an alias for DescribeMatch.
func Describe(ms model.MatchAll) string { return DescribeMatch(ms) }
