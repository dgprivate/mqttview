package mqttc

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// maxStoredPayload caps how much of a payload the tree keeps for the
	// "last known value" view. Larger payloads are truncated rather than
	// dropped so the UI can still show a preview.
	maxStoredPayload = 256 * 1024
	// maxTopics bounds tree memory. Subscribing to '#' on a busy broker can
	// otherwise grow without limit.
	maxTopics = 200_000
)

// TopicValue is the last known state of a single topic.
type TopicValue struct {
	Topic     string    `json:"topic"`
	Payload   []byte    `json:"payload"`
	Truncated bool      `json:"truncated,omitempty"`
	Size      int       `json:"size"`
	QoS       byte      `json:"qos"`
	Retain    bool      `json:"retain"`
	UpdatedAt time.Time `json:"updatedAt"`
	Count     uint64    `json:"count"`
}

// TreeNode is one level of the topic tree as sent to the UI. Only immediate
// children are returned per request, so the browser never has to hold the
// whole tree.
type TreeNode struct {
	// Name is the single topic level, e.g. "temperature".
	Name string `json:"name"`
	// Topic is the full path to this node, e.g. "home/kitchen/temperature".
	Topic string `json:"topic"`
	// ChildCount is the number of immediate children.
	ChildCount int `json:"childCount"`
	// TopicCount is the number of value-bearing topics at or below this node.
	TopicCount int `json:"topicCount"`
	// Messages is how many messages have arrived at or below this node.
	Messages uint64 `json:"messages"`
	// Value is present when this node is itself a topic that has received a
	// message.
	Value *TopicValue `json:"value,omitempty"`
}

type treeNode struct {
	name     string
	topic    string
	children map[string]*treeNode

	hasValue  bool
	payload   []byte
	truncated bool
	size      int
	qos       byte
	retain    bool
	updatedAt time.Time
	count     uint64

	// subMessages counts messages at or below this node.
	subMessages uint64
	// subUpdatedAt is the most recent message at or below this node. Kept on
	// the write path with the counters rather than computed on read: the graph
	// asks for it once per node, and walking each node's subtree to find it
	// turns drawing a large namespace into quadratic work.
	subUpdatedAt time.Time
	// subTopics counts value-bearing topics at or below this node.
	subTopics int
}

// Tree is a concurrent topic hierarchy with last-known values.
type Tree struct {
	mu       sync.RWMutex
	root     *treeNode
	topics   int
	messages uint64
	// full is set once maxTopics is reached; new topics are then ignored and
	// the UI shows a warning rather than the server running out of memory.
	full bool
}

// NewTree returns an empty topic tree.
func NewTree() *Tree {
	return &Tree{root: &treeNode{children: map[string]*treeNode{}}}
}

// Record folds a message into the tree.
func (t *Tree) Record(m Message) {
	levels := strings.Split(m.Topic, "/")

	t.mu.Lock()
	defer t.mu.Unlock()

	// Walk once to find or create the leaf, tracking the path so the
	// aggregate counters can be updated without a second traversal.
	path := make([]*treeNode, 0, len(levels)+1)
	cur := t.root
	path = append(path, cur)
	for i, level := range levels {
		child, ok := cur.children[level]
		if !ok {
			if t.full {
				return
			}
			child = &treeNode{
				name:     level,
				topic:    strings.Join(levels[:i+1], "/"),
				children: map[string]*treeNode{},
			}
			cur.children[level] = child
		}
		cur = child
		path = append(path, cur)
	}

	if !cur.hasValue {
		cur.hasValue = true
		t.topics++
		if t.topics >= maxTopics {
			t.full = true
		}
		for _, n := range path {
			n.subTopics++
		}
	}

	cur.size = len(m.Payload)
	if len(m.Payload) > maxStoredPayload {
		cur.payload = append(cur.payload[:0], m.Payload[:maxStoredPayload]...)
		cur.truncated = true
	} else {
		cur.payload = append(cur.payload[:0], m.Payload...)
		cur.truncated = false
	}
	cur.qos = m.QoS
	cur.retain = m.Retain
	cur.updatedAt = m.ReceivedAt
	cur.count++

	t.messages++
	for _, n := range path {
		n.subMessages++
		if m.ReceivedAt.After(n.subUpdatedAt) {
			n.subUpdatedAt = m.ReceivedAt
		}
	}
}

