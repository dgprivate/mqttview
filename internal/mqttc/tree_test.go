package mqttc

import (
	"testing"
	"time"
)

func graphOf(t *testing.T, topics map[string]int, depth, nodes int) []GraphNode {
	t.Helper()

	tree := NewTree()
	base := time.Now()
	for topic, count := range topics {
		for i := range count {
			tree.Record(Message{
				Topic:      topic,
				Payload:    []byte("x"),
				ReceivedAt: base.Add(time.Duration(i) * time.Millisecond),
			})
		}
	}
	got, _, _ := tree.Graph(depth, nodes)
	return got
}

func findNode(nodes []GraphNode, topic string) (GraphNode, bool) {
	for _, n := range nodes {
		if n.Topic == topic {
			return n, true
		}
	}
	return GraphNode{}, false
}

// A parent is worth drawing because of what is below it, not because of what
// it holds itself.
func TestAGraphNodeCountsEverythingBelowIt(t *testing.T) {
	nodes := graphOf(t, map[string]int{
		"home/kitchen/temperature": 10,
		"home/kitchen/humidity":    5,
		"home/hall/motion":         2,
	}, 3, 100)

	home, ok := findNode(nodes, "home")
	if !ok {
		t.Fatal("the top of the namespace is missing from the graph")
	}
	if home.Messages != 17 {
		t.Errorf("home carries %d messages, want 17", home.Messages)
	}
	if home.Topics != 3 {
		t.Errorf("home has %d topics below it, want 3", home.Topics)
	}

	kitchen, _ := findNode(nodes, "home/kitchen")
	if kitchen.Messages != 15 {
		t.Errorf("home/kitchen carries %d messages, want 15", kitchen.Messages)
	}
}

func TestTheGraphStopsAtTheRequestedDepth(t *testing.T) {
	nodes := graphOf(t, map[string]int{"a/b/c/d/e": 1}, 2, 100)

	for _, n := range nodes {
		if n.Depth > 2 {
			t.Errorf("node %q is at depth %d, past the limit of 2", n.Topic, n.Depth)
		}
	}
	if _, ok := findNode(nodes, "a/b/c"); ok {
		t.Error("a node past the depth limit was returned")
	}
}

// A node at the depth limit that still has children is a branch, and the
// picture should not draw it as a leaf.
func TestANodeCutOffAtTheDepthLimitSaysItHasMore(t *testing.T) {
	nodes := graphOf(t, map[string]int{"a/b/c": 1}, 2, 100)

	b, ok := findNode(nodes, "a/b")
	if !ok {
		t.Fatal("a/b is missing")
	}
	if !b.Truncated {
		t.Error("a/b has a child past the limit but is not marked truncated")
	}

	leaf := graphOf(t, map[string]int{"a/b": 1}, 2, 100)
	if n, _ := findNode(leaf, "a/b"); n.Truncated {
		t.Error("a genuine leaf was marked truncated")
	}
}

// A graph of a namespace is drawn to answer "what is loud here". Dropping the
// loud branch to fit a quiet one answers the opposite question.
func TestWhenTheBudgetRunsOutTheBusiestBranchesAreKept(t *testing.T) {
	nodes := graphOf(t, map[string]int{
		"quiet/one": 1,
		"quiet/two": 1,
		"loud/one":  100,
	}, 1, 1)

	if len(nodes) != 1 {
		t.Fatalf("got %d nodes against a budget of 1", len(nodes))
	}
	if nodes[0].Topic != "loud" {
		t.Errorf("kept %q, want the busiest branch", nodes[0].Topic)
	}
}

// The top of the namespace has to be there; a deep node is not worth dropping
// it for.
func TestAShallowNodeIsNeverDroppedForADeepOne(t *testing.T) {
	nodes := graphOf(t, map[string]int{
		"a/b/c/d": 1,
		"z":       1,
	}, 4, 2)

	for _, n := range nodes {
		if n.Depth > 2 {
			t.Errorf("node %q at depth %d was kept while shallower ones were dropped", n.Topic, n.Depth)
		}
	}
}

func TestAGraphNodeCarriesTheMostRecentUpdateBelowIt(t *testing.T) {
	tree := NewTree()
	base := time.Now().Truncate(time.Second)

	tree.Record(Message{Topic: "a/old", Payload: []byte("x"), ReceivedAt: base})
	tree.Record(Message{Topic: "a/new", Payload: []byte("x"), ReceivedAt: base.Add(time.Hour)})

	nodes, newest, _ := tree.Graph(1, 100)
	a, ok := findNode(nodes, "a")
	if !ok {
		t.Fatal("a is missing")
	}
	if !a.UpdatedAt.Equal(base.Add(time.Hour)) {
		t.Errorf("a was last updated %v, want the newer of its children %v", a.UpdatedAt, base.Add(time.Hour))
	}
	if !newest.Equal(base.Add(time.Hour)) {
		t.Errorf("the tree's newest is %v, want %v", newest, base.Add(time.Hour))
	}
}

func TestAnEmptyTreeGraphsToNothingRatherThanFailing(t *testing.T) {
	nodes, newest, truncated := NewTree().Graph(3, 100)
	if len(nodes) != 0 {
		t.Errorf("got %d nodes from an empty tree", len(nodes))
	}
	if !newest.IsZero() || truncated {
		t.Errorf("newest = %v, truncated = %v; want the zero values", newest, truncated)
	}
}
