// Package treesitter parses source files with a pure-Go tree-sitter runtime
// (github.com/odvcencio/gotreesitter), so release builds stay CGO_ENABLED=0.
//
// Grammars load lazily on the first file of each language and are kept.
// Every parse is bounded by a deadline and a memory budget: the runtime has
// grammar-ambiguity cliffs where an ordinary 20 KB file can take seconds
// (Swift, C#; see docs/chronos-indexer.md, "M6 parser runtime spike"). A
// parse stopped by the deadline or a limit returns ErrIncomplete and no tree
// (those trees are empty), and the caller indexes the file without symbols.
//
// The runtime can also give up when error recovery fails (no GLR stack
// left), which the C runtime never does. That tree is kept but marked
// Partial: it covers a prefix of the file, often with most declarations
// intact.
package treesitter

import (
	"errors"
	"fmt"
	"sync"
	"time"

	gts "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

// Defaults for Options.
const (
	DefaultTimeout      = time.Second
	DefaultMemoryBudget = 256 << 20
)

// ErrUnsupported reports a grammar that is not in this build.
var ErrUnsupported = errors.New("tree-sitter grammar not available")

// ErrIncomplete reports a parse that stopped before the end of the input
// (deadline, memory budget or a parser safety limit).
var ErrIncomplete = errors.New("tree-sitter parse incomplete")

// Options bounds each parse.
type Options struct {
	Timeout      time.Duration // per parse; 0 = DefaultTimeout
	MemoryBudget int64         // bytes per parse; 0 = DefaultMemoryBudget
}

// Runtime parses files for any number of grammars. It is safe for
// concurrent use.
type Runtime struct {
	opts  Options
	mu    sync.Mutex
	langs map[string]*grammar
}

type grammar struct {
	once    sync.Once
	lang    *gts.Language
	factory func([]byte, *gts.Language) gts.TokenSource
	pool    *gts.ParserPool
	err     error
}

// New returns a Runtime. No grammar is loaded until it is first used.
func New(opts Options) *Runtime {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MemoryBudget <= 0 {
		opts.MemoryBudget = DefaultMemoryBudget
	}
	return &Runtime{opts: opts, langs: map[string]*grammar{}}
}

func (r *Runtime) grammar(name string) *grammar {
	r.mu.Lock()
	g, ok := r.langs[name]
	if !ok {
		g = &grammar{}
		r.langs[name] = g
	}
	r.mu.Unlock()
	g.once.Do(func() {
		entry := grammars.DetectLanguageByName(name)
		if entry == nil || entry.Language == nil {
			g.err = fmt.Errorf("%w: %s", ErrUnsupported, name)
			return
		}
		lang := entry.Language()
		if lang == nil {
			g.err = fmt.Errorf("%w: %s (not embedded in this build)", ErrUnsupported, name)
			return
		}
		g.lang, g.factory = lang, entry.TokenSourceFactory
		g.pool = gts.NewParserPool(lang,
			gts.WithParserPoolTimeoutMicros(uint64(r.opts.Timeout.Microseconds())),
			gts.WithParserPoolMemoryBudgetBytes(r.opts.MemoryBudget))
	})
	return g
}

// Language returns the loaded grammar, for compiling queries.
func (r *Runtime) Language(name string) (*gts.Language, error) {
	g := r.grammar(name)
	return g.lang, g.err
}

// Tree is a syntax tree over Source. Call Release when done.
type Tree struct {
	tree   *gts.Tree
	Lang   *gts.Language
	Source []byte
	// Partial is set when the parser stopped after failed error recovery;
	// the tree then ends at byte Covered, and nodes near the end are
	// usually ERROR nodes. Covered is len(Source) for a full parse.
	Partial bool
	Covered int
}

// Root returns the root node.
func (t *Tree) Root() *gts.Node { return t.tree.RootNode() }

// Release returns the tree's memory to the runtime. The tree and its nodes
// must not be used afterwards.
func (t *Tree) Release() {
	if t.tree != nil {
		t.tree.Release()
		t.tree = nil
	}
}

// Parse parses src with the named grammar. A tree may contain syntax error
// nodes and may be Partial; ErrIncomplete means the deadline or a limit
// stopped the parser and no tree is returned.
func (r *Runtime) Parse(name string, src []byte) (*Tree, error) {
	g := r.grammar(name)
	if g.err != nil {
		return nil, g.err
	}
	var (
		tree *gts.Tree
		err  error
	)
	if g.factory != nil {
		tree, err = g.pool.ParseWithTokenSource(src, g.factory(src, g.lang))
	} else {
		tree, err = g.pool.Parse(src)
	}
	if err != nil {
		if tree != nil {
			tree.Release()
		}
		return nil, fmt.Errorf("parse %s: %w", name, err)
	}
	if tree == nil {
		return nil, fmt.Errorf("parse %s: no tree", name)
	}
	out := &Tree{tree: tree, Lang: g.lang, Source: src, Covered: len(src)}
	switch reason := tree.ParseStopReason(); reason {
	case gts.ParseStopNone, gts.ParseStopAccepted, "":
	case gts.ParseStopNoStacksAlive:
		out.Partial = true
		out.Covered = min(int(tree.RootNode().EndByte()), len(src))
	default:
		tree.Release()
		return nil, fmt.Errorf("%w: %s stopped at %s", ErrIncomplete, name, reason)
	}
	return out, nil
}