// Children returns the immediate children of prefix, sorted by name. An empty
// prefix returns the roots. The bool reports whether the prefix exists.
func (t *Tree) Children(prefix string) ([]TreeNode, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	node := t.find(prefix)
	if node == nil {
		return nil, false
	}
	out := make([]TreeNode, 0, len(node.children))
	for _, child := range node.children {
		out = append(out, child.export(false))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, true
}

// Node returns a single node including its value.
func (t *Tree) Node(topic string) (TreeNode, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	node := t.find(topic)
	if node == nil {
		return TreeNode{}, false
	}
	return node.export(true), true
}

// Value returns the last known value for an exact topic.
func (t *Tree) Value(topic string) (TopicValue, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	node := t.find(topic)
	if node == nil || !node.hasValue {
		return TopicValue{}, false
	}
	return node.value(), true
}

// Match returns the last known values of every topic matching an MQTT filter,
// newest first, capped at limit.
func (t *Tree) Match(filter string, limit int) []TopicValue {
	if limit <= 0 {
		limit = 500
	}
	t.mu.RLock()
	defer t.mu.RUnlock()

	var out []TopicValue
	t.walk(t.root, func(n *treeNode) {
		if n.hasValue && MatchFilter(filter, n.topic) {
			out = append(out, n.value())
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Search returns topics containing the given substring (case-insensitive).
func (t *Tree) Search(query string, limit int) []TopicValue {
	if limit <= 0 {
		limit = 200
	}
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	var out []TopicValue
	t.walk(t.root, func(n *treeNode) {
		if n.hasValue && strings.Contains(strings.ToLower(n.topic), needle) {
			out = append(out, n.value())
		}
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Topic < out[j].Topic })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Stats reports tree-wide counters.
func (t *Tree) Stats() (topics int, messages uint64, full bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.topics, t.messages, t.full
}

// Clear empties the tree, e.g. when the user resets a connection's view.
func (t *Tree) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.root = &treeNode{children: map[string]*treeNode{}}
	t.topics = 0
	t.messages = 0
	t.full = false
}

func (t *Tree) find(topic string) *treeNode {
	if topic == "" {
		return t.root
	}
	cur := t.root
	for _, level := range strings.Split(topic, "/") {
		child, ok := cur.children[level]
		if !ok {
			return nil
		}
		cur = child
	}
	return cur
}

func (t *Tree) walk(n *treeNode, fn func(*treeNode)) {
	fn(n)
	for _, child := range n.children {
		t.walk(child, fn)
	}
}

func (n *treeNode) export(withValue bool) TreeNode {
	out := TreeNode{
		Name:       n.name,
		Topic:      n.topic,
		ChildCount: len(n.children),
		TopicCount: n.subTopics,
		Messages:   n.subMessages,
	}
	if n.hasValue && withValue {
		v := n.value()
		out.Value = &v
	} else if n.hasValue {
		// List views need to know a node holds a value and when it changed,
		// but shipping every payload would bloat the response.
		out.Value = &TopicValue{
			Topic:     n.topic,
			Size:      n.size,
			QoS:       n.qos,
			Retain:    n.retain,
			UpdatedAt: n.updatedAt,
			Count:     n.count,
			Payload:   previewPayload(n.payload),
			Truncated: n.truncated || len(n.payload) > payloadPreviewBytes,
		}
	}
	return out
}

const payloadPreviewBytes = 512

func previewPayload(p []byte) []byte {
	if len(p) <= payloadPreviewBytes {
		return append([]byte(nil), p...)
	}
	return append([]byte(nil), p[:payloadPreviewBytes]...)
}

func (n *treeNode) value() TopicValue {
	return TopicValue{
		Topic:     n.topic,
		Payload:   append([]byte(nil), n.payload...),
		Truncated: n.truncated,
		Size:      n.size,
		QoS:       n.qos,
		Retain:    n.retain,
		UpdatedAt: n.updatedAt,
		Count:     n.count,
	}
}

// GraphNode is one node of the namespace, aggregated for a picture of it.
//
// Messages and Topics are counts at or below the node, which is what makes a
// parent worth drawing at all: "home" carrying forty thousand messages is the
// fact, and its individual leaves are not.
type GraphNode struct {
	Name     string `json:"name"`
	Topic    string `json:"topic"`
	Depth    int    `json:"depth"`
	Messages uint64 `json:"messages"`
	Topics   int    `json:"topics"`
	Children int    `json:"children"`
	// UpdatedAt is the most recent message at or below the node, so recency
	// can colour it. Zero when nothing below it has ever carried a value.
	UpdatedAt time.Time `json:"updatedAt,omitzero"`
	// Truncated says this node has children that were not returned, so the
	// picture can say so rather than implying a leaf.
	Truncated bool `json:"truncated,omitempty"`
}

// Graph returns the namespace down to maxDepth, at most maxNodes nodes.
//
// The busiest branches are kept when the budget runs out, because a graph of
// a broker's namespace is drawn to answer "what is loud here", and dropping
// the loud branch to make room for a quiet one answers the opposite question.
func (t *Tree) Graph(maxDepth, maxNodes int) ([]GraphNode, time.Time, bool) {
	if maxDepth <= 0 {
		maxDepth = 3
	}
	if maxNodes <= 0 {
		maxNodes = 500
	}

	t.mu.RLock()
	defer t.mu.RUnlock()

	var out []GraphNode
	truncated := false

	// Breadth-first, so a shallow node is never dropped to make room for a
	// deep one: the top of the namespace is the part that has to be there.
	level := []*treeNode{t.root}
	for depth := 1; depth <= maxDepth && len(level) > 0; depth++ {
		var next []*treeNode
		for _, parent := range level {
			children := make([]*treeNode, 0, len(parent.children))
			for _, child := range parent.children {
				children = append(children, child)
			}
			// Loudest first, so the budget is spent on what the picture is for.
			sort.Slice(children, func(i, j int) bool {
				return children[i].subMessages > children[j].subMessages
			})

			for _, child := range children {
				if len(out) >= maxNodes {
					truncated = true
					break
				}
				node := GraphNode{
					Name:      child.name,
					Topic:     child.topic,
					Depth:     depth,
					Messages:  child.subMessages,
					Topics:    child.subTopics,
					Children:  len(child.children),
					UpdatedAt: child.subUpdatedAt,
				}
				// A node at the depth limit that still has children is a
				// branch, not a leaf, and the picture should not pretend.
				if depth == maxDepth && len(child.children) > 0 {
					node.Truncated = true
				}
				out = append(out, node)
				if depth < maxDepth {
					next = append(next, child)
				}
			}
			if len(out) >= maxNodes {
				truncated = true
				break
			}
		}
		level = next
	}

	return out, t.newest(), truncated
}

// newest is the most recent update anywhere. Caller holds the lock.
func (t *Tree) newest() time.Time { return t.root.subUpdatedAt }
